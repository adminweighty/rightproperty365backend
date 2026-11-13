// Package migrate is a deliberately small migration runner over embedded SQL
// files. It must be pointed at a DIRECT database connection (not the PgBouncer
// pooler) because it uses a session-level advisory lock and multi-statement
// scripts.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// advisoryLockKey serialises concurrent migration runs ("rp365mig" in hex-ish form).
const advisoryLockKey int64 = 0x7270333635_6D69

var fileRe = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.(up|down)\.sql$`)

// Migration is one versioned change with its up and down scripts.
type Migration struct {
	Version int
	Name    string
	Up      string
	Down    string
}

// Applied describes a row of schema_migrations.
type Applied struct {
	Version   int
	Name      string
	AppliedAt time.Time
}

// Status pairs a known migration with when it was applied (nil = pending).
type Status struct {
	Migration Migration
	AppliedAt *time.Time
}

// Load reads and validates all migrations from fsys, sorted by version.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	byVersion := map[int]*Migration{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileRe.FindStringSubmatch(e.Name())
		if m == nil {
			if len(e.Name()) > 4 && e.Name()[len(e.Name())-4:] == ".sql" {
				return nil, fmt.Errorf("migration file %q does not match NNNN_name.(up|down).sql", e.Name())
			}
			continue
		}
		version, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		mig, ok := byVersion[version]
		if !ok {
			mig = &Migration{Version: version, Name: m[2]}
			byVersion[version] = mig
		} else if mig.Name != m[2] {
			return nil, fmt.Errorf("version %04d has two names: %q and %q", version, mig.Name, m[2])
		}
		if m[3] == "up" {
			mig.Up = string(body)
		} else {
			mig.Down = string(body)
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.Up == "" || m.Down == "" {
			return nil, fmt.Errorf("migration %04d_%s needs both an .up.sql and a .down.sql file", m.Version, m.Name)
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Runner applies migrations over a single direct connection.
type Runner struct {
	conn       *pgx.Conn
	migrations []Migration
}

// NewRunner builds a runner. conn must be a direct (non-pooled) connection.
func NewRunner(conn *pgx.Conn, migrations []Migration) *Runner {
	return &Runner{conn: conn, migrations: migrations}
}

// withLock runs fn while holding the advisory lock and ensures the
// bookkeeping table exists.
func (r *Runner) withLock(ctx context.Context, fn func() error) error {
	if _, err := r.conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = r.conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	if _, err := r.conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    bigint PRIMARY KEY,
			name       text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	return fn()
}

func (r *Runner) applied(ctx context.Context) (map[int]Applied, error) {
	rows, err := r.conn.Query(ctx, `SELECT version, name, applied_at FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]Applied{}
	for rows.Next() {
		var a Applied
		var version int64
		if err := rows.Scan(&version, &a.Name, &a.AppliedAt); err != nil {
			return nil, err
		}
		a.Version = int(version)
		out[a.Version] = a
	}
	return out, rows.Err()
}

// Up applies every pending migration in order, each in its own transaction,
// and returns the ones it applied.
func (r *Runner) Up(ctx context.Context) ([]Migration, error) {
	var done []Migration
	err := r.withLock(ctx, func() error {
		applied, err := r.applied(ctx)
		if err != nil {
			return err
		}
		for _, m := range r.migrations {
			if _, ok := applied[m.Version]; ok {
				continue
			}
			if err := r.inTx(ctx, m.Up, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, int64(m.Version), m.Name)
				return err
			}); err != nil {
				return fmt.Errorf("apply %04d_%s: %w", m.Version, m.Name, err)
			}
			done = append(done, m)
		}
		return nil
	})
	return done, err
}

// Down reverts the most recently applied migration (exactly one). It returns
// nil when nothing is applied.
func (r *Runner) Down(ctx context.Context) (*Migration, error) {
	var reverted *Migration
	err := r.withLock(ctx, func() error {
		applied, err := r.applied(ctx)
		if err != nil {
			return err
		}
		latest := -1
		for v := range applied {
			latest = max(latest, v)
		}
		if latest < 0 {
			return nil
		}
		var target *Migration
		for i := range r.migrations {
			if r.migrations[i].Version == latest {
				target = &r.migrations[i]
			}
		}
		if target == nil {
			return fmt.Errorf("database is at version %04d which this binary does not know; refusing to guess", latest)
		}
		if err := r.inTx(ctx, target.Down, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, int64(target.Version))
			return err
		}); err != nil {
			return fmt.Errorf("revert %04d_%s: %w", target.Version, target.Name, err)
		}
		reverted = target
		return nil
	})
	return reverted, err
}

// Status reports every known migration and whether it is applied.
func (r *Runner) Status(ctx context.Context) ([]Status, error) {
	var out []Status
	err := r.withLock(ctx, func() error {
		applied, err := r.applied(ctx)
		if err != nil {
			return err
		}
		for _, m := range r.migrations {
			st := Status{Migration: m}
			if a, ok := applied[m.Version]; ok {
				at := a.AppliedAt.UTC()
				st.AppliedAt = &at
			}
			out = append(out, st)
		}
		return nil
	})
	return out, err
}

// inTx runs a (possibly multi-statement) script plus bookkeeping atomically.
func (r *Runner) inTx(ctx context.Context, script string, bookkeeping func(pgx.Tx) error) error {
	tx, err := r.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// No arguments => simple protocol, which allows multiple statements.
	if _, err := tx.Exec(ctx, script); err != nil {
		return err
	}
	if err := bookkeeping(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}
