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

// videoStoreContract checks that a store keeps the video of a listing and of
// its revision. seller is a user who may own a listing.
func videoStoreContract(t *testing.T, store ManageStore, seller uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	start := 75
	id := uuid.New()
	ref := "RP-ZW-" + id.String()[:8]
	m := ManagedListing{Record: Record{
		ID: id, Slug: slugFor("Video tour contract", ref), Reference: ref, Title: "Video tour contract", MarketCode: "ZW",
		Price: Money{Currency: "USD"}, Status: StatusDraft, UpdatedAt: now, SellerUserID: &seller, Features: []string{},
		YouTubeVideoID: "aqz-KE-bpKQ", YouTubeStartSeconds: &start,
	}, CreatedAt: now}
	if err := store.CreateListing(ctx, m, Change{}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Managed(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.YouTubeVideoID != "aqz-KE-bpKQ" || got.YouTubeStartSeconds == nil || *got.YouTubeStartSeconds != 75 {
		t.Fatalf("created: %q %v", got.YouTubeVideoID, got.YouTubeStartSeconds)
	}

	// A revision carries its own video; the listing keeps the reviewed one.
	later := now.Add(time.Second)
	c := ContentOf(got.Record)
	c.YouTubeVideoID, c.YouTubeStartSeconds = "jNQXAC9IVRw", nil
	got.Revision = &Revision{Status: RevisionEditing, Content: &c, UpdatedAt: later}
	expect := got.UpdatedAt
	got.UpdatedAt = later
	if _, err := store.SaveListing(ctx, got, expect, Change{}); err != nil {
		t.Fatal(err)
	}
	got, err = store.Managed(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.YouTubeVideoID != "aqz-KE-bpKQ" || got.Revision == nil || got.Revision.Content == nil ||
		got.Revision.Content.YouTubeVideoID != "jNQXAC9IVRw" || got.Revision.Content.YouTubeStartSeconds != nil {
		t.Fatalf("revision: %q / %+v", got.YouTubeVideoID, got.Revision)
	}

	// Clearing removes both values.
	got.Revision.Content.ApplyTo(&got.Record)
	got.Revision = nil
	got.YouTubeVideoID, got.YouTubeStartSeconds = "", nil
	expect, got.UpdatedAt = got.UpdatedAt, later.Add(time.Second)
	if _, err := store.SaveListing(ctx, got, expect, Change{}); err != nil {
		t.Fatal(err)
	}
	if got, err = store.Managed(ctx, id); err != nil || got.YouTubeVideoID != "" || got.YouTubeStartSeconds != nil {
		t.Fatalf("cleared: %q %v %v", got.YouTubeVideoID, got.YouTubeStartSeconds, err)
	}
}

func TestMemoryStoreKeepsTheVideo(t *testing.T) {
	videoStoreContract(t, NewMemoryStore(noLabels{}), uuid.New())
}

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
func TestPostgresStoreKeepsTheVideo(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, created_at, updated_at) VALUES ($1, $2, 'Video Contract', 'ACTIVE', now(), now())`,
		seller, "video-"+seller.String()[:12]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	store := NewPostgresStore(pool)
	videoStoreContract(t, store, seller)

	// The CHECK constraints of migration 0027 hold whatever the API does.
	for name, sql := range map[string]string{
		"a 10-character id":         `UPDATE listings SET youtube_video_id = 'aqz-KE-bpK' WHERE seller_user_id = $1`,
		"markup":                    `UPDATE listings SET youtube_video_id = '<iframe x>' WHERE seller_user_id = $1`,
		"a negative start":          `UPDATE listings SET youtube_video_id = 'aqz-KE-bpKQ', youtube_start_seconds = -1 WHERE seller_user_id = $1`,
		"a start without the video": `UPDATE listings SET youtube_video_id = NULL, youtube_start_seconds = 5 WHERE seller_user_id = $1`,
	} {
		var pgErr *pgconn.PgError
		if _, err := pool.Exec(ctx, sql, seller); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s: %v, want a check violation", name, err)
		}
	}
}
