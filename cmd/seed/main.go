// Command seed writes the DEVELOPMENT sample data (organisations, people,
// roles, the 18 sample listings and the 29 directory profiles; see
// internal/devseed) into a Postgres
// database. It is the database counterpart of what cmd/api seeds into its
// in-memory stores.
//
// It runs when APP_ENV=development and DEV_SEED_PASSWORD is set. It also runs
// against STAGING, but only with SEED_ALLOW_STAGING=yes-create-sample-accounts
// and a DEV_SEED_PASSWORD of at least 24 characters: staging is on the
// internet and one sample account is a platform administrator, so generate a
// private password for it and never use the published development value. It
// never runs in production. It is idempotent: re-running it resets the sample
// rows (including the sample accounts' password) and touches nothing else.
// Run the migrations first (go run ./cmd/migrate up).
//
// The database is DATABASE_URL (or the optional DATABASE_DIRECT_URL override).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/database"
	"rightproperty365.com/backend/internal/platform/storage"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := config.LoadDotEnv(".env"); err != nil {
		return fmt.Errorf("read .env: %w", err)
	}
	// Read the two guard variables directly: the guard must hold even when
	// the rest of the configuration is incomplete. An unset APP_ENV is treated
	// as "not development" here, unlike the API's default.
	env, password := os.Getenv("APP_ENV"), os.Getenv("DEV_SEED_PASSWORD")
	if err := devseed.CheckEnvironment(env, password, os.Getenv("SEED_ALLOW_STAGING")); err != nil {
		return err
	}

	url, poolMode, err := config.ToolDatabase(os.Getenv)
	if err != nil {
		return err
	}
	if url == "" {
		return errors.New("DATABASE_URL is not set: seed writes to PostgreSQL only " +
			"(without a database the API seeds its in-memory stores by itself)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	pool, err := database.NewPool(ctx, url, 2, poolMode)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The onboarding demo documents go to the configured file storage
	// (STORAGE_DRIVER; the local folder by default in development).
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	files, err := storage.Open(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		return err
	}

	if err := devseed.SeedPostgres(ctx, pool, files, identity.DefaultHasher(), password, time.Now()); err != nil {
		return err
	}
	// The agreement samples (needs migration 0023), after the main seed.
	if err := devseed.SeedPostgresAgreements(ctx, pool, files, time.Now()); err != nil {
		return err
	}
	fmt.Printf("seeded %d organisations, %d accounts, %d listings and %d directory profiles (password: DEV_SEED_PASSWORD)\n",
		len(devseed.Organisations()), len(devseed.Accounts()), len(devseed.Listings()), len(devseed.Profiles()))
	return nil
}
