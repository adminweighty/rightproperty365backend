package inapp

import (
	"context"
	"os"
	"testing"

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

// The store contract on Postgres, with the users rows the foreign keys need,
// and the account-deletion cleanup (DeleteUserTx).
func TestPostgresStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	alice, bob := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{alice, bob} {
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Inbox Tester', 'ACTIVE', now())`,
			u, "inapp."+u.String()[:8]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	s := NewPostgresStore(pool)
	storeContract(t, s, alice, bob)

	var history int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_preference_changes WHERE user_id = $1`, alice).Scan(&history); err != nil || history != 1 {
		t.Fatalf("history rows = %d %v", history, err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return DeleteUserTx(ctx, tx, alice) }); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT count(*) FROM notifications WHERE user_id = $1`, `SELECT count(*) FROM user_devices WHERE user_id = $1`,
		`SELECT count(*) FROM notification_preferences WHERE user_id = $1`, `SELECT count(*) FROM notification_deliveries WHERE user_id = $1`} {
		var n int
		if err := pool.QueryRow(ctx, q, alice).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d %v", q, n, err)
		}
	}
}
