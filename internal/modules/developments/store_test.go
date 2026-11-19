package developments

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// fixture names the rows a store test needs: an organisation with a public
// developer profile, a user, and two listings of the organisation.
type fixture struct {
	org, profile, user uuid.UUID
	listing1, listing2 uuid.UUID
	// hide makes the profile not public; show makes it public again.
	hide, show func()
}

func sampleProject(f fixture, slug string, now time.Time) Development {
	lat, lon := -17.75, 31.09
	phase, unit := uuid.New(), uuid.New()
	three := 3
	user := f.user
	return Development{
		ID: uuid.New(), OrganisationID: f.org, ProfileID: f.profile, MarketCode: "ZW", Slug: slug, Name: "Contract " + slug,
		Summary: "Summary", Description: "Description", Status: StatusSelling, Moderation: ModerationDraft, Completion: "2027-Q3",
		CityCode: "harare", SuburbCode: "borrowdale", Latitude: &lat, Longitude: &lon, LocationPrecision: listings.PrecisionApproximate,
		Amenities: []string{"clubhouse"}, Highlights: []Highlight{{Label: "Yield", Value: "8%"}}, YouTubeVideoID: "aqz-KE-bpKQ",
		CreatedBy: &user, CreatedAt: now, UpdatedAt: now,
		Phases: []Phase{{ID: phase, Name: "Phase 1", Status: StatusSelling, Completion: "2026-12-31", CreatedAt: now, UnitTypes: []UnitType{
			{ID: unit, PhaseID: phase, Name: "Three-bed", Bedrooms: &three, PriceFrom: "100000.00", PriceTo: "120000.50", Currency: "USD", CreatedAt: now},
		}}},
		Photos: []listings.StoredPhoto{
			{ID: uuid.New(), Position: 0, URL: "/assets/images/a.jpg", Alt: "A", Width: 1200, Height: 800, CreatedAt: now},
			{ID: uuid.New(), Position: 1, Checksum: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Width: 2048, Height: 1365,
				Variants:   []listings.StoredVariant{{Size: listings.PhotoSizeLarge, Key: "developmentphotos/x", Width: 2048, Height: 1365, Bytes: 10}},
				UploadedBy: &user, CreatedAt: now},
		},
		Links: []Link{{ListingID: f.listing1, UnitTypeID: &unit, CreatedAt: now}},
	}
}

