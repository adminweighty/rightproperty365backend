package identity

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// mfaStoreContract is run on the memory store and, behind
// RP365_TEST_DATABASE_URL, on Postgres. The account row is created with
// CreateUser, which writes every row the foreign keys need.
func mfaStoreContract(t *testing.T, store Store, email string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	id := newMarketingUser(t, store, email, false, true, now)
	audit := func(action string) adminaudit.Event {
		return adminaudit.New(id, "Marketing Tester", action, adminaudit.TargetUser, id.String(), "", "", nil, nil, now)
	}
	sealed := bytes.Repeat([]byte{7}, 49)

	if _, err := store.AdminMFA(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no row yet: %v", err)
	}
	if err := store.UseTOTPStep(ctx, id, 5, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("step without an authenticator: %v", err)
	}
	if err := store.PutPendingTOTP(ctx, id, sealed, now); err != nil {
		t.Fatal(err)
	}
	// A pending setup is replaced.
	sealed2 := bytes.Repeat([]byte{8}, 49)
	if err := store.PutPendingTOTP(ctx, id, sealed2, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rec, err := store.AdminMFA(ctx, id)
	if err != nil || rec.ConfirmedAt != nil || !bytes.Equal(rec.SecretSealed, sealed2) {
		t.Fatalf("pending = %+v %v", rec, err)
	}
	if err := store.UseTOTPStep(ctx, id, 5, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("step on a pending setup: %v", err)
	}
	// A setup older than notBefore is not confirmed.
	if err := store.ConfirmTOTP(ctx, id, 100, []string{"h"}, now.Add(2*time.Minute), now, audit(adminaudit.ActionAdminMFAEnrolled)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale setup confirmed: %v", err)
	}
	hashes := []string{"$argon2id$v=19$m=64,t=1,p=1$aaaaaaaaaaaaaaaaaaaaaa$one", "$argon2id$v=19$m=64,t=1,p=1$aaaaaaaaaaaaaaaaaaaaaa$two"}
	if err := store.ConfirmTOTP(ctx, id, 100, hashes, now, now.Add(2*time.Minute), audit(adminaudit.ActionAdminMFAEnrolled)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPendingTOTP(ctx, id, sealed, now); !errors.Is(err, ErrMFAAlreadyEnrolled) {
		t.Fatalf("setup over a confirmed authenticator: %v", err)
	}
	// Replay protection: only a newer step.
	for step, want := range map[int64]error{100: ErrMFACodeReused, 99: ErrMFACodeReused} {
		if err := store.UseTOTPStep(ctx, id, step, now); !errors.Is(err, want) {
			t.Fatalf("step %d: %v", step, err)
		}
	}
	if err := store.UseTOTPStep(ctx, id, 101, now); err != nil {
		t.Fatal(err)
	}
	if err := store.UseTOTPStep(ctx, id, 101, now); !errors.Is(err, ErrMFACodeReused) {
		t.Fatalf("same step twice: %v", err)
	}

	codes, err := store.UnusedRecoveryCodes(ctx, id)
	if err != nil || len(codes) != 2 {
		t.Fatalf("codes = %d %v", len(codes), err)
	}
	ev := OutboxEvent{Type: EventAdminMFARecoveryCodeUsed, UserID: id, OccurredAt: now}
	if err := store.UseRecoveryCode(ctx, id, codes[0].ID, now, audit(adminaudit.ActionAdminMFARecoveryCodeUsed), ev); err != nil {
		t.Fatal(err)
	}
	if err := store.UseRecoveryCode(ctx, id, codes[0].ID, now, audit(adminaudit.ActionAdminMFARecoveryCodeUsed), ev); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a recovery code used twice: %v", err)
	}
	if left, _ := store.UnusedRecoveryCodes(ctx, id); len(left) != 1 {
		t.Fatalf("%d left", len(left))
	}
	if err := store.ReplaceRecoveryCodes(ctx, id, hashes, now, audit(adminaudit.ActionAdminMFARecoveryRegenerated)); err != nil {
		t.Fatal(err)
	}
	if left, _ := store.UnusedRecoveryCodes(ctx, id); len(left) != 2 {
		t.Fatalf("%d after regeneration", len(left))
	}

	// The sign-in challenge's second-factor step.
	idHash := bytes.Repeat([]byte{byte(now.Nanosecond() + 3)}, 32)
	copy(idHash, id[:])
	c := LoginChallenge{IDHash: idHash, UserID: id, Scope: "admin", Delivery: DeliveryCookie, CodeHash: []byte("h"),
		ExpiresAt: now.Add(10 * time.Minute), LastSentAt: now, CreatedAt: now}
	if err := store.CreateLoginChallenge(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkLoginChallengeCodeVerified(ctx, idHash, now, now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkLoginChallengeCodeVerified(ctx, idHash, now, now.Add(20*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("marked twice: %v", err)
	}
	got, err := store.LoginChallenge(ctx, idHash)
	if err != nil || got.CodeVerifiedAt == nil || !got.ExpiresAt.Equal(now.Add(20*time.Minute)) {
		t.Fatalf("challenge = %+v %v", got, err)
	}
	if err := store.ResendLoginChallenge(ctx, idHash, []byte("x"), now.Add(time.Hour), now, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resend after the code step: %v", err)
	}
	for i := 1; i <= SignInCodeMaxAttempts; i++ {
		n, err := store.FailLoginChallengeMFA(ctx, idHash, now)
		if err != nil || n != i {
			t.Fatalf("fail %d: %d %v", i, n, err)
		}
	}
	if err := store.ConsumeLoginChallenge(ctx, idHash, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a locked challenge was consumed: %v", err)
	}
	if err := store.RecordAdminAudit(ctx, audit(adminaudit.ActionAdminMFAChallengeLocked)); err != nil {
		t.Fatal(err)
	}

	// Removal.
	if err := store.DeleteAdminMFA(ctx, id, now, audit(adminaudit.ActionAdminMFAReset), true,
		OutboxEvent{Type: EventAdminMFAReset, UserID: id, OccurredAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAdminMFA(ctx, id, now, audit(adminaudit.ActionAdminMFAReset), true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nothing to delete: %v", err)
	}
	if left, _ := store.UnusedRecoveryCodes(ctx, id); len(left) != 0 {
		t.Fatalf("%d recovery codes after removal", len(left))
	}
	return id
}

func TestMemoryMFAStoreContract(t *testing.T) {
	store := NewMemoryStore()
	log := &adminaudit.MemoryLog{}
	store.SetAuditLog(log)
	mfaStoreContract(t, store, "mfa-mem@example.test")
	if n := len(log.All()); n != 5 {
		t.Fatalf("%d audit records, want 5", n)
	}
	// Anonymisation removes the rows.
	id := newMarketingUser(t, store, "mfa-anon@example.test", false, true, time.Now())
	if err := store.PutPendingTOTP(context.Background(), id, []byte("sealed"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AnonymiseAccount(id, "Deleted user", "deleted@example.invalid", time.Now()); err != nil {
		t.Fatal(err)
	}
	if store.HasAdminMFA(id) {
		t.Fatal("anonymisation kept the MFA rows")
	}
}

// The MFA store on Postgres (migration 0042), the audit records it writes in
// the same transactions, and DeleteAdminMFATx (account deletion).
func TestPostgresMFAStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	s := NewPostgresStore(pool)
	suffix := uuid.NewString()[:8]
	id := mfaStoreContract(t, s, "mfa-pg-"+suffix+"@example.test")

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_events WHERE target_id = $1 AND action LIKE 'ADMIN_MFA_%'`, id.String()).Scan(&n); err != nil || n != 5 {
		t.Fatalf("audit records = %d %v", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type LIKE 'ADMIN_MFA_%'`, id).Scan(&n); err != nil || n != 2 {
		t.Fatalf("outbox events = %d %v", n, err)
	}
	// The secret column refuses something too short to be sealed.
	if _, err := pool.Exec(ctx, `INSERT INTO admin_mfa_totp (user_id, secret_sealed, created_at, updated_at) VALUES ($1, '\x00', now(), now())`, id); err == nil {
		t.Fatal("a malformed sealed secret was accepted")
	}
	// A customer challenge cannot be marked as waiting for a second factor.
	if _, err := pool.Exec(ctx, `INSERT INTO sign_in_challenges (id, user_id, scope, delivery, code_hash, expires_at, last_sent_at, code_verified_at)
		VALUES ($1, $2, 'customer', 'cookie', '\x00', now(), now(), now())`, bytes.Repeat([]byte{9}, 32), id); err == nil {
		t.Fatal("a customer challenge with a second factor was accepted")
	}

	// Account deletion: DeleteAdminMFATx removes the rows.
	now := time.Now().UTC()
	if err := s.PutPendingTOTP(ctx, id, bytes.Repeat([]byte{7}, 49), now); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := PutConfirmedAdminMFATx(ctx, tx, id, bytes.Repeat([]byte{7}, 49), []string{"$argon2id$v=19$m=64,t=1,p=1$aaaa$bbbbbbbbbbbbbbbbbbbbbbbb"}, now); err != nil {
			return err
		}
		existed, err := DeleteAdminMFATx(ctx, tx, id)
		if err == nil && !existed {
			t.Error("the seeded authenticator was not found")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminMFA(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after deletion: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_mfa_recovery_codes WHERE user_id = $1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("recovery codes after deletion = %d %v", n, err)
	}
}
