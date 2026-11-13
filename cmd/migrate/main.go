// Command migrate applies the embedded SQL migrations.
//
//	migrate up       apply all pending migrations
//	migrate down     revert the most recent migration (one step)
//	migrate status   list migrations and whether they are applied
//
// It connects with DATABASE_DIRECT_URL (a direct connection, never the
// PgBouncer pooler) and serialises concurrent runs with an advisory lock.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 || (args[0] != "up" && args[0] != "down" && args[0] != "status") {
		return fmt.Errorf("usage: migrate up|down|status")
	}
	if err := config.LoadDotEnv(".env"); err != nil {
		return fmt.Errorf("read .env: %w", err)
	}
	url := os.Getenv("DATABASE_DIRECT_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_DIRECT_URL is not set (use the direct connection string, not the pooled one)")
	}

	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	runner := migrate.NewRunner(conn, migs)
	switch args[0] {
	case "up":
		applied, err := runner.Up(ctx)
		for _, m := range applied {
			fmt.Printf("applied  %04d_%s\n", m.Version, m.Name)
		}
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			fmt.Println("nothing to apply; database is up to date")
		}
	case "down":
		reverted, err := runner.Down(ctx)
		if err != nil {
			return err
		}
		if reverted == nil {
			fmt.Println("nothing to revert")
		} else {
			fmt.Printf("reverted %04d_%s\n", reverted.Version, reverted.Name)
		}
	case "status":
		statuses, err := runner.Status(ctx)
		if err != nil {
			return err
		}
		for _, st := range statuses {
			state := "pending"
			if st.AppliedAt != nil {
				state = "applied " + st.AppliedAt.Format(time.RFC3339)
			}
			fmt.Printf("%04d_%-30s %s\n", st.Migration.Version, st.Migration.Name, state)
		}
	}
	return nil
}
