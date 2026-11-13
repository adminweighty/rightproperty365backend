// Command seed writes the DEVELOPMENT sample data (organisations, people,
// roles, the 18 sample listings and the 29 directory profiles; see
// internal/devseed) into a Postgres
// database. It is the database counterpart of what cmd/api seeds into its
// in-memory stores.
//
// It refuses to run unless APP_ENV=development and DEV_SEED_PASSWORD is set,
// because every sample account shares that published password. It is
// idempotent: re-running it resets the sample rows and touches nothing else.
// Run the migrations first (go run ./cmd/migrate up).
//
// The database is DATABASE_DIRECT_URL, or DATABASE_URL when that is not set.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/database"
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
	if err := devseed.CheckEnvironment(env, password); err != nil {
		return err
	}

	url := os.Getenv("DATABASE_DIRECT_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		return errors.New("neither DATABASE_DIRECT_URL nor DATABASE_URL is set: seed writes to Postgres only " +
			"(without a database the API seeds its in-memory stores by itself)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	pool, err := database.NewPool(ctx, url, 2)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := devseed.SeedPostgres(ctx, pool, identity.DefaultHasher(), password, time.Now()); err != nil {
		return err
	}
	fmt.Printf("seeded %d organisations, %d accounts, %d listings and %d directory profiles (password: DEV_SEED_PASSWORD)\n",
		len(devseed.Organisations()), len(devseed.Accounts()), len(devseed.Listings()), len(devseed.Profiles()))
	return nil
}
