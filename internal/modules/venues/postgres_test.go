package venues

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
func openTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	return pool
}

// pgFixture inserts the rows the fixture's IDs need (organisation, two
// listings, two customers) and returns the fixture on the Postgres store.
func pgFixture(t *testing.T, pool *pgxpool.Pool) *fixture {
	t.Helper()
	ctx := context.Background()
	org, showroom, house, alice, bob := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	owner, agent, staff := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'HOST', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
		org, "Venues Test "+org.String()[:8])
	for _, u := range []uuid.UUID{alice, bob, owner, agent, staff} {
		exec(`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Venue Tester', 'ACTIVE', now())`,
			u, "venues."+u.String()[:8]+"@example.test")
	}
	for _, l := range []uuid.UUID{showroom, house} {
		exec(`INSERT INTO listings (id, market_id, title, status, price_amount, currency_code, organisation_id)
			VALUES ($1, (SELECT id FROM markets WHERE code = 'ZW'), 'Venue contract', 'DRAFT', 100, 'USD', $2)`, l, org)
	}
	return newFixture(t, NewPostgresStore(pool), org, showroom, house, alice, bob, owner, agent, staff)
}

// FR-024 acceptance against Postgres: two goroutines confirm overlapping
// requests at once; the per-listing lock row serialises them and exactly
// one succeeds.
func TestPostgresConcurrentConfirm(t *testing.T) {
	pool := openTestDatabase(t)
	for range 5 {
		f := pgFixture(t, pool)
		concurrentConfirm(t, f)
		var held int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM venue_bookings WHERE listing_id = $1 AND status = 'CONFIRMED'`,
			f.showroom).Scan(&held); err != nil || held != 1 {
			t.Fatalf("%d confirmed rows, %v", held, err)
		}
		var audits int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM organisation_audit_events WHERE organisation_id = $1 AND action = 'VENUE_BOOKING_CONFIRMED'`,
			f.org).Scan(&audits); err != nil || audits != 1 {
			t.Fatalf("%d audit rows, %v", audits, err)
		}
	}
}

// The Postgres store keeps the same rules as the memory store end to end.
func TestPostgresTransitions(t *testing.T) {
	pool := openTestDatabase(t)
	f := pgFixture(t, pool)
	ctx := context.Background()
	b := f.request(t, f.alice, 10, 2)
	got, err := f.store.Booking(ctx, b.ID)
	if err != nil || got.Pricing.TotalExcludingDeposit != b.Pricing.TotalExcludingDeposit || got.Reference != b.Reference {
		t.Fatalf("read back %+v %v", got, err)
	}
	s, ok, err := f.store.Settings(ctx, f.showroom)
	if err != nil || !ok || s.RatePerHour != "50.00" || len(s.Fees) != 1 || s.Fees[0].Amount != "15" || len(s.OpeningHours) != 6 || *s.Deposit != "100.00" {
		t.Fatalf("settings %+v %v", s, err)
	}
	if _, err := f.svc.Confirm(ctx, f.owner, Host{OrganisationID: &f.org}, b.ID); err != nil {
		t.Fatal(err)
	}
	list, total, err := f.svc.MineList(ctx, f.alice, Filter{Page: 1, PageSize: 10, Statuses: []string{StatusConfirmed}, FromDay: "2026-11-09", ToDay: "2026-11-09"})
	if err != nil || total != 1 || len(list) != 1 {
		t.Fatalf("list %d %v", total, err)
	}
	occ, err := f.store.Occupied(ctx, f.showroom, b.Start, b.End, nil)
	if err != nil || len(occ) != 1 {
		t.Fatalf("occupied %d %v", len(occ), err)
	}
	if c, err := f.svc.CustomerCancel(ctx, f.alice, b.ID, ""); err != nil || c.Status != StatusCancelled {
		t.Fatalf("cancel %v", err)
	}
	if ev, _ := f.store.Events(ctx, b.ID); len(ev) != 3 {
		t.Fatalf("events %+v", ev)
	}
}
