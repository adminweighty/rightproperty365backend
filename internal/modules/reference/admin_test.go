package reference

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
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// adminStoreContract is what both stores must do for the administration.
func adminStoreContract(t *testing.T, s interface {
	Store
	AdminStore
}) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	code := "contract-" + uuid.NewString()[:8]
	audit := adminaudit.New(uuid.Nil, "Test", adminaudit.ActionReferenceCreated, adminaudit.TargetReferenceItem, SetListingFeatures+"/"+code, "x", "", nil, nil, now)
	if err := s.CreateItem(ctx, SetListingFeatures, Item{Code: code, Label: "Contract feature", SortOrder: 5, Active: true}, audit); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateItem(ctx, SetListingFeatures, Item{Code: code, Label: "Again", Active: true}, audit); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	it, err := s.Item(ctx, SetListingFeatures, code)
	if err != nil || it.Label != "Contract feature" || !it.Active || it.SortOrder != 5 {
		t.Fatalf("item: %+v %v", it, err)
	}
	changed, err := s.ChangeItem(ctx, SetListingFeatures, code, func(it *Item) (adminaudit.Event, error) {
		it.Label, it.Active = "Renamed", false
		again := audit
		again.ID = uuid.New() // each admin action is its own audit event
		return again, nil
	})
	if err != nil || changed.Label != "Renamed" || changed.Active {
		t.Fatalf("change: %+v %v", changed, err)
	}
	active, err := s.List(ctx, SetListingFeatures, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range active {
		if a.Code == code {
			t.Fatal("an inactive row is not listed")
		}
	}
	all, err := s.List(ctx, SetListingFeatures, Filter{IncludeInactive: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range all {
		found = found || (a.Code == code && a.Label == "Renamed")
	}
	if !found {
		t.Fatal("an inactive row is kept")
	}
	if _, err := s.ChangeItem(ctx, SetListingFeatures, "no-such-code", func(*Item) (adminaudit.Event, error) { return audit, nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown row: %v", err)
	}
}

func TestMemoryAdminStore(t *testing.T) {
	s := NewMemoryStore(nil)
	log := &adminaudit.MemoryLog{}
	s.SetAuditLog(log)
	adminStoreContract(t, s)
	if len(log.All()) != 2 {
		t.Fatalf("audit records: %d", len(log.All()))
	}
}

func TestPostgresAdminStore(t *testing.T) {
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
	defer pool.Close()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM reference_items WHERE set_code = 'listing_features' AND code LIKE 'contract-%'`)
	})
	adminStoreContract(t, NewPostgresStore(pool))
}
