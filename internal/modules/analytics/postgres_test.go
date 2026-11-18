package analytics

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

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
	// The fixture asserts exact totals over the whole tables.
	for _, table := range []string{"analytics_events", "analytics_late_events", "analytics_listing_daily", "analytics_rollup_runs"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func TestPostgresStore(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	store := NewPostgresStore(pool)
	r := newRig(t, store)
	count := func(q string, args ...any) int {
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The FR-035 fixture gives the same figures as on the memory store.
	f := runFixture(t, r)
	checkFixture(t, r, f, func() int { return count(`SELECT count(*) FROM analytics_events`) })

	// Only the truncated network is stored.
	var network string
	if err := pool.QueryRow(ctx, `SELECT DISTINCT network::text FROM analytics_events WHERE network IS NOT NULL`).Scan(&network); err != nil || network != "203.0.113.0/24" {
		t.Fatalf("network %q %v", network, err)
	}
	// The database refuses a full address, whatever the code does.
	at := time.Now().UTC()
	if _, err := store.Insert(ctx, Event{ID: uuid.New(), Type: TypeDetailView, OccurredAt: at, ReceivedAt: at, LocalDay: "2026-11-18",
		ListingID: f.zw, MarketCode: "ZW", Network: "203.0.113.77/32", Origin: OriginBrowser}, SPAWindow); err == nil {
		t.Fatal("a /32 was stored")
	}
	// IPv6 /48.
	r.now = at
	l := r.listing("GB", true)
	if _, err := r.svc.Ingest(ctx, Client{UserAgent: humanUA, IP: "2001:db8:abcd:12::1"}, 1, []json.RawMessage{ev(r.eventID(), TypeContactClick, at, l, nil)}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT network::text FROM analytics_events WHERE listing_id = $1`, l).Scan(&network); err != nil || network != "2001:db8:abcd::/48" {
		t.Fatalf("ipv6 network %q %v", network, err)
	}

	// Retention: the raw events go, the rollups stay.
	rollups := count(`SELECT count(*) FROM analytics_listing_daily`)
	r.now = f.rollupAt.AddDate(0, 0, 91)
	events, late, err := r.svc.PurgeRaw(ctx)
	if err != nil || events != 91 || late != 1 {
		t.Fatalf("purge: %d %d %v", events, late, err)
	}
	if n := count(`SELECT count(*) FROM analytics_listing_daily`); n != rollups {
		t.Fatalf("rollups after purge: %d, want %d", n, rollups)
	}
}
