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

	"github.com/go-chi/chi/v5"

	"rightproperty365.com/backend/internal/modules/health"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/database"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/logging"
)

const (
	maxBodyBytes    = 64 << 10 // 64 KB
	requestTimeout  = 15 * time.Second
	shutdownTimeout = 20 * time.Second

	// Per-IP limit on POST /v1/property-requests: bursts of 10, refilled at
	// 10 per minute. In-memory and per-process — see httpx.RateLimiter; it must
	// become a shared limiter before the API is autoscaled.
	propertyRequestsPerMinute = 10
	propertyRequestsBurst     = 10
)

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
		marketStore  markets.Store
		requestStore propertyrequests.Store
		healthH      *health.Handler
	)
	if cfg.UseMemoryStore() {
		// config.Load guarantees this branch is only reachable in development.
		logger.Warn("DATABASE_URL is not set: using IN-MEMORY stores. Data is lost on restart and the worker cannot see it. Development only.")
		marketStore = markets.NewMemoryStore()
		requestStore = propertyrequests.NewMemoryStore()
		healthH = health.NewHandler(health.StoreMemory, nil, logger)
	} else {
		pool, err := database.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
		if err != nil {
			return err
		}
		defer pool.Close()
		logger.Info("database pool ready", slog.Int("max_conns", int(cfg.DBMaxConns)))
		marketStore = markets.NewPostgresStore(pool)
		requestStore = propertyrequests.NewPostgresStore(pool)
		healthH = health.NewHandler(health.StorePostgres, pool.Ping, logger)
	}

	marketsH := markets.NewHandler(marketStore, logger)
	requestsH := propertyrequests.NewHandler(propertyrequests.NewService(requestStore), logger)
	requestLimiter := httpx.NewRateLimiter(propertyRequestsPerMinute, propertyRequestsBurst)

	r := chi.NewRouter()
	r.Use(
		httpx.RequestID,
		httpx.AccessLog(logger),
		httpx.Recover(logger),
		httpx.SecurityHeaders(cfg.Env != config.EnvDevelopment),
		httpx.CORS(cfg.CORSAllowedOrigins),
		httpx.BodyLimit(maxBodyBytes),
		httpx.Timeout(requestTimeout),
	)
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)

	r.Get("/healthz", healthH.Live)
	r.Get("/readyz", healthH.Ready)
	r.Route("/v1", func(r chi.Router) {
		r.Get("/markets", marketsH.List)
		r.With(requestLimiter.Middleware).Post("/property-requests", requestsH.Create)
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
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
