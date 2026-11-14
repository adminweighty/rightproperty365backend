// Command worker runs background processing: currently the transactional
// outbox dispatcher that will drive notifications.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"rightproperty365.com/backend/internal/modules/notifications"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/database"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := config.LoadDotEnv(".env"); err != nil {
		return fmt.Errorf("read .env: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(os.Stdout, cfg.LogLevel, "worker", cfg.Env)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.UseMemoryStore() {
		// config.Load only allows this in development. The API's in-memory
		// outbox lives in the API process, so there is nothing to consume.
		logger.Warn("DATABASE_URL is not set: worker is idle (development only). Set DATABASE_URL to process the outbox.")
		<-ctx.Done()
		logger.Info("worker stopped")
		return nil
	}

	if warning := config.DatabaseTLSWarning(cfg.DatabaseURL); warning != "" {
		logger.Warn("DATABASE_URL: " + warning)
	}
	pool, err := database.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBPoolMode)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Notification emails: one per recipient per event, sent once (see
	// mail.Service). The line naming the mailer is logged before any event is
	// processed.
	sender, err := notifications.NewSender(cfg, logger)
	if err != nil {
		return err
	}
	renderer, err := mail.NewRenderer(cfg.PublicWebURL)
	if err != nil {
		return err
	}
	handlers := notifications.NewHandlers(
		mail.NewService(sender, mail.NewPostgresDeliveryLog(pool), logger),
		renderer, notifications.NewPostgresSource(pool), logger)

	handlers.SetRoleLabels(func(ctx context.Context) (map[string]string, error) {
		return reference.NewService(reference.NewPostgresStore(pool), nil).Labels(ctx, reference.SetAccountRoles)
	})

	dispatcher := outbox.NewDispatcher(outbox.NewPostgresQueue(pool), logger, outbox.Config{})
	handlers.Register(dispatcher)

	err = dispatcher.Run(ctx)
	logger.Info("worker stopped")
	return err
}
