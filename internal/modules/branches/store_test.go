package branches

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/migrations"
)

// contract runs the Store rules. org has member user (named "Branch
// Tester"); listing is unknown to the store's organisation unless tie is true.
func contract(t *testing.T, store Store, org, user, actor uuid.UUID, audits func() int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ev := func(action string) orgaudit.Event {
		return orgaudit.New(org, actor, "Owner", action, TargetBranch, uuid.New(), "x", nil, nil, now)
	}
	before := audits()
	mk := func(name string) Branch {
		return Branch{ID: uuid.New(), OrganisationID: org, Name: name, Active: true, CreatedAt: now, UpdatedAt: now}
	}
	a, b := mk("Harare North"), mk("Bulawayo")
	for _, br := range []Branch{a, b} {
		if err := store.Create(ctx, br, ev(ActionCreated)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Create(ctx, mk("HARARE NORTH"), ev(ActionCreated)); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	list, err := store.List(ctx, org)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %v", list, err)
	}
	if _, err := store.Get(ctx, uuid.New(), a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other organisation: %v", err)
	}
	b.Name, b.Active = "Harare north", false
	if err := store.Update(ctx, b, ev(ActionUpdated)); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("rename onto a: %v", err)
	}
	b.Name = "Bulawayo CBD"
	if err := store.Update(ctx, b, ev(ActionUpdated)); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ctx, org, b.ID); err != nil || got.Name != "Bulawayo CBD" || got.Active {
		t.Fatalf("updated: %+v %v", got, err)
	}
	if found, err := ResolveName(ctx, store, org, " harare NORTH "); err != nil || found == nil || found.ID != a.ID {
		t.Fatalf("resolve: %v %v", found, err)
	}
	if found, _ := ResolveName(ctx, store, org, "bulawayo cbd"); found != nil {
		t.Fatal("an archived branch resolved")
	}

	var name string
	if err := store.PinMember(ctx, org, user, &a.ID, func(n string) orgaudit.Event { name = n; return ev(ActionMemberBranch) }); err != nil {
		t.Fatal(err)
	}
	if name != "Branch Tester" {
		t.Fatalf("member name %q", name)
	}
	if pins, err := store.Pins(ctx, org); err != nil || pins[user] != a.ID || len(pins) != 1 {
		t.Fatalf("pins: %v %v", pins, err)
	}
	if err := store.PinMember(ctx, org, uuid.New(), &a.ID, func(string) orgaudit.Event { return ev(ActionMemberBranch) }); !errors.Is(err, ErrNotMember) {
		t.Fatalf("not a member: %v", err)
	}
	if err := store.PinMember(ctx, org, user, nil, func(string) orgaudit.Event { return ev(ActionMemberBranch) }); err != nil {
		t.Fatal(err)
	}
	if pins, _ := store.Pins(ctx, org); len(pins) != 0 {
		t.Fatalf("unpinned: %v", pins)
	}
	if err := store.TieListing(ctx, org, uuid.New(), &a.ID, func(string) orgaudit.Event { return ev(ActionListingBranch) }); !errors.Is(err, ErrNoListing) {
		t.Fatalf("unknown listing: %v", err)
	}
	// 2 creates, 1 update, 2 pins.
	if got := audits() - before; got != 5 {
		t.Fatalf("%d audit events, want 5", got)
	}
}

func TestMemoryStoreContract(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	var log []orgaudit.Event
	store := NewMemoryStore(MemoryDeps{
		Member: func(o, u uuid.UUID) (string, bool) { return "Branch Tester", o == org && u == user },
		Listing: func(uuid.UUID) (*uuid.UUID, string, bool) {
			return nil, "", false
		},
		Audit: func(e orgaudit.Event) { log = append(log, e) },
	})
	contract(t, store, org, user, uuid.New(), func() int { return len(log) })
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

func TestPostgresStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	org, user, owner := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh organisation per run, so reruns on one database don't collide.
	exec(`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'AGENCY', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
		org, "Branch Org "+org.String()[:8])
	for _, u := range []struct {
		id         uuid.UUID
		name, role string
	}{{user, "Branch Tester", "AGENT"}, {owner, "Branch Owner", "OWNER"}} {
		exec(`INSERT INTO users (id, email, full_name, status, email_verified_at, password_hash) VALUES ($1, $2, $3, 'ACTIVE', now(), 'x')`,
			u.id, "branches."+u.id.String()[:8]+"@example.test", u.name)
		exec(`INSERT INTO memberships (id, organisation_id, user_id, role, status, market_id)
			SELECT $1, $2, $3, $4, 'ACTIVE', market_id FROM organisations WHERE id = $2`, uuid.New(), org, u.id, u.role)
	}
	contract(t, NewPostgresStore(pool), org, user, owner, func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM organisation_audit_events WHERE organisation_id = $1`, org).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
}
