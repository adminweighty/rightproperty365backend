package identity

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

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

// The marketing store contract on Postgres (CreateUser writes every row the
// foreign keys need: users, account roles, consent), the append-only
// trigger, the account-deletion cleanup (DeleteMarketingConsentTx) and the
// eligibility of a suspended account.
func TestPostgresMarketingStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	s := NewPostgresStore(pool)
	suffix := uuid.NewString()[:8]
	marketingStoreContract(t, s, suffix)
	consentTextContract(t, s)

	// The wording rows are immutable: no update, no delete.
	if _, err := pool.Exec(ctx, `UPDATE marketing_consent_texts SET status = 'APPROVED' WHERE version = $1`, CurrentMarketingConsentVersion); err == nil {
		t.Fatal("an update of a consent wording was accepted")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM marketing_consent_texts WHERE version = $1`, CurrentMarketingConsentVersion); err == nil {
		t.Fatal("a delete of a consent wording was accepted")
	}

	now := time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)
	id := newMarketingUser(t, s, "mkt-pg-"+suffix+"@example.test", true, true, now)

	// Append-only: an update is refused.
	if _, err := pool.Exec(ctx, `UPDATE marketing_consent_events SET source = 'api' WHERE user_id = $1`, id); err == nil {
		t.Fatal("an update of a consent record was accepted")
	}
	// A record must not carry a version on opt-out.
	if _, err := pool.Exec(ctx, `INSERT INTO marketing_consent_events (id, user_id, opt_in, changed_at, source, context, consent_text_version)
		VALUES ($1, $2, false, now(), 'web', 'PREFERENCES', 'marketing-2026-01')`, uuid.New(), id); err == nil {
		t.Fatal("an opt-out with a version was accepted")
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'SUSPENDED', suspended_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if e, err := s.MarketingEligibility(ctx, id); err != nil || e.Status != StatusSuspended || !e.OptIn {
		t.Fatalf("eligibility = %+v %v", e, err)
	}

	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return DeleteMarketingConsentTx(ctx, tx, id) }); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM marketing_consent_events WHERE user_id = $1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("records after deletion = %d %v", n, err)
	}
}
