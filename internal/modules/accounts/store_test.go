package accounts

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// harness builds the fixtures of the store contract on one kind of store.
type harness struct {
	store Store
	// user creates an active, verified account with a session.
	user func(t *testing.T, name string) uuid.UUID
	// liveSessions counts the user's sessions that are not revoked.
	liveSessions func(t *testing.T, id uuid.UUID) int
	// agency creates an organisation with owner as its OWNER and member as
	// an AGENT, and returns its name.
	agency func(t *testing.T, owner, member uuid.UUID) string
}

func storeContract(t *testing.T, h harness) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := uuid.NewString()[:8]
	alice := h.user(t, "Alice Contract "+suffix)
	bob := h.user(t, "Bob Contract "+suffix)
	admin := Actor{UserID: uuid.Nil, Name: "Test Admin"}

	items, total, err := h.store.Search(ctx, Filter{Q: "contract " + suffix, Limit: 10})
	if err != nil || total != 2 || len(items) != 2 {
		t.Fatalf("search: %d %+v %v", total, items, err)
	}
	if _, total, _ := h.store.Search(ctx, Filter{Q: "contract " + suffix, Status: StatusSuspended}); total != 0 {
		t.Fatalf("status filter: %d", total)
	}
	if _, total, _ := h.store.Search(ctx, Filter{Q: "contract " + suffix, Role: "AGENT"}); total != 2 {
		t.Fatalf("role filter: %d", total)
	}
	if _, err := h.store.Account(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	a, err := h.store.Account(ctx, alice)
	if err != nil || a.Status != StatusActive || a.LastSignInAt == nil || len(a.AccountRoles) != 1 || a.PasswordHash == "" {
		t.Fatalf("account: %+v %v", a, err)
	}

	audit := adminaudit.New(uuid.Nil, admin.Name, adminaudit.ActionUserSuspended, adminaudit.TargetUser, alice.String(), "Alice", "Because", nil, nil, now)
	got, err := h.store.Suspend(ctx, Suspension{UserID: alice, Reason: "Because of reports", Actor: admin, At: now, Audit: audit})
	if err != nil || got.Status != StatusSuspended || got.SuspensionReason != "Because of reports" || got.SuspendedAt == nil {
		t.Fatalf("suspend: %+v %v", got, err)
	}
	if n := h.liveSessions(t, alice); n != 0 {
		t.Fatalf("%d sessions still live after suspension", n)
	}
	if _, err := h.store.Suspend(ctx, Suspension{UserID: alice, Reason: "Again", Actor: admin, At: now, Audit: audit}); !errors.Is(err, ErrState) {
		t.Fatalf("suspending twice: %v", err)
	}
	if _, total, _ := h.store.Search(ctx, Filter{Q: "contract " + suffix, Status: StatusSuspended}); total != 1 {
		t.Fatalf("suspended filter: %d", total)
	}
	reactivated := adminaudit.New(uuid.Nil, admin.Name, adminaudit.ActionUserReactivated, adminaudit.TargetUser, alice.String(), "Alice", "", nil, nil, now)
	got, err = h.store.Reactivate(ctx, Reactivation{UserID: alice, Actor: admin, At: now, Audit: reactivated})
	if err != nil || got.Status != StatusActive || got.SuspensionReason != "" || got.SuspendedAt != nil {
		t.Fatalf("reactivate: %+v %v", got, err)
	}
	if _, err := h.store.Reactivate(ctx, Reactivation{UserID: alice, Actor: admin, At: now, Audit: reactivated}); !errors.Is(err, ErrState) {
		t.Fatalf("reactivating an active account: %v", err)
	}

	// The only owner of an organisation with a member cannot be deleted.
	org := h.agency(t, alice, bob)
	deletion := func(id uuid.UUID) Deletion {
		return Deletion{UserID: id, Actor: admin, Reason: "Asked", At: now,
			Audit: adminaudit.New(uuid.Nil, admin.Name, adminaudit.ActionUserDeleted, adminaudit.TargetUser, id.String(), DeletedName, "Asked", nil, nil, now)}
	}
	var sole *SoleOwnerError
	if _, err := h.store.Delete(ctx, deletion(alice)); !errors.As(err, &sole) || len(sole.Organisations) != 1 || sole.Organisations[0] != org {
		t.Fatalf("sole owner: %v", err)
	}
	if a, _ := h.store.Account(ctx, alice); a.Status != StatusActive || a.Email == DeletedEmail(alice) {
		t.Fatalf("a refused deletion changes nothing: %+v", a)
	}

	// The member can go: anonymised, every session ended, removed from the team.
	out, err := h.store.Delete(ctx, deletion(bob))
	if err != nil || out.OrganisationsLeft != 1 {
		t.Fatalf("delete: %+v %v", out, err)
	}
	b, err := h.store.Account(ctx, bob)
	if err != nil || b.Status != StatusDeleted || b.Name != DeletedName || b.Email != DeletedEmail(bob) || b.PasswordHash != "" || b.DeletedAt == nil {
		t.Fatalf("anonymised: %+v %v", b, err)
	}
	for _, m := range b.Memberships {
		if m.Status != "REMOVED" {
			t.Fatalf("membership: %+v", m)
		}
	}
	if n := h.liveSessions(t, bob); n != 0 {
		t.Fatalf("%d sessions still live after deletion", n)
	}
	if _, err := h.store.Delete(ctx, deletion(bob)); !errors.Is(err, ErrState) {
		t.Fatalf("deleting twice: %v", err)
	}
	// Alice is now the only member: she may go.
	if _, err := h.store.Delete(ctx, deletion(alice)); err != nil {
		t.Fatalf("delete the last member: %v", err)
	}
}

