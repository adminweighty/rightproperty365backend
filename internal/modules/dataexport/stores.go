package dataexport

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore is the in-memory Store (development and tests).
type MemoryStore struct {
	mu     sync.Mutex
	rows   []Export
	outbox []outbox.Event
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Outbox returns the events written.
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]outbox.Event{}, s.outbox...)
}

func (s *MemoryStore) latestLocked(userID uuid.UUID) (int, bool) {
	best := -1
	for i, e := range s.rows {
		if e.UserID == userID && (best < 0 || e.RequestedAt.After(s.rows[best].RequestedAt)) {
			best = i
		}
	}
	return best, best >= 0
}

// Latest implements Store.
func (s *MemoryStore) Latest(_ context.Context, userID uuid.UUID) (Export, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.latestLocked(userID)
	if !ok {
		return Export{}, false, nil
	}
	return s.rows[i], true, nil
}

// Create implements Store.
func (s *MemoryStore) Create(_ context.Context, e Export, since time.Time) (Export, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.latestLocked(e.UserID); ok && s.rows[i].Status != StatusFailed && !s.rows[i].RequestedAt.Before(since) {
		return s.rows[i], false, nil
	}
	s.rows = append(s.rows, e)
	return e, true, nil
}

// Claim implements Store.
func (s *MemoryStore) Claim(_ context.Context, now, leaseUntil time.Time, limit int) ([]Export, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Export{}
	idx := make([]int, 0)
	for i, e := range s.rows {
		if e.Status == StatusRequested || (e.Status == StatusBuilding && e.ReadyAt != nil && e.ReadyAt.Before(now)) {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool { return s.rows[idx[a]].RequestedAt.Before(s.rows[idx[b]].RequestedAt) })
	for _, i := range idx {
		if len(out) == limit {
			break
		}
		lease := leaseUntil
		// While BUILDING, ReadyAt holds the lease end.
		s.rows[i].Status, s.rows[i].ReadyAt = StatusBuilding, &lease
		s.rows[i].Attempts++
		out = append(out, s.rows[i])
	}
	return out, nil
}

func (s *MemoryStore) find(id uuid.UUID) int {
	for i := range s.rows {
		if s.rows[i].ID == id {
			return i
		}
	}
	return -1
}

// Ready implements Store.
func (s *MemoryStore) Ready(_ context.Context, id uuid.UUID, key string, size int64, now, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.find(id)
	if i < 0 {
		return errors.New("export not found")
	}
	ready, exp := now, expires
	s.rows[i].Status, s.rows[i].ObjectKey, s.rows[i].SizeBytes, s.rows[i].ReadyAt, s.rows[i].ExpiresAt = StatusReady, key, size, &ready, &exp
	payload, _ := json.Marshal(map[string]any{"export_id": id, "user_id": s.rows[i].UserID, "expires_at": exp})
	s.outbox = append(s.outbox, outbox.Event{ID: uuid.New(), Type: EventReady, AggregateType: AggregateExport, AggregateID: id, Payload: payload, Attempts: 1, CreatedAt: now})
	return nil
}

// Fail implements Store.
func (s *MemoryStore) Fail(_ context.Context, id uuid.UUID, final bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.find(id); i >= 0 {
		s.rows[i].Status, s.rows[i].ReadyAt = StatusRequested, nil
		if final {
			s.rows[i].Status = StatusFailed
		}
	}
	return nil
}

// DueForExpiry implements Store.
func (s *MemoryStore) DueForExpiry(_ context.Context, now time.Time, limit int) ([]Export, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Export{}
	for _, e := range s.rows {
		if e.Status == StatusReady && e.ExpiresAt != nil && !e.ExpiresAt.After(now) && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

// MarkExpired implements Store.
func (s *MemoryStore) MarkExpired(_ context.Context, id uuid.UUID, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.find(id); i >= 0 {
		at := now
		s.rows[i].Status, s.rows[i].ExpiredAt, s.rows[i].ObjectKey = StatusExpired, &at, ""
	}
	return nil
}

// Downloaded implements Store.
func (s *MemoryStore) Downloaded(_ context.Context, id uuid.UUID, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.find(id); i >= 0 {
		at := now
		s.rows[i].Downloads++
		s.rows[i].LastDownloadAt = &at
	}
	return nil
}

// PostgresStore is the production Store.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const columns = `id, user_id, status, COALESCE(object_key, ''), size_bytes, attempts, requested_at, ready_at, expires_at, downloads, last_download_at, expired_at`

func scan(row pgx.Row) (Export, error) {
	var e Export
	err := row.Scan(&e.ID, &e.UserID, &e.Status, &e.ObjectKey, &e.SizeBytes, &e.Attempts, &e.RequestedAt, &e.ReadyAt, &e.ExpiresAt, &e.Downloads, &e.LastDownloadAt, &e.ExpiredAt)
	e.RequestedAt = e.RequestedAt.UTC()
	for _, p := range []**time.Time{&e.ReadyAt, &e.ExpiresAt, &e.LastDownloadAt, &e.ExpiredAt} {
		if *p != nil {
			t := (**p).UTC()
			*p = &t
		}
	}
	return e, err
}

func collect(rows pgx.Rows, err error) ([]Export, error) {
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Export, error) { return scan(row) })
	if out == nil {
		out = []Export{}
	}
	return out, err
}

// Latest implements Store.
func (s *PostgresStore) Latest(ctx context.Context, userID uuid.UUID) (Export, bool, error) {
	e, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM data_exports WHERE user_id = $1 ORDER BY requested_at DESC LIMIT 1`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Export{}, false, nil
	}
	return e, err == nil, err
}

// Create implements Store.
func (s *PostgresStore) Create(ctx context.Context, e Export, since time.Time) (Export, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Export{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, "data_exports:"+e.UserID.String()); err != nil {
		return Export{}, false, err
	}
	prev, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM data_exports WHERE user_id = $1 AND status <> 'FAILED' AND requested_at >= $2
		ORDER BY requested_at DESC LIMIT 1`, e.UserID, since))
	if err == nil {
		return prev, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Export{}, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO data_exports (id, user_id, status, requested_at) VALUES ($1, $2, 'REQUESTED', $3)`, e.ID, e.UserID, e.RequestedAt); err != nil {
		return Export{}, false, err
	}
	return e, true, tx.Commit(ctx)
}

// Claim implements Store (SKIP LOCKED; a BUILDING export whose lease ran
// out is taken again).
func (s *PostgresStore) Claim(ctx context.Context, now, leaseUntil time.Time, limit int) ([]Export, error) {
	return collect(s.pool.Query(ctx, `
		UPDATE data_exports SET status = 'BUILDING', lease_until = $2, attempts = attempts + 1
		WHERE id IN (SELECT id FROM data_exports
			WHERE status = 'REQUESTED' OR (status = 'BUILDING' AND lease_until < $1)
			ORDER BY requested_at LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING `+columns, now, leaseUntil, limit))
}

// Ready implements Store.
func (s *PostgresStore) Ready(ctx context.Context, id uuid.UUID, key string, size int64, now, expires time.Time) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var userID uuid.UUID
	if err := tx.QueryRow(ctx, `UPDATE data_exports SET status = 'READY', object_key = $2, size_bytes = $3, ready_at = $4, expires_at = $5, lease_until = NULL
		WHERE id = $1 RETURNING user_id`, id, key, size, now, expires).Scan(&userID); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"export_id": id, "user_id": userID, "expires_at": expires})
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`, uuid.New(), EventReady, AggregateExport, id, string(payload), now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Fail implements Store.
func (s *PostgresStore) Fail(ctx context.Context, id uuid.UUID, final bool) error {
	status := StatusRequested
	if final {
		status = StatusFailed
	}
	_, err := s.pool.Exec(ctx, `UPDATE data_exports SET status = $2, lease_until = NULL WHERE id = $1`, id, status)
	return err
}

// DueForExpiry implements Store.
func (s *PostgresStore) DueForExpiry(ctx context.Context, now time.Time, limit int) ([]Export, error) {
	return collect(s.pool.Query(ctx, `SELECT `+columns+` FROM data_exports WHERE status = 'READY' AND expires_at <= $1 ORDER BY expires_at LIMIT $2`, now, limit))
}

// MarkExpired implements Store.
func (s *PostgresStore) MarkExpired(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE data_exports SET status = 'EXPIRED', expired_at = $2, object_key = NULL WHERE id = $1 AND status = 'READY'`, id, now)
	return err
}

// Downloaded implements Store.
func (s *PostgresStore) Downloaded(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE data_exports SET downloads = downloads + 1, last_download_at = $2 WHERE id = $1`, id, now)
	return err
}
