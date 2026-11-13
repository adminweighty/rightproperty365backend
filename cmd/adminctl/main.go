// Command adminctl creates platform administrators and changes their
// passwords directly in Postgres. Platform roles cannot be obtained through
// the API, so this is how the first administrator of an environment is made.
//
//	adminctl create --email EMAIL --name NAME --role ROLE [--market CC]
//	adminctl set-password --email EMAIL
//
// The password comes from the ADMINCTL_PASSWORD environment variable or from
// standard input (typed without echo on a terminal, or piped). It is never a
// flag, never echoed and never logged. The database is DATABASE_DIRECT_URL,
// or DATABASE_URL when that is not set; there is no in-memory mode.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"

	"rightproperty365.com/backend/internal/adminctl"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/database"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "adminctl:", err)
		if adminctl.IsUsageError(err) {
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, adminctl.Usage())
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run() error {
	if err := config.LoadDotEnv(".env"); err != nil {
		return fmt.Errorf("read .env: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	return adminctl.Run(ctx, adminctl.Env{
		Args:         os.Args[1:],
		Getenv:       os.Getenv,
		ReadPassword: readPassword,
		Stdin:        os.Stdin,
		Stdout:       os.Stdout,
		OpenStore:    openStore,
		Hasher:       identity.DefaultHasher(),
		Now:          time.Now,
	})
}

// readPassword prompts without echo on a terminal and confirms the entry;
// otherwise it reads the first line of standard input (a pipe or a file).
func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return adminctl.FirstLine(os.Stdin)
	}
	fmt.Fprint(os.Stderr, "Password: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Repeat password: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(first) != string(second) {
		return "", errors.New("the two entries do not match")
	}
	return string(first), nil
}

// openStore connects to Postgres. The direct URL is preferred because this is
// a one-off administrative command, like migrations.
func openStore(ctx context.Context) (adminctl.Store, func(), error) {
	url := os.Getenv("DATABASE_DIRECT_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		return nil, nil, errors.New("neither DATABASE_DIRECT_URL nor DATABASE_URL is set: adminctl works on Postgres only " +
			"(in development without a database, sign in as the seeded admin@example.test instead)")
	}
	pool, err := database.NewPool(ctx, url, 2)
	if err != nil {
		return nil, nil, err
	}
	return identity.NewPostgresStore(pool), pool.Close, nil
}
