package listings

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

type reportRig interface {
	Store
	ManageStore
	ReportStore
	FlagStore
}

// reportContract checks the report store, the moderator's take-down and
// warning, and the duplicate flags. seller and reporter are existing
// accounts; events counts a listing's outbox events of a type.
func reportContract(t *testing.T, store reportRig, seller, reporter, admin uuid.UUID, events func(id uuid.UUID, typ string) int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	two := 2
	publish := func(title, price string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		ref := "RP-ZW-" + id.String()[:8]
		m := ManagedListing{Record: Record{ID: id, Slug: slugFor(title, ref), Reference: ref, Title: title, MarketCode: "ZW",
			Price: Money{Amount: price, Currency: "USD"}, City: "harare-" + seller.String()[:8], Bedrooms: &two,
			Status: StatusDraft, UpdatedAt: now.Add(-time.Hour), SellerUserID: &seller, Features: []string{}}, CreatedAt: now.Add(-time.Hour)}
		if err := store.CreateListing(ctx, m, Change{}); err != nil {
			t.Fatal(err)
		}
		expect := m.UpdatedAt
		m.Status, m.PublishedAt, m.ExpiresAt, m.UpdatedAt = StatusPublished, now.Add(-time.Hour), now.Add(60*24*time.Hour), now.Add(-time.Minute)
		if _, err := store.SaveListing(ctx, m, expect, Change{NewCycle: true}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	title := "Report contract cottage " + seller.String()[:8]
	listing := publish(title, "500.00")

	// Reports: a signed-in reporter once while open; guests as often as they like.
	rep := ListingReport{ID: uuid.New(), ListingID: listing, Reason: ReportFraud, Note: "Asked for a deposit by WhatsApp", ReporterUserID: &reporter, Status: ReportOpen, CreatedAt: now}
	if created, err := store.CreateReport(ctx, rep); err != nil || !created {
		t.Fatalf("report: %v %v", created, err)
	}
	again := rep
	again.ID = uuid.New()
	if created, err := store.CreateReport(ctx, again); err != nil || created {
		t.Fatalf("a second open report of the same reporter: %v %v", created, err)
	}
	guest := ListingReport{ID: uuid.New(), ListingID: listing, Reason: ReportWrongPrice, Status: ReportOpen, CreatedAt: now.Add(time.Second)}
	for i := range 2 {
		g := guest
		g.ID = uuid.New()
		g.CreatedAt = g.CreatedAt.Add(time.Duration(i) * time.Second)
		if created, err := store.CreateReport(ctx, g); err != nil || !created {
			t.Fatalf("guest report: %v %v", created, err)
		}
	}
	got, err := store.GetReport(ctx, rep.ID)
	if err != nil || got.OpenReports != 3 || got.ListingReference == "" || got.MarketCode != "ZW" || got.ListingStatus != StatusPublished {
		t.Fatalf("get: %+v %v", got, err)
	}
	list, total, err := store.ListReports(ctx, ReportFilter{Status: ReportOpen, Markets: []string{"ZW"}, Page: 1, PageSize: 500})
	if err != nil || total < 3 || len(list) < 3 {
		t.Fatalf("list: %d %v", total, err)
	}
	if _, total, _ := store.ListReports(ctx, ReportFilter{Status: ReportOpen, Markets: []string{"QQ"}, Page: 1, PageSize: 5}); total != 0 {
		t.Fatalf("another market: %d", total)
	}

	// Flags: the same title elsewhere, and contact details in the text.
	twin := publish(title, "900.00")
	refs, err := store.SimilarListings(ctx, SimilarQuery{ExceptID: listing, MarketCode: "ZW", Title: normalTitle(title)})
	if err != nil || len(refs) != 1 {
		t.Fatalf("similar by title: %v %v", refs, err)
	}
	refs, err = store.SimilarListings(ctx, SimilarQuery{ExceptID: twin, MarketCode: "ZW", Title: "something else", PriceAmount: "500.00", Currency: "USD",
		City: "harare-" + seller.String()[:8], Bedrooms: &two})
	if err != nil || len(refs) != 1 {
		t.Fatalf("similar by price and place: %v %v", refs, err)
	}
	if n, err := store.CreatedSince(ctx, nil, &seller, now.Add(-2*time.Hour)); err != nil || n != 2 {
		t.Fatalf("created since: %d %v", n, err)
	}

	// The moderator: a warning keeps the listing; a take-down hides it.
	mg := NewManager(NewService(store, nil, noLabels{}), store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mod := &auth.Principal{UserID: admin, Name: "Moderator", SessionScope: auth.ScopeAdmin, PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformModerator}}}
	if err := mg.Warn(ctx, mod, listing, "Please correct the price."); err != nil {
		t.Fatal(err)
	}
	if events(listing, EventListingWarned) != 1 {
		t.Fatal("no warning event")
	}
	if err := mg.ModeratorUnpublish(ctx, mod, listing, ""); err == nil {
		t.Fatal("a reason is needed")
	}
	if err := mg.ModeratorUnpublish(ctx, mod, listing, "Reported as a scam: the advertiser asked for a deposit."); err != nil {
		t.Fatal(err)
	}
	m, err := store.Managed(ctx, listing)
	if err != nil || m.Status != StatusChangesRequested || m.ReviewReason == "" {
		t.Fatalf("taken down: %s %v", m.Status, err)
	}
	if events(listing, EventListingUnpublished) != 1 || events(listing, EventListingChangesRequested) != 1 {
		t.Fatal("take-down events")
	}
	if err := mg.ModeratorUnpublish(ctx, mod, listing, "again"); err == nil {
		t.Fatal("taken down twice")
	}

	// Deciding closes the report, and with allOpen the others of the listing.
	audit := adminaudit.New(admin, "Moderator", ActionUnpublishedOnReport, TargetListing, listing.String(), "x", "y", nil, nil, now)
	decided, err := store.DecideReports(ctx, rep.ID, true, ReportActioned, admin, "Moderator", "Taken down", audit, now)
	if err != nil || decided.Status != ReportActioned || decided.OpenReports != 0 || decided.DecidedByName != "Moderator" {
		t.Fatalf("decide: %+v %v", decided, err)
	}
	if _, err := store.DecideReports(ctx, rep.ID, false, ReportDismissed, admin, "Moderator", "x", audit, now); !errors.Is(err, ErrReportDecided) {
		t.Fatalf("decided twice: %v", err)
	}
	if created, err := store.CreateReport(ctx, ListingReport{ID: uuid.New(), ListingID: listing, Reason: ReportFraud, ReporterUserID: &reporter, Status: ReportOpen, CreatedAt: now}); err != nil || !created {
		t.Fatalf("a new report after the decision: %v %v", created, err)
	}
}

func TestTextFlags(t *testing.T) {
	cases := map[string][]string{
		"Lovely cottage":                              {},
		"CALL NOW FOR THE BEST HOUSE":                 {FlagShouting},
		"Call +263 77 123 4567 for a viewing":         {FlagContactInText},
		"Write to owner@example.com":                  {FlagContactInText},
		"See www.example.com":                         {FlagContactInText},
		"Three bedrooms, 2 bathrooms, built in 2019.": {},
	}
	for title, want := range cases {
		got := textFlags(Content{Title: title})
		if len(got) != len(want) {
			t.Errorf("%q: %+v, want %v", title, got, want)
			continue
		}
		for i := range want {
			if got[i].Code != want[i] {
				t.Errorf("%q: %+v, want %v", title, got, want)
			}
		}
	}
}

func TestMemoryReports(t *testing.T) {
	store := NewMemoryStore(noLabels{})
	reportContract(t, store, uuid.New(), uuid.New(), uuid.New(), func(id uuid.UUID, typ string) int {
		n := 0
		for _, ev := range store.Outbox() {
			if ev.AggregateID == id && ev.Type == typ {
				n++
			}
		}
		return n
	})
}

func TestPostgresReports(t *testing.T) {
	url := os.Getenv("RP365_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RP365_TEST_DATABASE_URL is not set: the Postgres store is only exercised against a throwaway database")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.NewRunner(conn, migs).Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	seller, reporter, admin := uuid.New(), uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{seller, reporter, admin} {
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, created_at, updated_at) VALUES ($1, $2, 'Report Contract', 'ACTIVE', now(), now())`,
			u, "report-"+u.String()[:12]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	reportContract(t, NewPostgresStore(pool), seller, reporter, admin, func(id uuid.UUID, typ string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, id, typ).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
}
