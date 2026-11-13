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

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/database"
	"rightproperty365.com/backend/internal/platform/logging"
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

	dispatcher := outbox.NewDispatcher(outbox.NewPostgresQueue(pool), logger, outbox.Config{})
	dispatcher.Register(propertyrequests.EventPropertyRequestCreated, propertyRequestCreated(logger))
	dispatcher.Register(businessenquiries.EventBusinessEnquiryCreated, businessEnquiryCreated(logger))

	err = dispatcher.Run(ctx)
	logger.Info("worker stopped")
	return err
}

// propertyRequestCreated is the placeholder consumer for new property
// requests. It only logs. When real notifications (email/WhatsApp/in-app) are
// added here they MUST be idempotent: delivery is at-least-once, so key every
// side effect on ev.ID.
func propertyRequestCreated(logger *slog.Logger) outbox.Handler {
	return outbox.HandlerFunc(func(ctx context.Context, ev outbox.Event) error {
		logger.InfoContext(ctx, "PROPERTY_REQUEST_CREATED",
			slog.String("event_id", ev.ID.String()),
			slog.String("property_request_id", ev.AggregateID.String()),
			slog.Int("attempt", ev.Attempts),
		)
		return nil
	})
}

// businessEnquiryCreated is the placeholder consumer for new business
// enquiries. It only logs identifiers; the payload carries no personal data.
// The same idempotency rule applies when real notifications are added.
func businessEnquiryCreated(logger *slog.Logger) outbox.Handler {
	return outbox.HandlerFunc(func(ctx context.Context, ev outbox.Event) error {
		logger.InfoContext(ctx, "BUSINESS_ENQUIRY_CREATED",
			slog.String("event_id", ev.ID.String()),
			slog.String("business_enquiry_id", ev.AggregateID.String()),
			slog.Int("attempt", ev.Attempts),
		)
		return nil
	})
}
