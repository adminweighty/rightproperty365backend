package listings

import (
	"context"
	"errors"
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

type placementRig interface {
	Store
	ManageStore
	PlacementStore
}

// placementContract checks placements on a store. seller may own listings;
// admin is an existing account; audits and updates count this test's audit
// events and LISTING_UPDATED events of a listing.
func placementContract(t *testing.T, store placementRig, seller, admin uuid.UUID, audits func() int, updates func(id uuid.UUID) int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	day := 24 * time.Hour
	clock := now
	publish := func(title string, published bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		ref := "RP-ZW-" + id.String()[:8]
		m := ManagedListing{Record: Record{ID: id, Slug: slugFor(title, ref), Reference: ref, Title: title, MarketCode: "ZW", Price: Money{Currency: "USD"},
			Status: StatusDraft, UpdatedAt: now.Add(-time.Hour), SellerUserID: &seller, Features: []string{}}, CreatedAt: now.Add(-time.Hour)}
		if err := store.CreateListing(ctx, m, Change{}); err != nil {
			t.Fatal(err)
		}
		if published {
			expect := m.UpdatedAt
			m.Status, m.PublishedAt, m.ExpiresAt, m.UpdatedAt = StatusPublished, now.Add(-time.Hour), now.Add(60*day), now.Add(-time.Minute)
			if _, err := store.SaveListing(ctx, m, expect, Change{NewCycle: true}); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	record := func(id uuid.UUID) Record {
		t.Helper()
		rs, err := store.ByIDs(ctx, []uuid.UUID{id})
		if err != nil || len(rs) != 1 {
			t.Fatalf("record: %v %v", rs, err)
		}
		return rs[0]
	}
	root := &auth.Principal{UserID: admin, Name: "Placement Admin", SessionScope: auth.ScopeAdmin,
		PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformFinanceAdmin}}}
	elsewhere := &auth.Principal{UserID: admin, Name: "Market Admin", SessionScope: auth.ScopeAdmin,
		PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformMarketAdmin, MarketCode: "ZA"}}}
	limit := 1
	quota := func(_ context.Context, _ uuid.UUID, _ Record) (*int, string, error) { return &limit, "", nil }
	svc := NewPlacements(store, store, quota, func() time.Time { return clock })
	ends := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	startAudits := audits()

	one, two, draft := publish("Placement one", true), publish("Placement two", true), publish("Placement draft", false)

	// Featured now: shown at once, with one index event.
	featured, err := svc.Grant(ctx, root, one, PlacementInput{Kind: PlacementFeatured, EndsAt: ends(10 * day), Note: "Paid by invoice"})
	if err != nil {
		t.Fatal(err)
	}
	if r := record(one); !r.Featured || r.PlacementKind != PlacementFeatured || updates(one) != 1 {
		t.Fatalf("granted: %v %q %d", r.Featured, r.PlacementKind, updates(one))
	}
	if _, err := svc.Grant(ctx, root, one, PlacementInput{Kind: PlacementSponsored, EndsAt: ends(3 * day)}); !errors.Is(err, ErrPlacementOverlap) {
		t.Fatalf("overlap: %v", err)
	}
	if _, err := svc.Grant(ctx, root, draft, PlacementInput{Kind: PlacementFeatured, EndsAt: ends(day)}); !errors.Is(err, ErrPlacementNotPublished) {
		t.Fatalf("draft: %v", err)
	}
	if _, err := svc.Grant(ctx, elsewhere, one, PlacementInput{Kind: PlacementFeatured, EndsAt: ends(day)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another market: %v", err)
	}
	var verr *ValidationError
	if _, err := svc.Grant(ctx, root, two, PlacementInput{Kind: "GOLD", EndsAt: ends(400 * day)}); !errors.As(err, &verr) ||
		verr.Fields["kind"] == "" || verr.Fields["ends_at"] == "" {
		t.Fatalf("validation: %v", err)
	}

	// Sponsored from tomorrow: nothing yet, shown once the day comes.
	start := now.Add(day).Format(time.RFC3339)
	sponsored, err := svc.Grant(ctx, root, two, PlacementInput{Kind: PlacementSponsored, StartsAt: &start, EndsAt: ends(5 * day)})
	if err != nil {
		t.Fatal(err)
	}
	if r := record(two); r.Featured || updates(two) != 0 {
		t.Fatalf("scheduled: %v %d", r.Featured, updates(two))
	}
	clock = now.Add(day + time.Minute)
	if _, err := svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r := record(two); !r.Featured || r.PlacementKind != PlacementSponsored || updates(two) != 1 {
		t.Fatalf("started: %v %q %d", r.Featured, r.PlacementKind, updates(two))
	}
	if _, err := svc.Sync(ctx); err != nil || updates(two) != 1 {
		t.Fatalf("a second sync changes nothing: %d %v", updates(two), err)
	}

	// The list, scoped.
	list, total, err := svc.List(ctx, root, PlacementFilter{ListingID: &two, Status: PlacementActive, Page: 1, PageSize: 10})
	if err != nil || total != 1 || list[0].ID != sponsored.ID || list[0].ListingReference == "" || list[0].MarketCode != "ZW" {
		t.Fatalf("list: %+v %d %v", list, total, err)
	}
	if list, _, _ := svc.List(ctx, elsewhere, PlacementFilter{ListingID: &two, Page: 1, PageSize: 10}); len(list) != 0 {
		t.Fatalf("another market's list: %+v", list)
	}

	// Ended early: hidden at once; not twice.
	if _, err := svc.End(ctx, root, featured.ID, ""); !errors.As(err, &verr) {
		t.Fatalf("a reason is needed: %v", err)
	}
	ended, err := svc.End(ctx, root, featured.ID, "Campaign cancelled")
	if err != nil || ended.Status(clock) != PlacementEnded {
		t.Fatalf("end: %+v %v", ended, err)
	}
	if r := record(one); r.Featured || r.PlacementKind != "" || updates(one) != 2 {
		t.Fatalf("ended: %v %q %d", r.Featured, r.PlacementKind, updates(one))
	}
	if _, err := svc.End(ctx, root, featured.ID, "again"); !errors.Is(err, ErrPlacementEnded) {
		t.Fatalf("ended twice: %v", err)
	}

	// The period over: the sync hides it.
	clock = now.Add(6 * day)
	if _, err := svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if r := record(two); r.Featured || r.PlacementKind != "" {
		t.Fatalf("period over: %v %q", r.Featured, r.PlacementKind)
	}

	// Tied to a subscription allowing one: a second overlapping one is refused.
	clock = now
	sub := uuid.NewString()
	three, four := publish("Placement three", true), publish("Placement four", true)
	if _, err := svc.Grant(ctx, root, three, PlacementInput{Kind: PlacementFeatured, EndsAt: ends(day), SubscriptionID: &sub}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Grant(ctx, root, four, PlacementInput{Kind: PlacementFeatured, EndsAt: ends(day), SubscriptionID: &sub}); !errors.Is(err, ErrPlacementLimit) {
		t.Fatalf("limit: %v", err)
	}
	if got := audits() - startAudits; got != 4 {
		t.Fatalf("%d audit events, want 4 (3 grants, 1 end)", got)
	}
}

func TestMemoryPlacements(t *testing.T) {
	store := NewMemoryStore(noLabels{})
	log := &adminaudit.MemoryLog{}
	store.SetPlacementAudit(log)
	placementContract(t, store, uuid.New(), uuid.New(), func() int { return len(log.All()) }, func(id uuid.UUID) int {
		n := 0
		for _, ev := range store.Outbox() {
			if ev.AggregateID == id && ev.Type == EventListingUpdated {
				n++
			}
		}
		return n
	})
}

func TestPostgresPlacements(t *testing.T) {
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
	seller, admin := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{seller, admin} {
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, created_at, updated_at) VALUES ($1, $2, 'Placement Contract', 'ACTIVE', now(), now())`,
			u, "placement-"+u.String()[:12]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	placementContract(t, NewPostgresStore(pool), seller, admin, func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_events WHERE actor_user_id = $1`, admin).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}, func(id uuid.UUID) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, id, EventListingUpdated).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
}
