// Package database creates the runtime pgx connection pool.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/config"
)

// NewPool connects to PostgreSQL.
//
// poolMode says what the URL points at:
//
//   - config.PoolModeSession (the default): PostgreSQL itself. pgx keeps its
//     normal behaviour, including the per-connection prepared-statement cache.
//   - config.PoolModeTransaction: PgBouncer (or a similar pooler) in
//     transaction-pooling mode. Consecutive statements may then run on
//     different server connections, so named prepared statements cannot be
//     relied on; QueryExecModeCacheDescribe keeps real parameter binding but
//     never creates them.
//
// Whatever the mode, the application code avoids session state (no SET, no
// LISTEN/NOTIFY, no session advisory locks outside the migration runner), so
// switching modes needs no code change.
//
// TLS is decided entirely by the connection string (sslmode); nothing here
// overrides it.
func NewPool(ctx context.Context, databaseURL string, maxConns int32, poolMode string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// pgx redacts the password in parse errors, but do not wrap the raw URL ourselves.
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second
	switch poolMode {
	case config.PoolModeSession:
		// pgx default (QueryExecModeCacheStatement): leave it alone.
	case config.PoolModeTransaction:
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	default:
		return nil, fmt.Errorf("unknown database pool mode %q", poolMode)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