func newSession(userID uuid.UUID, now time.Time) identity.Session {
	hash := make([]byte, 32)
	copy(hash, uuid.New().String())
	return identity.Session{ID: uuid.New(), UserID: userID, TokenHash: hash, Scope: auth.ScopeCustomer, Delivery: identity.DeliveryCookie,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}
}

func newUser(name string, now time.Time) identity.NewUser {
	email := strings.ToLower(strings.ReplaceAll(name, " ", ".")) + "@example.test"
	return identity.NewUser{ID: uuid.New(), Name: name, Email: email, PasswordHash: "not-a-real-hash", PrivacyVersion: "test",
		AccountRoles: []identity.AccountRole{{Role: "AGENT", Status: "ACTIVE"}}, EmailVerifiedAt: &now, CreatedAt: now}
}

func TestMemoryStore(t *testing.T) {
	ids := identity.NewMemoryStore()
	ctx := context.Background()
	now := time.Now().UTC()
	storeContract(t, harness{
		store: NewMemoryStore(MemoryDeps{Identity: ids}),
		user: func(t *testing.T, name string) uuid.UUID {
			u := newUser(name, now)
			if err := ids.CreateUser(ctx, u); err != nil {
				t.Fatal(err)
			}
			if err := ids.CreateSession(ctx, newSession(u.ID, now)); err != nil {
				t.Fatal(err)
			}
			return u.ID
		},
		liveSessions: func(t *testing.T, id uuid.UUID) int {
			n := 0
			for _, s := range ids.Sessions() {
				if s.UserID == id && s.RevokedAt == nil {
					n++
				}
			}
			return n
		},
		agency: func(t *testing.T, owner, member uuid.UUID) string {
			org := identity.MemoryOrganisation{ID: uuid.New(), Name: "Contract Agency", Kind: auth.KindAgency, MarketCode: "ZW"}
			ids.PutOrganisation(org)
			if err := ids.AddMembership(owner, org.ID, "ZW", auth.RoleOwner); err != nil {
				t.Fatal(err)
			}
			if err := ids.AddMembership(member, org.ID, "ZW", auth.RoleAgent); err != nil {
				t.Fatal(err)
			}
			return org.Name
		},
	})
}

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

func TestPostgresStore(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	users := identity.NewPostgresStore(pool)
	var created []uuid.UUID
	var orgs []uuid.UUID
	t.Cleanup(func() {
		// Audit rows keep their actor and target; only the fixtures go.
		for _, org := range orgs {
			_, _ = pool.Exec(context.Background(), `DELETE FROM organisation_audit_events WHERE organisation_id = $1`, org)
			_, _ = pool.Exec(context.Background(), `DELETE FROM organisations WHERE id = $1`, org)
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1)`, created)
	})
	storeContract(t, harness{
		store: NewPostgresStore(pool),
		user: func(t *testing.T, name string) uuid.UUID {
			u := newUser(name, now)
			if err := users.CreateUser(ctx, u); err != nil {
				t.Fatal(err)
			}
			created = append(created, u.ID)
			if err := users.CreateSession(ctx, newSession(u.ID, now)); err != nil {
				t.Fatal(err)
			}
			return u.ID
		},
		liveSessions: func(t *testing.T, id uuid.UUID) int {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, id).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		},
		agency: func(t *testing.T, owner, member uuid.UUID) string {
			org := uuid.New()
			orgs = append(orgs, org)
			name := "Contract Agency " + org.String()[:8]
			for _, sql := range []string{
				`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'AGENCY', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
			} {
				if _, err := pool.Exec(ctx, sql, org, name); err != nil {
					t.Fatal(err)
				}
			}
			for _, m := range []struct {
				user uuid.UUID
				role string
			}{{owner, "OWNER"}, {member, "AGENT"}} {
				if _, err := pool.Exec(ctx, `INSERT INTO memberships (id, organisation_id, user_id, role, status, market_id)
					SELECT $1, $2, $3, $4, 'ACTIVE', market_id FROM organisations WHERE id = $2`, uuid.New(), org, m.user, m.role); err != nil {
					t.Fatal(err)
				}
			}
			return name
		},
	})
}
