package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// securityContract runs the account-security store checks on a store with
// one user (id, email) holding sessions a and b.
func securityContract(t *testing.T, s Store, user uuid.UUID, email string, a, b uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	list, err := s.ActiveSessions(ctx, user, now)
	if err != nil || len(list) != 2 || list[0].Client == "" {
		t.Fatalf("sessions: %+v %v", list, err)
	}
	if err := s.RevokeUserSession(ctx, uuid.New(), a, now); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("another user's session: %v", err)
	}
	if err := s.ChangePassword(ctx, user, "new-hash", a, now); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ActiveSessions(ctx, user, now); len(list) != 1 || list[0].ID != a {
		t.Fatalf("after password change: %+v", list)
	}
	if n, err := s.RevokeOtherSessions(ctx, user, a, now); err != nil || n != 0 {
		t.Fatalf("revoke others: %d %v", n, err)
	}
	if err := s.RevokeUserSession(ctx, user, a, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeUserSession(ctx, user, a, now); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("revoked twice: %v", err)
	}

	// Email change: the newest request wins, single use.
	newEmail := "changed." + user.String()[:8] + "@example.test"
	old := hashToken("old-token-" + user.String())
	tok := hashToken("new-token-" + user.String())
	if err := s.CreateEmailChange(ctx, user, "first."+newEmail, old, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateEmailChange(ctx, user, newEmail, tok, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConfirmEmailChange(ctx, old, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("replaced token: %v", err)
	}
	id, gotOld, err := s.ConfirmEmailChange(ctx, tok, now)
	if err != nil || id != user || gotOld != email {
		t.Fatalf("confirm: %s %q %v", id, gotOld, err)
	}
	if _, _, err := s.ConfirmEmailChange(ctx, tok, now); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("used twice: %v", err)
	}
	u, err := s.UserByID(ctx, user)
	if err != nil || u.Email != newEmail || u.EmailVerifiedAt == nil || u.PasswordHash != "new-hash" {
		t.Fatalf("user: %+v %v", u, err)
	}
}

func TestPostgresSecurityStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	user := uuid.New()
	email := "security." + user.String()[:8] + "@example.test"
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, email_verified_at, password_hash) VALUES ($1, $2, 'Security Tester', 'ACTIVE', now(), 'old')`,
		user, email); err != nil {
		t.Fatal(err)
	}
	s := NewPostgresStore(pool)
	now := time.Now().UTC()
	a, b := uuid.New(), uuid.New()
	for i, id := range []uuid.UUID{a, b} {
		tok := hashToken(id.String())
		if err := s.CreateSession(ctx, Session{ID: id, UserID: user, TokenHash: tok, Scope: "customer", Delivery: DeliveryBearer,
			CreatedAt: now.Add(time.Duration(-i) * time.Minute), LastSeenAt: now, ExpiresAt: now.Add(time.Hour), Client: "Chrome on Windows"}); err != nil {
			t.Fatal(err)
		}
	}
	securityContract(t, s, user, email, a, b)
}

func TestMemorySecurityStoreContract(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	user := uuid.New()
	email := "security.memory@example.test"
	if err := s.CreateUser(ctx, NewUser{ID: user, Name: "Security Tester", Email: email, PasswordHash: "old", PrivacyVersion: "2026-01"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	a, b := uuid.New(), uuid.New()
	for i, id := range []uuid.UUID{a, b} {
		if err := s.CreateSession(ctx, Session{ID: id, UserID: user, TokenHash: hashToken(id.String()), Scope: "customer", Delivery: DeliveryBearer,
			CreatedAt: now.Add(time.Duration(-i) * time.Minute), LastSeenAt: now, ExpiresAt: now.Add(time.Hour), Client: "Chrome on Windows"}); err != nil {
			t.Fatal(err)
		}
	}
	securityContract(t, s, user, email, a, b)
}
