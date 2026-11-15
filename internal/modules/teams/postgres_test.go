package teams

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// testDatabaseEnv names a THROWAWAY Postgres database for the Postgres store
// tests. They are skipped when it is not set (there is no database where
// these tests are usually run). It is never read from .env: point it at a
// database you can lose, e.g. a local container. The tests migrate it to the
// latest version and only touch rows they create (random IDs), which they
// delete at the end.
const testDatabaseEnv = "RP365_TEST_DATABASE_URL"

type recordingMailer struct{ last InvitationMail }

func (m *recordingMailer) SendInvitation(_ context.Context, _ string, inv InvitationMail) error {
	m.last = inv
	return nil
}

func openTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseEnv)
	if url == "" {
		t.Skipf("%s is not set: the Postgres store is only exercised against a throwaway database", testDatabaseEnv)
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

// The Postgres store, end to end: invite, replace, accept (membership,
// account role, agent profile), suspend (no access on the next read), the
// last-owner rule, remove, and the audit trail.
func TestPostgresStore(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	org, owner, agent, invitee := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	suffix := strings.ReplaceAll(org.String()[:8], "-", "")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		for _, sql := range []string{
			`DELETE FROM directory_profiles WHERE organisation_id = $1`,
			`DELETE FROM organisations WHERE id = $1`,
		} {
			_, _ = pool.Exec(ctx, sql, org)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{owner, agent, invitee})
	})
	exec(`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'AGENCY', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
		org, "Test Agency "+suffix)
	for _, u := range []struct {
		id   uuid.UUID
		name string
	}{{owner, "Olive Owner"}, {agent, "Andy Agent"}, {invitee, "Ivy Invitee"}} {
		exec(`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, $3, 'ACTIVE', now())`,
			u.id, strings.ToLower(strings.Fields(u.name)[0])+"."+suffix+"@example.test", u.name)
	}
	exec(`INSERT INTO memberships (id, organisation_id, user_id, role, status, market_id) SELECT $1, $2, $3, 'OWNER', 'ACTIVE', market_id FROM organisations WHERE id = $2`, uuid.New(), org, owner)
	exec(`INSERT INTO memberships (id, organisation_id, user_id, role, status, market_id) SELECT $1, $2, $3, 'AGENT', 'ACTIVE', market_id FROM organisations WHERE id = $2`, uuid.New(), org, agent)
	exec(`INSERT INTO user_account_roles (id, user_id, role, status) VALUES ($1, $2, 'AGENT', 'PENDING_VERIFICATION')`, uuid.New(), invitee)

	users := identity.NewPostgresStore(pool)
	principal := func(id uuid.UUID) *auth.Principal {
		t.Helper()
		u, err := users.UserByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return &auth.Principal{UserID: u.ID, Name: u.Name, Email: u.Email, Memberships: u.Memberships, SessionScope: auth.ScopeCustomer}
	}
	mailer := &recordingMailer{}
	svc := NewService(NewPostgresStore(pool), mailer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.SetClock(func() time.Time { return now })

	inviteeEmail := "ivy." + suffix + "@example.test"
	if _, err := svc.Invite(ctx, principal(owner), org, InviteInput{Email: inviteeEmail, Role: auth.RoleAgent}); err != nil {
		t.Fatal(err)
	}
	first := mailer.last.Token
	if _, err := svc.Invite(ctx, principal(owner), org, InviteInput{Email: inviteeEmail, Role: auth.RoleAgent}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Invitation(ctx, first); err != ErrInvitationUsed {
		t.Fatalf("the replaced invitation: %v", err)
	}
	if _, err := svc.Invite(ctx, principal(owner), org, InviteInput{Email: "andy." + suffix + "@example.test", Role: auth.RoleStaff}); err != ErrAlreadyMember {
		t.Fatalf("inviting a member: %v", err)
	}
	accepted, err := svc.Accept(ctx, principal(invitee), mailer.last.Token)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Profile == nil || accepted.Profile.Kind != "agents" {
		t.Fatalf("accepted: %+v", accepted)
	}
	if p := principal(invitee); p.MembershipRole(org) != auth.RoleAgent {
		t.Fatalf("the invitee's memberships: %+v", p.Memberships)
	}
	if _, err := svc.Accept(ctx, principal(invitee), mailer.last.Token); err != ErrInvitationUsed {
		t.Fatalf("accepting twice: %v", err)
	}

	suspended := MemberSuspended
	if _, err := svc.ChangeMember(ctx, principal(owner), org, agent, MemberPatch{Status: &suspended}); err != nil {
		t.Fatal(err)
	}
	if p := principal(agent); p.MembershipRole(org) != "" {
		t.Fatal("a suspended member still has access")
	}
	if _, err := svc.ChangeMember(ctx, principal(owner), org, owner, MemberPatch{Status: &suspended}); err != ErrSelfChange {
		t.Fatalf("an owner suspending themselves: %v", err)
	}
	if _, err := svc.RemoveMember(ctx, principal(owner), org, agent); err != nil {
		t.Fatal(err)
	}
	_, members, err := svc.Members(ctx, principal(owner), org)
	if err != nil || len(members) != 2 {
		t.Fatalf("members after the removal: %+v %v", members, err)
	}
	page, err := svc.Audit(ctx, principal(owner), org, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, it := range page.Items {
		actions = append(actions, it.Action)
	}
	if strings.Join(actions, ",") != "MEMBER_REMOVED,MEMBER_SUSPENDED,INVITATION_ACCEPTED,INVITATION_CREATED,INVITATION_CREATED" {
		t.Fatalf("audit: %v", actions)
	}
	if page.Items[1].After["released_requests"] != float64(0) {
		t.Fatalf("suspension audit: %+v", page.Items[1].After)
	}
}
