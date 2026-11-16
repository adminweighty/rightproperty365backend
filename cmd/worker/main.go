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
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/app"
	"rightproperty365.com/backend/internal/modules/accounts"
	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/notifications"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/database"
	"rightproperty365.com/backend/internal/platform/elastic"
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

	// Onboarding emails read the application at send time. The worker never
	// opens a document, so it needs no file storage.
	handlers.SetOnboardingSource(app.OnboardingNotifications(onboarding.NewPostgresStore(pool), nil, reference.NewPostgresStore(pool), logger))

	// Appointment emails read the appointment at send time.
	handlers.SetAppointmentSource(appointments.NewPostgresStore(pool).Get)

	listingStore := listings.NewPostgresStore(pool)
	// Listing review emails (approved, changes requested, rejected) read the
	// listing at send time.
	handlers.SetListingSource(app.ListingRecord(listingStore))
	handlers.SetListingLookup(func(ctx context.Context, id uuid.UUID) (string, string, bool) {
		records, err := listingStore.ByIDs(ctx, []uuid.UUID{id})
		if err != nil || len(records) == 0 {
			return "", "", false
		}
		return records[0].Title, records[0].Reference, true
	})

	// Account suspension emails carry the administrator's reason, and the
	// subscription emails read the subscription, at send time.
	handlers.SetSuspensionReasons(app.SuspensionReasons(accounts.NewPostgresStore(pool)))
	subscriptionStore := subscriptions.NewPostgresStore(pool)
	handlers.SetSubscriptionSource(subscriptionStore.Subscription)

	dispatcher := outbox.NewDispatcher(outbox.NewPostgresQueue(pool), logger, outbox.Config{})
	handlers.Register(dispatcher)

	// A request's stage changes and resolved routing exceptions are recorded
	// in the outbox for later consumers (a customer notification, analytics).
	// Nothing sends an email for them yet, so they are acknowledged: without a
	// handler the dispatcher would retry and then dead-letter them.
	for _, t := range propertyrequests.WorkEventTypes() {
		dispatcher.Register(t, outbox.HandlerFunc(func(context.Context, outbox.Event) error { return nil }))
	}

	// Search index: listing events keep Elasticsearch in step. With the
	// database search driver there is no index, and the events are acknowledged.
	if cfg.SearchDriver == config.SearchDriverElasticsearch {
		es, err := elastic.New(elastic.Config{URL: cfg.ElasticsearchURL, APIKey: cfg.ElasticsearchAPIKey.Reveal(), Timeout: 30 * time.Second})
		if err != nil {
			return err
		}
		indexer := listings.NewIndexer(es, cfg.SearchIndexPrefix, listingStore, reference.NewService(reference.NewPostgresStore(pool), nil), logger)
		// A document's region is the region of its city (reference data).
		indexer.SetPlaces(app.Places(reference.NewPostgresStore(pool), markets.NewPostgresStore(pool)))
		indexer.Register(dispatcher)
		logger.Info("search driver selected: elasticsearch (listing events update the search index)",
			slog.String("search_driver", config.SearchDriverElasticsearch), slog.String("search_alias", listings.IndexAlias(cfg.SearchIndexPrefix)))
	} else {
		for _, t := range listings.IndexEventTypes() {
			dispatcher.Register(t, outbox.HandlerFunc(func(context.Context, outbox.Event) error { return nil }))
		}
		logger.Info("search driver selected: database (listing events are acknowledged; there is no search index)",
			slog.String("search_driver", config.SearchDriverDatabase))
	}

	// Agreements: expiry and the reminders that are due, every few minutes
	// (each reminder is recorded under a job key, so it is sent once). Their
	// emails carry signing links, so they are sent from here, not through
	// the outbox. A lease follows its agreement.
	agreementSvc := agreements.NewService(agreements.Deps{Store: agreements.NewPostgresStore(pool),
		Mailer: notifications.NewAccountMailer(mail.NewService(sender, mail.NewPostgresDeliveryLog(pool), logger), renderer),
		Logger: logger, PublicWebURL: cfg.PublicWebURL})
	leaseSvc := leases.NewService(leases.Deps{Store: leases.NewPostgresStore(pool), Logger: logger})
	go app.RunAgreementSweeps(ctx, agreementSvc, leaseSvc, logger)

	// Subscriptions: an ACTIVE subscription whose end has passed is recorded
	// EXPIRED (and its holder emailed) every few minutes.
	go app.RunSubscriptionSweeps(ctx, subscriptions.NewService(subscriptions.Deps{Store: subscriptionStore, Logger: logger}), logger)

	err = dispatcher.Run(ctx)
	logger.Info("worker stopped")
	return err
}
