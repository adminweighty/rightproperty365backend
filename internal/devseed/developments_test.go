package devseed

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/developments"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/internal/platform/storage"
	"rightproperty365.com/backend/migrations"
)

func TestDevelopmentSamples(t *testing.T) {
	now := time.Now()
	list := DevelopmentRecords(now)
	if len(list) != 2 {
		t.Fatalf("%d samples", len(list))
	}
	published, draft := list[0], list[1]
	if published.Moderation != developments.ModerationPublished || len(published.Phases) != 2 || draft.Moderation != developments.ModerationDraft {
		t.Fatalf("samples: %+v / %+v", published.Moderation, draft.Moderation)
	}
	units, onRequest := 0, 0
	for _, p := range published.Phases {
		for _, u := range p.UnitTypes {
			units++
			if u.PriceOnRequest {
				onRequest++
			}
		}
	}
	if units != 3 || onRequest != 1 || len(published.Links) != 1 || published.Links[0].ListingID != DevelopmentListingID {
		t.Fatalf("units %d, on request %d, links %+v", units, onRequest, published.Links)
	}
	for _, d := range list {
		if _, ok := developments.ParseCompletion(d.Completion); !ok {
			t.Fatalf("%s: completion %q", d.Slug, d.Completion)
		}
		for _, text := range []string{d.Name, d.Summary, d.Description} {
			if developments.HasContactDetails(text) {
				t.Fatalf("%s: contact details in %q", d.Slug, text)
			}
		}
	}
	// The linked listing belongs to the developer's organisation and is public.
	l := DevelopmentListingRecord(now)
	if l.OrganisationID == nil || *l.OrganisationID != published.OrganisationID || !l.Public(now) {
		t.Fatalf("listing: %+v", l)
	}
	if err := listings.CheckRecord(l); err != nil {
		t.Fatal(err)
	}
}

// The development seed writes the samples and can be run again.
func TestSeedPostgresDevelopmentsIsRerunnable(t *testing.T) {
	url := os.Getenv("RP365_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RP365_TEST_DATABASE_URL is not set: the Postgres seed is only exercised against a throwaway database")
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
	files, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hasher := identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1}
	if err := SeedPostgres(ctx, pool, files, hasher, "Development-Seed-2026!", time.Now()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := SeedPostgresDevelopments(ctx, pool, time.Now()); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	store := developments.NewPostgresStore(pool)
	d, err := store.PublicBySlug(ctx, MarketCode, BorrowdaleHeightsSlug)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Phases) != 2 || len(d.Photos) != 3 || len(d.Links) != 1 {
		t.Fatalf("published sample: %d phases, %d photos, %d links", len(d.Phases), len(d.Photos), len(d.Links))
	}
	if _, err := store.PublicBySlug(ctx, MarketCode, RuwaGardensSlug); err == nil {
		t.Fatal("the draft is not public")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM developments WHERE id IN ($1, $2)`, BorrowdaleHeightsID, RuwaGardensID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("%d sample rows, %v", count, err)
	}
}
