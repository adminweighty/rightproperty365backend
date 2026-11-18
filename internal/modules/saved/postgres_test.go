package saved

import (
	"context"
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
	return pool
}

// storeContract runs the same checks on both stores. user creates an
// account; listing a listing (any status); events counts the outbox events
// of a type about a listing.
func storeContract(t *testing.T, s Store, user func() uuid.UUID, listing func() uuid.UUID, events func(eventType string, listingID uuid.UUID) int) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	alice, bob := user(), user()
	a, b := listing(), listing()
	slug := "contract"
	snap := Snapshot{Slug: &slug, Reference: "RP-T-1", Title: "Before"}

	got, created, err := s.Save(ctx, Saved{UserID: alice, ListingID: a, SavedAt: now, Snapshot: snap, SnapshotAt: now})
	if err != nil || !created || !got.SavedAt.Equal(now) || got.Snapshot.Title != "Before" {
		t.Fatalf("save: %+v %v %v", got, created, err)
	}
	later := now.Add(time.Hour)
	got, created, err = s.Save(ctx, Saved{UserID: alice, ListingID: a, SavedAt: later, Snapshot: snap, SnapshotAt: later})
	if err != nil || created || !got.SavedAt.Equal(now) {
		t.Fatalf("save again: %+v %v %v", got, created, err)
	}
	if n := events(EventListingSaved, a); n != 1 {
		t.Fatalf("%d LISTING_SAVED events, want 1", n)
	}
	if _, _, err := s.Save(ctx, Saved{UserID: alice, ListingID: b, SavedAt: later, Snapshot: snap, SnapshotAt: later}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Save(ctx, Saved{UserID: bob, ListingID: a, SavedAt: later, Snapshot: snap, SnapshotAt: later}); err != nil {
		t.Fatal(err)
	}

	rows, total, err := s.List(ctx, alice, 10, 0)
	if err != nil || total != 2 || len(rows) != 2 || rows[0].ListingID != b || rows[1].ListingID != a {
		t.Fatalf("list: %+v %d %v", rows, total, err)
	}
	if rows, total, _ := s.List(ctx, alice, 1, 1); total != 2 || len(rows) != 1 || rows[0].ListingID != a {
		t.Fatalf("second page: %+v %d", rows, total)
	}
	ids, err := s.ListingIDs(ctx, alice)
	if err != nil || len(ids) != 2 || ids[0] != b {
		t.Fatalf("ids: %v %v", ids, err)
	}

	snap.Title = "After"
	if err := s.UpdateSnapshot(ctx, alice, a, snap, later); err != nil {
		t.Fatal(err)
	}
	rows, _, _ = s.List(ctx, alice, 10, 0)
	if rows[1].Snapshot.Title != "After" || !rows[1].SnapshotAt.Equal(later) || rows[1].Snapshot.Slug == nil || *rows[1].Snapshot.Slug != slug {
		t.Fatalf("refreshed snapshot: %+v", rows[1])
	}
	if rows, _, _ := s.List(ctx, bob, 10, 0); len(rows) != 1 || rows[0].Snapshot.Title != "Before" {
		t.Fatalf("bob's snapshot changed with alice's: %+v", rows)
	}

	removed, err := s.Remove(ctx, alice, a, later)
	if err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if removed, err := s.Remove(ctx, alice, a, later); err != nil || removed {
		t.Fatalf("remove again: %v %v", removed, err)
	}
	if n := events(EventListingUnsaved, a); n != 1 {
		t.Fatalf("%d LISTING_UNSAVED events, want 1", n)
	}
	if rows, total, _ := s.List(ctx, bob, 10, 0); total != 1 || rows[0].ListingID != a {
		t.Fatalf("bob's save went with alice's removal: %+v", rows)
	}
}

func TestMemoryStore(t *testing.T) {
	s := NewMemoryStore()
	storeContract(t, s, uuid.New, uuid.New, func(eventType string, listingID uuid.UUID) int {
		n := 0
		for _, ev := range s.Outbox() {
			if ev.Type == eventType && ev.AggregateID == listingID {
				n++
			}
		}
		return n
	})
}

func TestPostgresStore(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	user := func() uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, created_at, updated_at) VALUES ($1, $2, 'Saved Contract', 'ACTIVE', now(), now())`,
			id, "saved-"+id.String()[:12]+"@example.test"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	seller := user()
	listing := func() uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO listings (id, market_id, title, status, price_amount, currency_code, seller_user_id)
			VALUES ($1, (SELECT id FROM markets WHERE code = 'ZW'), 'Saved contract', 'DRAFT', 100, 'USD', $2)`, id, seller); err != nil {
			t.Fatal(err)
		}
		return id
	}
	store := NewPostgresStore(pool)
	storeContract(t, store, user, listing, func(eventType string, listingID uuid.UUID) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type = $1 AND aggregate_type = 'listing' AND aggregate_id = $2`,
			eventType, listingID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})

	// The users row's cascade (a hard delete) takes the saves with it.
	alice, a := user(), listing()
	now := time.Now().UTC()
	if _, _, err := store.Save(ctx, Saved{UserID: alice, ListingID: a, SavedAt: now, Snapshot: Snapshot{Title: "x"}, SnapshotAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, alice); err != nil {
		t.Fatal(err)
	}
	if ids, _ := store.ListingIDs(ctx, alice); len(ids) != 0 {
		t.Fatalf("saves left after the user was deleted: %v", ids)
	}
}
