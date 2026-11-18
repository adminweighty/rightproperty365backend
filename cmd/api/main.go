// Command api serves the RightProperty365 versioned REST API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/app"
	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/accounts"
	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/availability"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/health"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/notifications"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/reports"
	"rightproperty365.com/backend/internal/modules/saved"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/modules/teams"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/database"
	"rightproperty365.com/backend/internal/platform/elastic"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
	"rightproperty365.com/backend/internal/platform/storage"
)

const shutdownTimeout = 20 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
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
	logger := logging.New(os.Stdout, cfg.LogLevel, "api", cfg.Env)
	slog.SetDefault(logger)
	if cfg.AuthDemoCode.IsSet() {
		// config.Load refused it outside development and staging. Never the code itself.
		logger.Warn(config.DemoCodeWarning, slog.String("app_env", cfg.Env))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Email transport. This logs which mailer is in use before anything can
	// trigger an email. MAIL_DRIVER=log forces the development log mailer
	// even when RESEND_API_KEY is set.
	sender, err := notifications.NewSender(cfg, logger)
	if err != nil {
		return err
	}
	renderer, err := mail.NewRenderer(cfg.PublicWebURL)
	if err != nil {
		return err
	}
	var deliveryLog mail.DeliveryLog

	// Object storage: onboarding documents (private) and profile logos
	// (public, but only ever served by the API).
	files, err := storage.Open(cfg, logger)
	if err != nil {
		return err
	}
	// onboardingInfo loads the data behind an onboarding email. It is set
	// once the router's service exists; events only arrive after that.
	var onboardingStore onboarding.Store

	var (
		appointmentStore appointments.Store
		marketStore      markets.Store
		requestStore     propertyrequests.Store
		identityStore    identity.Store
		enquiryStore     businessenquiries.Store
		referenceStore   reference.Store
		listingStore     listings.Store
		// availabilityStore holds the calendars of the short stays.
		availabilityStore availability.Store
		listingSearch     listings.Search
		directoryStore    directory.Store
		healthH           *health.Handler
		// teamStore holds invitations and the team audit trail.
		teamStore teams.Store
		// leaseStore holds the lease templates and the leases.
		leaseStore leases.Store
		// agreementStore holds the agreements (signing).
		agreementStore agreements.Store
		// The administrators' area: accounts, subscriptions, reports and
		// the administrators' audit trail.
		accountStore      accounts.Store
		subscriptionStore subscriptions.Store
		reportStore       reports.Store
		adminAuditLog     adminaudit.Log
		// savedStore holds the listings customers saved.
		savedStore saved.Store
		// memSubscriptions is set without a database (its expiry sweep runs here).
		memSubscriptions *subscriptions.MemoryStore
	)
	if cfg.UseMemoryStore() {
		// config.Load guarantees this branch is only reachable in development.
		logger.Warn("DATABASE_URL is not set: using IN-MEMORY stores. Data is lost on restart and the worker cannot see it. Development only.")
		marketStore = markets.NewMemoryStore()
		memReference := devseed.NewMemoryReferenceStore()
		referenceStore = memReference
		// The public catalogue: the web's 18 sample listings and 29 profiles.
		memListings := listings.NewMemoryStore(reference.NewService(referenceStore, nil))
		memDirectory := directory.NewMemoryStore()
		devseed.SeedMemoryContent(memListings, memDirectory, time.Now())
		// ...whose logos are drawn at start-up and kept in the file storage.
		if err := devseed.SeedLogoFiles(ctx, files); err != nil {
			return fmt.Errorf("seed sample logos: %w", err)
		}
		listingStore, listingSearch, directoryStore = memListings, memListings, memDirectory
		memRequests := propertyrequests.NewMemoryStore()
		// Development sample data: 18 published listings, each with its
		// organisation and assigned agent (or its direct seller).
		devseed.SeedMemoryListings(memRequests)
		requestStore = memRequests
		// ...and the 29 directory profiles that can be sent an enquiry.
		memEnquiries := businessenquiries.NewMemoryStore()
		devseed.SeedMemoryProfiles(memEnquiries)
		enquiryStore = memEnquiries
		// Appointments: a request with a preferred slot stores its
		// appointment together with itself. A few sample ones are seeded.
		memAppointments := appointments.NewMemoryStore()
		memRequests.SetAppointmentWriter(memAppointments.CreateLinked)
		devseed.SeedMemoryAppointments(memAppointments, time.Now())
		appointmentStore = memAppointments
		// Availability of the sample short stays: a booked week, a blocked weekend.
		memAvailability := availability.NewMemoryStore()
		devseed.SeedMemoryAvailability(memAvailability, time.Now())
		availabilityStore = memAvailability
		healthH = health.NewHandler(health.StoreMemory, nil, logger)

		memIdentity := identity.NewMemoryStore()
		identityStore = memIdentity
		// Who advertises a listing — and whether they are verified — is read
		// from the profiles and accounts every time, never from a copy.
		memListings.SetAdvertisers(app.MemoryAdvertisers(memDirectory, memIdentity))
		memOnboarding := onboarding.NewMemoryStore(onboarding.MemoryDeps{
			Identity: memIdentity, Directory: memDirectory, Enquiries: memEnquiries,
			MarketID: func(code string) (uuid.UUID, bool) {
				all, err := marketStore.List(ctx)
				if err != nil {
					return uuid.Nil, false
				}
				for _, m := range all {
					if m.Code == code {
						return m.ID, true
					}
				}
				return uuid.Nil, false
			},
		})
		onboardingStore = memOnboarding
		// Teams: invitations, member changes and the audit trail, applied to
		// the stores above (memberships, profiles, requests, appointments).
		memTeams := teams.NewMemoryStore(teams.MemoryDeps{
			Identity: memIdentity, Directory: memDirectory, Enquiries: memEnquiries, Requests: memRequests,
			Appointments: memAppointments, Onboarding: memOnboarding,
			MarketID: func(code string) (uuid.UUID, bool) {
				all, err := marketStore.List(ctx)
				if err != nil {
					return uuid.Nil, false
				}
				for _, m := range all {
					if m.Code == code {
						return m.ID, true
					}
				}
				return uuid.Nil, false
			},
		})
		teamStore = memTeams
		memLeases := leases.NewMemoryStore()
		leaseStore = memLeases
		// The sample templates (approved) and a draft lease, with its PDF.
		if err := devseed.SeedMemoryLeases(ctx, memLeases, files, time.Now()); err != nil {
			return fmt.Errorf("seed sample leases: %w", err)
		}
		// Saved listings: the sample tenant has two, one of them expired.
		memSaved := saved.NewMemoryStore()
		devseed.SeedMemorySaved(memSaved, time.Now())
		savedStore = memSaved
		memAgreements := agreements.NewMemoryStore()
		agreementStore = memAgreements
		if err := devseed.SeedMemoryAgreements(ctx, memAgreements, memLeases, files, time.Now()); err != nil {
			return fmt.Errorf("seed sample agreements: %w", err)
		}

		// The administrators' area: one audit log for every store that
		// records administrators' actions.
		memAudit := &adminaudit.MemoryLog{}
		adminAuditLog = memAudit
		memReference.SetAuditLog(memAudit)
		memSubscriptions = subscriptions.NewMemoryStore(memAudit)
		memSubscriptions.SetActiveListings(app.MemoryActiveListings(memListings))
		subscriptionStore = memSubscriptions
		accountStore = accounts.NewMemoryStore(accounts.MemoryDeps{Identity: memIdentity, Listings: memListings, Teams: memTeams,
			Directory: memDirectory, Audit: memAudit, CancelSubscriptions: memSubscriptions.CancelForDeletedUser,
			DeleteSignature: memAgreements.DeleteSavedSignature, DeleteSavedListings: memSaved.DeleteUser})
		reportStore = app.MemoryReports(app.MemoryReportStores{Identity: memIdentity, Onboarding: memOnboarding, Listings: memListings,
			Requests: memRequests, Enquiries: memEnquiries, Leases: memLeases, Agreements: memAgreements, Subscriptions: memSubscriptions,
			Markets: marketStore})

		// Without a database there is no outbox table for the worker to poll,
		// so notification emails are handled in this process, straight from
		// the in-memory stores. Development only: no retries, nothing durable.
		memDeliveries := mail.NewMemoryDeliveryLog()
		deliveryLog = memDeliveries
		handlers := notifications.NewHandlers(mail.NewService(sender, memDeliveries, logger), renderer,
			notifications.NewMemorySource(memIdentity, memRequests, memEnquiries), logger)
		handlers.SetListingLookup(func(ctx context.Context, id uuid.UUID) (string, string, bool) {
			records, err := memListings.ByIDs(ctx, []uuid.UUID{id})
			if err != nil || len(records) == 0 {
				return "", "", false
			}
			return records[0].Title, records[0].Reference, true
		})
		handlers.SetRoleLabels(func(ctx context.Context) (map[string]string, error) {
			return reference.NewService(referenceStore, nil).Labels(ctx, reference.SetAccountRoles)
		})
		handlers.SetOnboardingSource(app.OnboardingNotifications(memOnboarding, files, referenceStore, logger))
		handlers.SetAppointmentSource(memAppointments.Get)
		handlers.SetListingSource(app.ListingRecord(memListings))
		handlers.SetSuspensionReasons(app.SuspensionReasons(accountStore))
		handlers.SetSubscriptionSource(memSubscriptions.Subscription)
		sink := func(ev outbox.Event) {
			go func() {
				hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				if err := handlers.Handle(hctx, ev); err != nil {
					logger.Error("in-process notification failed (development; not retried)",
						slog.String("event_type", ev.Type), slog.String("error", err.Error()))
				}
			}()
		}
		memIdentity.SetOutboxSink(sink)
		memRequests.SetOutboxSink(sink)
		memEnquiries.SetOutboxSink(sink)
		memOnboarding.SetOutboxSink(sink)
		memAppointments.SetOutboxSink(sink)
		memSubscriptions.SetOutboxSink(sink)
		// The listing store also writes search-index events; without a worker
		// only the review emails are handled.
		memListings.SetOutboxSink(func(ev outbox.Event) {
			if slices.Contains(listings.ReviewEventTypes(), ev.Type) {
				sink(ev)
			}
		})
		if cfg.DevSeedPassword == "" {
			logger.Warn("DEV_SEED_PASSWORD is not set: development seed users were NOT created (copy .env.example to .env to get them)")
		} else {
			if err := devseed.SeedMemoryUsers(ctx, memIdentity, identity.DefaultHasher(), cfg.DevSeedPassword, time.Now()); err != nil {
				return fmt.Errorf("seed development users: %w", err)
			}
			if err := devseed.SeedMemoryOnboarding(ctx, memIdentity, memOnboarding, files, identity.DefaultHasher(), cfg.DevSeedPassword, time.Now()); err != nil {
				return fmt.Errorf("seed onboarding demo accounts: %w", err)
			}
			if err := devseed.SeedMemoryTeams(ctx, memIdentity, memOnboarding, memTeams, files, identity.DefaultHasher(), cfg.DevSeedPassword, time.Now()); err != nil {
				return fmt.Errorf("seed the sample team's invitation and join request: %w", err)
			}
			if err := devseed.SeedMemorySubscriptions(ctx, memSubscriptions, time.Now()); err != nil {
				return fmt.Errorf("seed sample subscription plans: %w", err)
			}
			logger.Info("development seed users created (password from DEV_SEED_PASSWORD)",
				slog.Any("emails", append(devseed.Emails(), devseed.PendingProviderEmail, devseed.PendingAgentEmail, devseed.JoiningAgentEmail)))
		}
	} else {
		if warning := config.DatabaseTLSWarning(cfg.DatabaseURL); warning != "" {
			logger.Warn("DATABASE_URL: " + warning)
		}
		pool, err := database.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBPoolMode)
		if err != nil {
			return err
		}
		defer pool.Close()
		logger.Info("database pool ready", slog.Int("max_conns", int(cfg.DBMaxConns)), slog.String("pool_mode", cfg.DBPoolMode))
		marketStore = markets.NewPostgresStore(pool)
		pgRequests := propertyrequests.NewPostgresStore(pool)
		// The request and its appointment commit in one transaction.
		pgRequests.SetAppointmentWriter(appointments.InsertLinkedTx)
		requestStore = pgRequests
		appointmentStore = appointments.NewPostgresStore(pool)
		availabilityStore = availability.NewPostgresStore(pool)
		identityStore = identity.NewPostgresStore(pool)
		enquiryStore = businessenquiries.NewPostgresStore(pool)
		referenceStore = reference.NewPostgresStore(pool)
		pgListings := listings.NewPostgresStore(pool)
		listingStore, listingSearch, directoryStore = pgListings, pgListings, directory.NewPostgresStore(pool)
		healthH = health.NewHandler(health.StorePostgres, pool.Ping, logger)
		deliveryLog = mail.NewPostgresDeliveryLog(pool)
		onboardingStore = onboarding.NewPostgresStore(pool)
		teamStore = teams.NewPostgresStore(pool)
		leaseStore = leases.NewPostgresStore(pool)
		agreementStore = agreements.NewPostgresStore(pool)
		accountStore = accounts.NewPostgresStore(pool)
		subscriptionStore = subscriptions.NewPostgresStore(pool)
		reportStore = reports.NewPostgresStore(pool)
		savedStore = saved.NewPostgresStore(pool)
		adminAuditLog = adminaudit.PostgresLog{Q: pool}
	}

	// Search engine. Elasticsearch only proposes listing IDs: every hit is
	// re-read from the source of truth, and if Elasticsearch fails the same
	// request is answered by the database adapter.
	switch cfg.SearchDriver {
	case config.SearchDriverElasticsearch:
		es, err := elastic.New(elastic.Config{URL: cfg.ElasticsearchURL, APIKey: cfg.ElasticsearchAPIKey.Reveal(), Timeout: cfg.SearchTimeout})
		if err != nil {
			return err
		}
		listingSearch = listings.NewFallbackSearch(listings.NewElasticSearch(es, cfg.SearchIndexPrefix, cfg.SearchTimeout), listingSearch, logger)
		logger.Info("search driver selected: elasticsearch (falls back to the database adapter on error)",
			slog.String("search_driver", config.SearchDriverElasticsearch), slog.String("search_alias", listings.IndexAlias(cfg.SearchIndexPrefix)),
			slog.Duration("search_timeout", cfg.SearchTimeout), slog.Bool("api_key_set", cfg.ElasticsearchAPIKey.IsSet()))
	default:
		store := "postgres"
		if cfg.UseMemoryStore() {
			store = "memory"
		}
		logger.Info("search driver selected: database ("+store+" adapter; Elasticsearch is not used)",
			slog.String("search_driver", config.SearchDriverDatabase), slog.String("search_store", store))
	}

	// Verification and password-reset emails are sent from the request path
	// (their links are credentials and never go through the outbox).
	mailer := notifications.NewAccountMailer(mail.NewService(sender, deliveryLog, logger), renderer)

	handler, err := app.NewRouter(app.Deps{
		Config:        cfg,
		Logger:        logger,
		Health:        healthH,
		Markets:       marketStore,
		Requests:      requestStore,
		Enquiries:     enquiryStore,
		Reference:     referenceStore,
		Listings:      listingStore,
		ListingSearch: listingSearch,
		Directory:     directoryStore,
		Identity:      identityStore,
		Mailer:        mailer,
		Onboarding:    onboardingStore,
		Appointments:  appointmentStore,
		Availability:  availabilityStore,
		Teams:         teamStore,
		Leases:        leaseStore,
		Agreements:    agreementStore,
		Files:         files,
		Accounts:      accountStore,
		Subscriptions: subscriptionStore,
		Reports:       reportStore,
		AdminAudit:    adminAuditLog,
		Saved:         savedStore,
	})
	if err != nil {
		return err
	}

	// Without a database there is no worker: this process expires
	// agreements and sends their reminders (development only).
	if cfg.UseMemoryStore() {
		go app.RunAgreementSweeps(ctx, agreements.NewService(agreements.Deps{Store: agreementStore, Files: files, Mailer: mailer, Logger: logger,
			PublicWebURL: cfg.PublicWebURL}), nil, logger)
		go app.RunSubscriptionSweeps(ctx, subscriptions.NewService(subscriptions.Deps{Store: memSubscriptions, Logger: logger}), logger)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("api listening", slog.String("addr", cfg.HTTPAddr), slog.Any("cors_allowed_origins", cfg.CORSAllowedOrigins))
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutdown signal received, draining connections")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("api stopped")
	return nil
}
