package propertyrequests

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
func openFunnelDatabase(t *testing.T) *pgxpool.Pool {
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

// TestPostgresFunnelMatchesMemory runs the funnel fixture through both
// stores and requires the same reports for every scope and filter.
func TestPostgresFunnelMatchesMemory(t *testing.T) {
	pool := openFunnelDatabase(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	p := newFunnelPeople()
	if err := pool.QueryRow(ctx, `SELECT id FROM markets WHERE code = 'ZW'`).Scan(&p.market); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO organisations (id, market_id, name, kind, status) VALUES ($1, $2, $3, 'AGENCY', 'ACTIVE')`,
		p.org, p.market, "Funnel Test "+p.org.String()[:8])
	for _, u := range []uuid.UUID{p.owner, p.agentA, p.agentB, p.seller} {
		exec(`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Funnel Tester', 'ACTIVE', now())`,
			u, "funnel."+u.String()[:8]+"@example.test")
	}
	for _, l := range []uuid.UUID{p.listingA, p.listingB} {
		exec(`INSERT INTO listings (id, market_id, title, status, price_amount, currency_code, organisation_id)
			VALUES ($1, $2, 'Funnel listing', 'PUBLISHED', 100, 'USD', $3)`, l, p.market, p.org)
	}
	exec(`INSERT INTO listings (id, market_id, title, status, price_amount, currency_code, seller_user_id)
		VALUES ($1, $2, 'Funnel seller listing', 'PUBLISHED', 100, 'USD', $3)`, p.sellerListing, p.market, p.seller)

	pg := NewPostgresStore(pool)
	// Twice: the seed replaces its own rows.
	for range 2 {
		if err := pg.SeedRequests(ctx, funnelFixture(p)); err != nil {
			t.Fatal(err)
		}
	}
	var rows, timeline int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM property_requests WHERE listing_id IN ($1, $2, $3)`,
		p.listingA, p.listingB, p.sellerListing).Scan(&rows); err != nil || rows != 11 {
		t.Fatalf("%d seeded requests after two runs, %v", rows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM request_timeline t JOIN property_requests pr ON pr.id = t.request_id
		WHERE pr.listing_id IN ($1, $2, $3)`, p.listingA, p.listingB, p.sellerListing).Scan(&timeline); err != nil {
		t.Fatal(err)
	}
	mem := seededMemory(t, p)
	if want := len(mem.Timeline()); timeline != want {
		t.Fatalf("%d timeline rows, the memory store has %d", timeline, want)
	}

	manager := InboxScope{UserID: p.owner, ManagedOrganisationIDs: []uuid.UUID{p.org}}
	agentA := InboxScope{UserID: p.agentA, MemberOrganisationIDs: []uuid.UUID{p.org}}
	seller := InboxScope{UserID: p.seller}
	type named struct {
		name string
		f    FunnelFilter
	}
	withListing := funnelWindowFilter(manager, &p.org)
	withListing.ListingID = &p.listingB
	withAgent := funnelWindowFilter(manager, &p.org)
	withAgent.AgentUserID = &p.agentA
	withType := funnelWindowFilter(manager, &p.org)
	withType.RequestType, withType.MarketIDs = TypeViewingRequest, []uuid.UUID{p.market}
	noMarket := funnelWindowFilter(manager, &p.org)
	noMarket.MarketIDs = []uuid.UUID{}
	wide := funnelWindowFilter(manager, &p.org)
	wide.From = wide.From.AddDate(0, 0, -10)
	wide.To = wide.To.AddDate(0, 0, 10)
	for _, c := range []named{
		{"manager", funnelWindowFilter(manager, &p.org)},
		{"agent", funnelWindowFilter(agentA, &p.org)},
		{"seller", funnelWindowFilter(seller, nil)},
		{"listing", withListing},
		{"agent filter", withAgent},
		{"type and market", withType},
		{"no market", noMarket},
		{"wide window", wide},
	} {
		want, got := reportOf(t, mem, c.f), reportOf(t, pg, c.f)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("%s: postgres %+v\nmemory %+v", c.name, got, want)
		}
		if c.name == "manager" {
			checkManagerReport(t, got)
		}
	}
}
