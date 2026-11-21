package dataexport

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/internal/platform/storage"
	"rightproperty365.com/backend/migrations"
)

type mapStorage struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (m *mapStorage) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = b
	return err
}
func (m *mapStorage) Open(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *mapStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}
func (m *mapStorage) Name() string { return "map" }

type fixedCollector struct{ fail int }

func (c *fixedCollector) Collect(_ context.Context, userID uuid.UUID) ([]Section, error) {
	if c.fail > 0 {
		c.fail--
		return nil, errors.New("database away")
	}
	return []Section{{Name: "account", Rows: []map[string]string{{"id": userID.String(), "email": "me@example.test"}}}}, nil
}

// contract runs the export flow on a store; ready counts EventReady.
func contract(t *testing.T, store Store, user uuid.UUID, ready func(id uuid.UUID) int) {
	t.Helper()
	ctx := context.Background()
	clock := time.Now().UTC().Truncate(time.Second)
	files := &mapStorage{objs: map[string][]byte{}}
	col := &fixedCollector{fail: 1}
	svc := NewService(store, files, col, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return clock })

	if e, err := svc.Latest(ctx, user); err != nil || e != nil {
		t.Fatalf("nothing yet: %v %v", e, err)
	}
	e, err := svc.Request(ctx, user)
	if err != nil || e.Status != StatusRequested {
		t.Fatalf("request: %+v %v", e, err)
	}
	if again, err := svc.Request(ctx, user); !errors.Is(err, ErrRecent) || again.ID != e.ID {
		t.Fatalf("again: %v", err)
	}
	if _, _, err := svc.Open(ctx, user); !errors.Is(err, ErrNotReady) {
		t.Fatalf("not ready: %v", err)
	}
	// The first build fails and is retried; the second works.
	if n, _ := svc.Sweep(ctx); n != 0 {
		t.Fatalf("first sweep built %d", n)
	}
	if n, err := svc.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("second sweep: %d %v", n, err)
	}
	if ready(e.ID) != 1 {
		t.Fatal("no ready event")
	}
	got, _ := svc.Latest(ctx, user)
	if got.Status != StatusReady || got.ExpiresAt == nil || !got.ExpiresAt.Equal(clock.Add(Lifetime)) || got.SizeBytes == 0 || got.Attempts != 2 {
		t.Fatalf("ready: %+v", got)
	}
	_, rc, err := svc.Open(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || len(zr.File) != 2 || zr.File[0].Name != "README.txt" || zr.File[1].Name != "account.json" {
		t.Fatalf("archive: %v", err)
	}
	if got, _ := svc.Latest(ctx, user); got.Downloads != 1 {
		t.Fatalf("downloads: %d", got.Downloads)
	}

	// Seven days on: the object is deleted, the export EXPIRED.
	clock = clock.Add(Lifetime + time.Minute)
	if _, err := svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Latest(ctx, user); got.Status != StatusExpired || len(files.objs) != 0 {
		t.Fatalf("expired: %+v %d", got, len(files.objs))
	}
	// A day after the first request, a new one is possible.
	if _, err := svc.Request(ctx, user); err != nil {
		t.Fatalf("new request: %v", err)
	}
}

func TestMemoryExport(t *testing.T) {
	s := NewMemoryStore()
	contract(t, s, uuid.New(), func(id uuid.UUID) int {
		n := 0
		for _, ev := range s.Outbox() {
			if ev.AggregateID == id && ev.Type == EventReady {
				n++
			}
		}
		return n
	})
}

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

func TestPostgresExport(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	user := uuid.New()
	email := "export-" + user.String()[:12] + "@example.test"
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, created_at, updated_at, password_hash) VALUES ($1, $2, 'Export Contract', 'ACTIVE', now(), now(), 'secret-hash')`,
		user, email); err != nil {
		t.Fatal(err)
	}
	// Every section's query runs on the real schema; secrets stay out.
	sections, err := NewPostgresCollector(pool).Collect(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != len(sectionNames()) {
		t.Fatalf("%d sections", len(sections))
	}
	account, _ := json.Marshal(sections[0].Rows)
	if !strings.Contains(string(account), email) || strings.Contains(string(account), "password_hash") || strings.Contains(string(account), "secret-hash") {
		t.Fatalf("account: %s", account)
	}
	other := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, created_at, updated_at) VALUES ($1, $2, 'Export Contract 2', 'ACTIVE', now(), now())`,
		other, "export-"+other.String()[:12]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	// Earlier runs' requests on this database must not be claimed by this one.
	if _, err := pool.Exec(ctx, `UPDATE data_exports SET status = 'FAILED', lease_until = NULL WHERE status IN ('REQUESTED', 'BUILDING')`); err != nil {
		t.Fatal(err)
	}
	contract(t, NewPostgresStore(pool), other, func(id uuid.UUID) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, id, EventReady).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
}

func sectionNames() []string {
	out := []string{}
	for _, s := range sections {
		out = append(out, s.name)
	}
	return out
}
