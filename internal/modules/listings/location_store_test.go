package listings

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

	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// locationStoreContract checks that a store keeps the coordinates of a
// listing and of its revision, exactly (six decimals). seller is a user who
// may own a listing.
func locationStoreContract(t *testing.T, store ManageStore, seller uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := uuid.New()
	ref := "RP-ZW-" + id.String()[:8]
	m := ManagedListing{Record: Record{
		ID: id, Slug: slugFor("Location contract", ref), Reference: ref, Title: "Location contract", MarketCode: "ZW",
		Price: Money{Currency: "USD"}, Status: StatusDraft, UpdatedAt: now, SellerUserID: &seller, Features: []string{},
		Latitude: floatp(-17.783456), Longitude: floatp(31.067891), LocationPrecision: PrecisionApproximate,
	}, CreatedAt: now}
	if err := store.CreateListing(ctx, m, Change{}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Managed(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Latitude == nil || *got.Latitude != -17.783456 || got.Longitude == nil || *got.Longitude != 31.067891 ||
		got.LocationPrecision != PrecisionApproximate {
		t.Fatalf("created: %v %v %q", got.Latitude, got.Longitude, got.LocationPrecision)
	}

	// A revision carries its own location; the listing keeps the reviewed one.
	later := now.Add(time.Second)
	c := ContentOf(got.Record)
	c.Latitude, c.Longitude, c.LocationPrecision = floatp(51.507351), floatp(-0.127758), PrecisionExact
	got.Revision = &Revision{Status: RevisionEditing, Content: &c, UpdatedAt: later}
	expect := got.UpdatedAt
	got.UpdatedAt = later
	if _, err := store.SaveListing(ctx, got, expect, Change{}); err != nil {
		t.Fatal(err)
	}
	if got, err = store.Managed(ctx, id); err != nil {
		t.Fatal(err)
	}
	rc := got.Revision.Content
	if *got.Latitude != -17.783456 || rc == nil || rc.Latitude == nil || *rc.Latitude != 51.507351 || *rc.Longitude != -0.127758 ||
		rc.LocationPrecision != PrecisionExact {
		t.Fatalf("revision: %v / %+v", *got.Latitude, rc)
	}

	// Applying it, then clearing the coordinates.
	rc.ApplyTo(&got.Record)
	got.Revision = nil
	expect, got.UpdatedAt = got.UpdatedAt, later.Add(time.Second)
	if _, err := store.SaveListing(ctx, got, expect, Change{}); err != nil {
		t.Fatal(err)
	}
	if got, err = store.Managed(ctx, id); err != nil || *got.Latitude != 51.507351 || *got.Longitude != -0.127758 || got.LocationPrecision != PrecisionExact {
		t.Fatalf("applied: %v %v %q %v", got.Latitude, got.Longitude, got.LocationPrecision, err)
	}
	got.Latitude, got.Longitude = nil, nil
	expect, got.UpdatedAt = got.UpdatedAt, later.Add(2*time.Second)
	if _, err := store.SaveListing(ctx, got, expect, Change{}); err != nil {
		t.Fatal(err)
	}
	if got, err = store.Managed(ctx, id); err != nil || got.Latitude != nil || got.Longitude != nil {
		t.Fatalf("cleared: %v %v %v", got.Latitude, got.Longitude, err)
	}
}

func TestMemoryStoreKeepsTheLocation(t *testing.T) {
	locationStoreContract(t, NewMemoryStore(noLabels{}), uuid.New())
}

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
func TestPostgresStoreKeepsTheLocation(t *testing.T) {
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

	seller := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, created_at, updated_at) VALUES ($1, $2, 'Location Contract', 'ACTIVE', now(), now())`,
		seller, "location-"+seller.String()[:12]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(pool)
	locationStoreContract(t, store, seller)

	// The CHECK constraints of migration 0028 hold whatever the API does.
	for name, sql := range map[string]string{
		"latitude out of range":  `UPDATE listings SET latitude = 91, longitude = 0 WHERE seller_user_id = $1`,
		"longitude out of range": `UPDATE listings SET latitude = 0, longitude = -181 WHERE seller_user_id = $1`,
		"latitude only":          `UPDATE listings SET latitude = 1, longitude = NULL WHERE seller_user_id = $1`,
		"longitude only":         `UPDATE listings SET latitude = NULL, longitude = 1 WHERE seller_user_id = $1`,
		"unknown precision":      `UPDATE listings SET location_precision = 'CITY' WHERE seller_user_id = $1`,
	} {
		var pgErr *pgconn.PgError
		if _, err := pool.Exec(ctx, sql, seller); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s: %v, want a check violation", name, err)
		}
	}

	// The database search adapter: the bbox, has_location and
	// mappable_total SQL runs and agrees with itself (the public rule needs
	// a verified advertiser this test does not create, so it checks the
	// query, not which listings are public).
	now := time.Now().UTC()
	// Whether the seller counts as verified depends on roles this test does
	// not create; the SQL must run and agree with itself either way.
	for _, q := range []Query{
		{BBox: &BBox{West: -179.5, South: -89.8, East: -179, North: -89.70}},
		{BBox: &BBox{West: -179.5, South: -89.699, East: -179, North: -89.69}},
		{HasLocation: true},
		{},
	} {
		q.Sort, q.Page, q.PageSize = SortNewest, 1, 12
		hits, err := store.Search(ctx, q, now)
		if err != nil {
			t.Fatalf("search %+v: %v", q, err)
		}
		if hits.Mappable > hits.Total || (q.BBox != nil && hits.Mappable != hits.Total) {
			t.Fatalf("search %+v: total %d, mappable %d", q, hits.Total, hits.Mappable)
		}
	}
}
