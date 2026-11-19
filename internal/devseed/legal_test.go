package devseed

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/legal"
	"rightproperty365.com/backend/internal/modules/tax"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// The legal and tax seed writes the development documents and sample rules
// and can be run again without error or duplicates.
func TestSeedPostgresLegalTaxIsRerunnable(t *testing.T) {
	url := os.Getenv("RP365_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RP365_TEST_DATABASE_URL is not set: the Postgres seed is only exercised against a throwaway database")
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
	for i := 0; i < 2; i++ {
		if err := SeedPostgresLegalTax(ctx, pool, time.Now()); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var published, drafts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'PUBLISHED' AND market_code = 'GLOBAL' AND draft_notice),
		count(*) FILTER (WHERE status = 'DRAFT' AND market_code IN ('ZW', 'GB'))
		FROM legal_document_versions WHERE version_label = $1`, legal.SeedVersion).Scan(&published, &drafts); err != nil {
		t.Fatal(err)
	}
	if published != 4 || drafts != 8 {
		t.Fatalf("legal seed: %d published, %d drafts", published, drafts)
	}
	var rate string
	var sample bool
	if err := pool.QueryRow(ctx, `SELECT rate::text, is_sample FROM tax_rules WHERE id = $1 AND status = 'ACTIVE'`, tax.SeedZWVATID).
		Scan(&rate, &sample); err != nil || rate != "15.0000" || !sample {
		t.Fatalf("tax seed: %q %v %v", rate, sample, err)
	}
}
