// Command worker runs background processing: the transactional outbox
// dispatcher (notifications, the search index, server-side analytics events)
// and the periodic jobs (agreements, subscriptions, analytics rollups and
// retention).
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
	"rightproperty365.com/backend/internal/modules/analytics"
	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/inapp"
	"rightproperty365.com/backend/internal/modules/inspections"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/notifications"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/packages"
	"rightproperty365.com/backend/internal/modules/payments"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/reviews"
	"rightproperty365.com/backend/internal/modules/saved"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/modules/venues"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/database"
	"rightproperty365.com/backend/internal/platform/elastic"
	"rightproperty365.com/backend/internal/platform/ics"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/malware"
	"rightproperty365.com/backend/internal/platform/outbox"
	"rightproperty365.com/backend/internal/platform/paygateway"
	"rightproperty365.com/backend/internal/platform/push"
	"rightproperty365.com/backend/internal/platform/storage"
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
	appointmentStore := appointments.NewPostgresStore(pool)
	handlers.SetAppointmentSource(appointmentStore.Get)
	// Calendar invitations (API 0.39.0) on the appointment and venue booking
	// emails, rebuilt at send time from the event and its history.
	handlers.SetCalendar(ics.Organizer(cfg.EmailFrom), appointmentStore.Timeline)

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

	// Questionnaires and applications: the emails read the application at
	// send time; the retention job erases answers and files past their
	// form's retention period and removes deleted files' objects, so the
	// worker opens the object storage (it never serves a file).
	files, err := storage.Open(cfg, logger)
	if err != nil {
		return err
	}
	// Malware scanning (API 0.41.0): with MALWARE_SCANNER=clamav this
	// process scans the quarantined documents (PENDING scan records) and
	// retries them with backoff while clamd is unreachable; an infected file
	// is deleted and its uploader told in-app. Deletions go through the
	// guard so a deleted file's scan record goes with it.
	scanner := malware.Open(ctx, cfg, logger)
	scanStore := malware.NewPostgresStore(pool)
	guard := malware.NewGuard(files, scanner, logger)
	guard.SetUnscannedPrefixes(app.UnscannedKeyPrefixes()...)
	guard.SetStore(scanStore)
	var scannedFiles storage.Storage = guard
	applicationStore := applications.NewPostgresStore(pool)
	handlers.SetApplicationSource(app.ApplicationNotices(applicationStore, listingStore, reference.NewPostgresStore(pool),
		identity.NewPostgresStore(pool), logger))

	// Hourly venue bookings: the emails read the booking at send time.
	handlers.SetVenueSource(app.VenueNotices(venues.NewPostgresStore(pool), listingStore, markets.NewPostgresStore(pool),
		reference.NewPostgresStore(pool), logger))

	// Maintenance requests (API 0.36.2): the emails read the request, its
	// book and its tenancy's tenants with a verified account at send time.
	handlers.SetMaintenanceSource(app.MaintenanceNotices(maintenance.NewPostgresStore(pool), tenancies.NewPostgresStore(pool),
		crm.NewPostgresStore(pool), identity.NewPostgresStore(pool)))

	// Inspections (API 0.36.3): the emails to the tenants read the
	// inspection, its market's time zone and the tenancy's tenants with a
	// verified account at send time.
	handlers.SetInspectionSource(app.InspectionNotices(inspections.NewPostgresStore(pool), tenancies.NewPostgresStore(pool),
		crm.NewPostgresStore(pool), identity.NewPostgresStore(pool), markets.NewPostgresStore(pool)))

	// Reviews (API 0.36.4): the moderation emails read the review (its
	// provider and reviewer) at send time.
	handlers.SetReviewSource(reviews.Notices(reviews.NewPostgresStore(pool)))

	// The notification centre (API 0.35.0): every notification email's
	// recipient who has an account also gets one in-app notification per
	// event (unique per event and recipient), pushed to their opted-in
	// devices by the push sender below, after the commit. The emails of
	// non-essential types follow the recipient's preferences. Without FCM
	// credentials push is disabled (one log line) and the inbox still fills.
	pusher := push.Open(cfg, logger)
	pushProvider := ""
	if pusher != nil {
		pushProvider = pusher.Name()
	}
	inboxStore := inapp.NewPostgresStore(pool)
	inboxSvc := inapp.NewService(inboxStore, pushProvider, logger, nil)
	handlers.SetInbox(inboxSvc)
	handlers.SetPlaceLookup(app.ListingPlace(listingStore, reference.NewPostgresStore(pool), logger))
	if pusher != nil {
		go inapp.NewSender(inboxStore, pusher, logger, nil).Run(ctx)
	}

	if malware.Enabled(scanner) {
		go malware.NewWorker(scanStore, files, scanner, app.FileScanNotifier(inboxSvc), logger, guard.Metrics()).Run(ctx, nil)
		logger.Info("file scan job scheduled", slog.Duration("interval", malware.WorkerPollInterval))
	} else {
		logger.Info("file scan job not scheduled (MALWARE_SCANNER=none): a document quarantined while scanning was on stays quarantined")
	}

	dispatcher := outbox.NewDispatcher(outbox.NewPostgresQueue(pool), logger, outbox.Config{})
	handlers.Register(dispatcher)

	// A request's stage changes and resolved routing exceptions are recorded
	// in the outbox for later consumers (a customer notification, analytics).
	// Nothing sends an email for them yet, so they are acknowledged: without a
	// handler the dispatcher would retry and then dead-letter them.
	for _, t := range propertyrequests.WorkEventTypes() {
		dispatcher.Register(t, outbox.HandlerFunc(func(context.Context, outbox.Event) error { return nil }))
	}

	// Saves and removals of saved listings: nothing is sent for them, so they
	// are acknowledged here; analytics counts the saves (below).
	for _, t := range saved.EventTypes() {
		dispatcher.Register(t, outbox.HandlerFunc(func(context.Context, outbox.Event) error { return nil }))
	}

	// Listing analytics: saves, enquiries (property requests), viewing
	// requests, confirmed appointments and relists are recorded as
	// server-side events, after any notification handler of the same event
	// (idempotent: the analytics event_id is derived from the outbox event's).
	// Every 15 minutes the rollups of the last 3 market-local days are
	// recomputed and raw events older than ANALYTICS_RAW_RETENTION_DAYS deleted.
	analyticsSvc := app.AnalyticsService(analytics.NewPostgresStore(pool), listingStore, markets.NewPostgresStore(pool),
		cfg.AnalyticsRawRetentionDays, logger, nil)
	analytics.NewRecorder(analyticsSvc, app.AppointmentListing(appointments.NewPostgresStore(pool))).Register(dispatcher)
	go app.RunAnalyticsJobs(ctx, analyticsSvc, logger)
	logger.Info("analytics jobs scheduled", slog.Duration("interval", app.AnalyticsJobInterval),
		slog.Int("raw_retention_days", cfg.AnalyticsRawRetentionDays))

	// The agent CRM: a new property request, business enquiry or appointment
	// is linked to the contacts of its organisation that share its email or
	// phone (after any other handler of the same event; idempotent), and
	// every minute the reminders that fell due get a CONTACT_REMINDER_DUE
	// event (acknowledged: nothing is sent for it yet).
	crmSvc := app.CRMService(crm.NewPostgresStore(pool), crm.NewPostgresItems(pool), listingStore, nil,
		reference.NewPostgresStore(pool), nil, logger, nil)
	crmSvc.Register(dispatcher)
	go app.RunCRMJobs(ctx, crmSvc)

	// Application retention, every hour: final applications past their
	// form version's retention_days keep a tombstone only (each erasure is
	// logged); then the purge of deleted files' objects.
	applicationSvc := app.ApplicationsService(applicationStore, scannedFiles,
		listings.NewService(listingStore, listingStore, reference.NewService(reference.NewPostgresStore(pool), nil)), listingStore,
		identity.NewPostgresStore(pool), logger, nil)
	go app.RunApplicationJobs(ctx, applicationSvc)
	logger.Info("application retention job scheduled", slog.Duration("interval", applications.RetentionInterval))

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

	// Online payment (API 0.42.0): open checkouts past their expiry become
	// EXPIRED, older ones are checked with the provider, and a SUCCEEDED
	// payment whose subscription still waits is activated (granting its
	// plan's listing package, as the API does). With PAYMENT_GATEWAY=none only
	// the expiry and follow-ups of past payments run.
	app.LinkSubscriptionPackages(subscriptionStore, packages.NewPostgresStore(pool))
	gateway, err := paygateway.Open(cfg, logger, nil)
	if err != nil {
		return err
	}
	go app.RunPaymentReconciliation(ctx, payments.NewService(payments.Deps{Store: payments.NewPostgresStore(pool), Gateway: gateway,
		Subscriptions: subscriptions.NewService(subscriptions.Deps{Store: subscriptionStore, Logger: logger}), PublicWebURL: cfg.PublicWebURL,
		ReconcileAfter: cfg.PaymentReconcileAfter, Logger: logger}), logger)
	logger.Info("payment reconciliation job scheduled", slog.Duration("interval", app.PaymentReconcileInterval))

	// Rent schedules (API 0.36.1): every ACTIVE tenancy's rule is re-run daily,
	// so a rolling schedule gains the next month's due when the month turns.
	go app.RunRentScheduleSweeps(ctx, tenancies.NewService(tenancies.Deps{Store: tenancies.NewPostgresStore(pool), Logger: logger}), logger)
	logger.Info("rent schedule job scheduled", slog.Duration("interval", app.RentScheduleSweepInterval))

	err = dispatcher.Run(ctx)
	logger.Info("worker stopped")
	return err
}
