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
	"syscall"
	"time"

	"rightproperty365.com/backend/internal/app"
	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/health"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/database"
	"rightproperty365.com/backend/internal/platform/logging"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		marketStore   markets.Store
		requestStore  propertyrequests.Store
		identityStore identity.Store
		enquiryStore  businessenquiries.Store
		healthH       *health.Handler
	)
	if cfg.UseMemoryStore() {
		// config.Load guarantees this branch is only reachable in development.
		logger.Warn("DATABASE_URL is not set: using IN-MEMORY stores. Data is lost on restart and the worker cannot see it. Development only.")
		marketStore = markets.NewMemoryStore()
		memRequests := propertyrequests.NewMemoryStore()
		// Development sample data: 18 published listings, each with its
		// organisation and assigned agent (or its direct seller).
		devseed.SeedMemoryListings(memRequests)
		requestStore = memRequests
		// ...and the 29 directory profiles that can be sent an enquiry.
		memEnquiries := businessenquiries.NewMemoryStore()
		devseed.SeedMemoryProfiles(memEnquiries)
		enquiryStore = memEnquiries
		healthH = health.NewHandler(health.StoreMemory, nil, logger)

		memIdentity := identity.NewMemoryStore()
		identityStore = memIdentity
		if cfg.DevSeedPassword == "" {
			logger.Warn("DEV_SEED_PASSWORD is not set: development seed users were NOT created (copy .env.example to .env to get them)")
		} else {
			if err := devseed.SeedMemoryUsers(ctx, memIdentity, identity.DefaultHasher(), cfg.DevSeedPassword, time.Now()); err != nil {
				return fmt.Errorf("seed development users: %w", err)
			}
			logger.Info("development seed users created (password from DEV_SEED_PASSWORD)", slog.Any("emails", devseed.Emails()))
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
		requestStore = propertyrequests.NewPostgresStore(pool)
		identityStore = identity.NewPostgresStore(pool)
		enquiryStore = businessenquiries.NewPostgresStore(pool)
		healthH = health.NewHandler(health.StorePostgres, pool.Ping, logger)
	}

	// Email: development logs the links; elsewhere nothing is sent until a
	// provider is chosen (the mailer then warns without logging any token).
	var mailer identity.Mailer
	if cfg.Env == config.EnvDevelopment {
		mailer = identity.NewDevMailer(logger)
	} else {
		logger.Warn("no email provider is configured: verification and password-reset emails will NOT be sent")
		mailer = identity.NewUnconfiguredMailer(logger)
	}

	handler, err := app.NewRouter(app.Deps{
		Config:    cfg,
		Logger:    logger,
		Health:    healthH,
		Markets:   marketStore,
		Requests:  requestStore,
		Enquiries: enquiryStore,
		Identity:  identityStore,
		Mailer:    mailer,
	})
	if err != nil {
		return err
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