// storeContract checks a Store against the rules every adapter keeps.
func storeContract(t *testing.T, store Store, f fixture, audit func() []adminaudit.Event) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	slug := "contract-" + uuid.NewString()[:8]
	d := sampleProject(f, slug, now)
	if err := store.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	unit := d.Phases[0].UnitTypes[0]
	if got.Slug != slug || got.Completion != "2027-Q3" || got.Latitude == nil || *got.Latitude != -17.75 || len(got.Phases) != 1 ||
		len(got.Phases[0].UnitTypes) != 1 || got.Phases[0].UnitTypes[0].PriceTo != "120000.50" || len(got.Photos) != 2 ||
		len(got.Links) != 1 || got.Links[0].UnitTypeID == nil || *got.Links[0].UnitTypeID != unit.ID || got.Highlights[0].Value != "8%" {
		t.Fatalf("created: %+v", got)
	}

	// The slug is unique per market; the same slug in another market is fine.
	dup := sampleProject(f, slug, now)
	dup.Links = nil
	if err := store.Create(ctx, dup); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("duplicate slug: %v", err)
	}
	dup.MarketCode, dup.CityCode, dup.SuburbCode = "GB", "london", ""
	if err := store.Create(ctx, dup); err != nil {
		t.Fatalf("other market: %v", err)
	}
	// A listing is in one project only.
	taken := sampleProject(f, slug+"-b", now)
	if err := store.Create(ctx, taken); !errors.Is(err, ErrListingLinked) {
		t.Fatalf("listing twice: %v", err)
	}

	// Not public as a draft.
	if _, err := store.PublicBySlug(ctx, "ZW", slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("draft is public: %v", err)
	}

	// Save: a unit type removed, a photo removed (returned), a phase added,
	// published with an audit record.
	later := now.Add(time.Second)
	got.Phases = append(got.Phases, Phase{ID: uuid.New(), Name: "Phase 2", Status: StatusPlanned, Position: 1, CreatedAt: later, UnitTypes: []UnitType{
		{ID: uuid.New(), Name: "Penthouse", PriceOnRequest: true, CreatedAt: later},
	}})
	got.Phases[1].UnitTypes[0].PhaseID = got.Phases[1].ID
	got.Phases[0].UnitTypes = nil
	got.Links[0].UnitTypeID = nil
	got.Photos = got.Photos[:1]
	got.Photos[0].ApprovedAt = &later
	got.Moderation, got.PublishedAt, got.UpdatedAt = ModerationPublished, &later, later
	ev := adminaudit.New(f.user, "Contract", ActionApproved, TargetDevelopment, got.ID.String(), got.Name, "", nil, nil, later)
	removed, err := store.Save(ctx, got, now, &ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || len(removed[0].Keys()) != 1 {
		t.Fatalf("removed photos: %+v", removed)
	}
	if _, err := store.Save(ctx, got, now, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	if audit != nil {
		found := false
		for _, e := range audit() {
			found = found || (e.TargetID == got.ID.String() && e.Action == ActionApproved)
		}
		if !found {
			t.Fatal("the audit record is written with the decision")
		}
	}
	pub, err := store.PublicBySlug(ctx, "ZW", slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.Phases) != 2 || len(pub.Phases[0].UnitTypes) != 0 || !pub.Phases[1].UnitTypes[0].PriceOnRequest || len(pub.Photos) != 1 ||
		pub.Photos[0].ApprovedAt == nil || pub.Links[0].UnitTypeID != nil {
		t.Fatalf("published: %+v", pub)
	}
	page, total, err := store.Public(ctx, PublicFilter{Market: "ZW", Q: slug, Limit: 10})
	if err != nil || total != 1 || len(page) != 1 || page[0].ID != got.ID {
		t.Fatalf("public list: %d %v %v", total, page, err)
	}
	if _, total, _ := store.Public(ctx, PublicFilter{Market: "ZW", Q: slug, Status: StatusPlanned}); total != 0 {
		t.Fatal("the status filter applies")
	}
	if list, err := store.PublicByProfile(ctx, f.profile); err != nil || len(list) == 0 {
		t.Fatalf("by profile: %v %v", list, err)
	}
	entries, err := store.PublicEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		found = found || (e.Market == "ZW" && e.Slug == slug)
	}
	if !found {
		t.Fatal("the sitemap names the project")
	}
	devID, photo, err := store.PhotoByID(ctx, got.Photos[0].ID)
	if err != nil || devID != got.ID || photo.URL != "/assets/images/a.jpg" {
		t.Fatalf("photo: %v %+v %v", devID, photo, err)
	}

	// A profile that is no longer public hides the project.
	f.hide()
	if _, err := store.PublicBySlug(ctx, "ZW", slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hidden profile: %v", err)
	}
	f.show()

	// The review queue.
	got, _ = store.Get(ctx, got.ID)
	expect := got.UpdatedAt
	got.Moderation, got.SubmittedAt, got.UpdatedAt = ModerationPending, &later, later.Add(time.Second)
	if _, err := store.Save(ctx, got, expect, nil); err != nil {
		t.Fatal(err)
	}
	queue, total, err := store.Queue(ctx, QueueFilter{Markets: []string{"ZW"}, Limit: 50})
	if err != nil || total == 0 || !containsID(queue, got.ID) {
		t.Fatalf("queue: %v %v", total, err)
	}
	if other, _, _ := store.Queue(ctx, QueueFilter{Markets: []string{"GB"}, Limit: 50}); containsID(other, got.ID) {
		t.Fatal("the queue is scoped to the markets")
	}
	if list, err := store.ByOrganisation(ctx, f.org); err != nil || !containsID(list, got.ID) {
		t.Fatalf("by organisation: %v", err)
	}

	// Delete.
	if _, err := store.Delete(ctx, dup.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, dup.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}

func containsID(list []Development, id uuid.UUID) bool {
	for _, d := range list {
		if d.ID == id {
			return true
		}
	}
	return false
}

func TestMemoryStoreContract(t *testing.T) {
	store := NewMemoryStore()
	visible := true
	store.SetVisibility(func(context.Context, Development) bool { return visible })
	log := &adminaudit.MemoryLog{}
	store.SetAuditLog(log)
	f := fixture{org: uuid.New(), profile: uuid.New(), user: uuid.New(), listing1: uuid.New(), listing2: uuid.New(),
		hide: func() { visible = false }, show: func() { visible = true }}
	storeContract(t, store, f, log.All)

	// Account deletion unlinks the person.
	store.ForgetUser(f.user)
	for _, d := range store.All() {
		if d.CreatedBy != nil && *d.CreatedBy == f.user {
			t.Fatal("still linked")
		}
		for _, ph := range d.Photos {
			if ph.UploadedBy != nil && *ph.UploadedBy == f.user {
				t.Fatal("photo still linked")
			}
		}
	}
}

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
func TestPostgresStoreContract(t *testing.T) {
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

	// Every row the foreign keys reference: the user, the organisation, the
	// developer profile and the two listings (markets come with 0001/0014).
	f := fixture{org: uuid.New(), profile: uuid.New(), user: uuid.New(), listing1: uuid.New(), listing2: uuid.New()}
	short := f.org.String()[:8]
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, full_name, status, created_at, updated_at) VALUES ($1, $2, 'Dev Contract', 'ACTIVE', now(), now())`,
		f.user, "dev-contract-"+short+"@example.test")
	exec(`INSERT INTO organisations (id, market_id, name, kind, status, created_at, updated_at)
		VALUES ($1, (SELECT id FROM markets WHERE code = 'ZW'), 'Dev Contract '||$2, 'DEVELOPER', 'ACTIVE', now(), now())`, f.org, short)
	exec(`INSERT INTO directory_profiles (id, market_id, kind, slug, name, organisation_id, status, verified, created_at, updated_at)
		VALUES ($1, (SELECT id FROM markets WHERE code = 'ZW'), 'developers', 'dev-contract-'||$2, 'Dev Contract', $3, 'ACTIVE', true, now(), now())`,
		f.profile, short, f.org)
	for _, l := range []uuid.UUID{f.listing1, f.listing2} {
		exec(`INSERT INTO listings (id, market_id, title, status, currency_code, organisation_id, created_at, updated_at)
			VALUES ($1, (SELECT id FROM markets WHERE code = 'ZW'), 'Contract listing', 'DRAFT', 'USD', $2, now(), now())`, l, f.org)
	}
	f.hide = func() { exec(`UPDATE directory_profiles SET status = 'SUSPENDED' WHERE id = $1`, f.profile) }
	f.show = func() { exec(`UPDATE directory_profiles SET status = 'ACTIVE' WHERE id = $1`, f.profile) }

	store := NewPostgresStore(pool)
	storeContract(t, store, f, func() []adminaudit.Event {
		events, _, err := adminaudit.List(ctx, pool, adminaudit.Filter{TargetType: TargetDevelopment})
		if err != nil {
			t.Fatal(err)
		}
		return events
	})

	// The CHECK constraints of migration 0046 hold whatever the API does.
	for name, sql := range map[string]string{
		"a price and on request": `UPDATE development_unit_types SET price_from = 1, currency_code = 'USD' WHERE development_id IN (SELECT id FROM developments WHERE organisation_id = $1) AND price_on_request`,
		"an unknown status":      `UPDATE developments SET status = 'BUILDING' WHERE organisation_id = $1`,
		"a malformed completion": `UPDATE developments SET completion_estimate = 'soon' WHERE organisation_id = $1`,
		"one coordinate":         `UPDATE developments SET latitude = NULL WHERE organisation_id = $1 AND longitude IS NOT NULL`,
	} {
		var pgErr *pgconn.PgError
		if _, err := pool.Exec(ctx, sql, f.org); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s: %v, want a check violation", name, err)
		}
	}

	// Account deletion (the users row is anonymised): the person is unlinked.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ForgetUserTx(ctx, tx, f.user); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var linked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM developments WHERE created_by = $1`, f.user).Scan(&linked); err != nil || linked != 0 {
		t.Fatalf("still linked: %d %v", linked, err)
	}
	// An enquiry quoting a unit type keeps its names when the project goes.
	var unit uuid.UUID
	var devID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT u.id, u.development_id FROM development_unit_types u JOIN developments d ON d.id = u.development_id
		WHERE d.organisation_id = $1 LIMIT 1`, f.org).Scan(&unit, &devID); err != nil {
		t.Fatal(err)
	}
	enquiry := uuid.New()
	exec(`INSERT INTO business_enquiries (id, reference, market_id, profile_id, status, message, preferred_contact_method, idempotency_key,
		request_hash, guest_name, guest_email, consent_privacy_version, consent_accepted_at, recipient_organisation_id, created_at, updated_at,
		development_id, development_unit_type_id, development_name, development_market, development_slug, development_unit_type_name)
		VALUES ($1, 'BE-'||$2, (SELECT id FROM markets WHERE code = 'ZW'), $3, 'RECEIVED', 'Hello', 'EMAIL', 'dev-contract-'||$2, 'x',
		'Guest', 'guest@example.test', '2026-01', now(), $4, now(), now(), $5, $6, 'Contract', 'ZW', 'contract', 'Penthouse')`,
		enquiry, short, f.profile, f.org, devID, unit)
	// The projects go: the enquiry keeps the names it quoted (ON DELETE SET NULL).
	exec(`DELETE FROM developments WHERE organisation_id = $1`, f.org)
	var name string
	var still *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT development_name, development_id FROM business_enquiries WHERE id = $1`, enquiry).Scan(&name, &still); err != nil ||
		name != "Contract" || still != nil {
		t.Fatalf("enquiry after the project went: %q %v %v", name, still, err)
	}
}
