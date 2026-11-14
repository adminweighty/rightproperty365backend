// Command reindex rebuilds the Elasticsearch listings index from the database.
//
// It creates a new index (<prefix>-listings-<timestamp>) with the explicit
// mapping, indexes every PUBLISHED, unexpired listing into it, moves the alias
// <prefix>-listings-current to it in one atomic action, and only then deletes
// the index the alias pointed to before. Searches keep working throughout. If
// anything fails before the swap, the alias is untouched and the half-built
// index is removed.
//
// Run it once after creating a cluster, after a mapping change, or to repair
// an index. It refuses to run without DATABASE_URL and ELASTICSEARCH_URL.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rightproperty365.com/backend/internal/app"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/database"
	"rightproperty365.com/backend/internal/platform/elastic"
	"rightproperty365.com/backend/internal/platform/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "reindex:", err)
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
	if cfg.DatabaseURL == "" {
		return errors.New("DATABASE_URL is not set: the index is built from the database, which is the source of truth")
	}
	if cfg.ElasticsearchURL == "" {
		return errors.New("ELASTICSEARCH_URL is not set: there is no cluster to build an index in")
	}
	logger := logging.New(os.Stdout, cfg.LogLevel, "reindex", cfg.Env)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	url, poolMode, err := config.ToolDatabase(os.Getenv)
	if err != nil {
		return err
	}
	pool, err := database.NewPool(ctx, url, 2, poolMode)
	if err != nil {
		return err
	}
	defer pool.Close()

	es, err := elastic.New(elastic.Config{URL: cfg.ElasticsearchURL, APIKey: cfg.ElasticsearchAPIKey.Reveal(), Timeout: time.Minute})
	if err != nil {
		return err
	}
	indexer := listings.NewIndexer(es, cfg.SearchIndexPrefix, listings.NewPostgresStore(pool),
		reference.NewService(reference.NewPostgresStore(pool), nil), logger)
	// A document's region is the region of its city (reference data).
	indexer.SetPlaces(app.Places(reference.NewPostgresStore(pool), markets.NewPostgresStore(pool)))
	name, count, err := indexer.Reindex(ctx)
	if err != nil {
		return err
	}
	logger.Info("search index rebuilt and alias swapped", slog.String("index", name),
		slog.String("alias", listings.IndexAlias(cfg.SearchIndexPrefix)), slog.Int("listings", count))
	return nil
}
