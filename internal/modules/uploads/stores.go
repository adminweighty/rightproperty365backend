package uploads

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MemoryStore keeps intents in memory.
type MemoryStore struct {
	mu      sync.Mutex
	intents map[uuid.UUID]Intent
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{intents: map[uuid.UUID]Intent{}} }

// Create implements Store.
func (s *MemoryStore) Create(_ context.Context, i Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intents[i.ID] = i
	return nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.intents[id]
	if !ok {
		return Intent{}, ErrNotFound
	}
	return i, nil
}

func open(i Intent, now time.Time) bool {
	return (i.Status == StatusPendingUpload || i.Status == StatusReady) && i.ExpiresAt.After(now)
}

// OpenCount implements Store.
func (s *MemoryStore) OpenCount(_ context.Context, user uuid.UUID, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, i := range s.intents {
		if i.UserID == user && open(i, now) {
			n++
		}
	}
	return n, nil
}

// Transition implements Store.
func (s *MemoryStore) Transition(_ context.Context, id uuid.UUID, from []string, to, reason string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.intents[id]
	if !ok {
		return false, ErrNotFound
	}
	if !slices.Contains(from, i.Status) {
		return false, nil
	}
	i.Status, i.Reason = to, reason
	switch to {
	case StatusReady, StatusRejected:
		i.CompletedAt = &at
	case StatusUsed:
		i.UsedAt = &at
	}
	s.intents[id] = i
	return true, nil
}

// Stale implements Store.
func (s *MemoryStore) Stale(_ context.Context, now time.Time, limit int) ([]Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Intent{}
	for _, i := range s.intents {
		if i.Status != StatusUsed && i.Status != StatusExpired && !i.ExpiresAt.After(now) {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ExpiresAt.Before(out[b].ExpiresAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PostgresStore keeps intents in upload_intents.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore builds the store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const columns = `id, user_id, purpose, content_type, size_bytes, sha256, object_key, status, COALESCE(reason, ''),
	created_at, upload_by, completed_at, used_at, expires_at`

func scan(row pgx.Row) (Intent, error) {
	var i Intent
	err := row.Scan(&i.ID, &i.UserID, &i.Purpose, &i.ContentType, &i.SizeBytes, &i.SHA256, &i.ObjectKey, &i.Status, &i.Reason,
		&i.CreatedAt, &i.UploadBy, &i.CompletedAt, &i.UsedAt, &i.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, ErrNotFound
	}
	return i, err
}

// Create implements Store.
func (s *PostgresStore) Create(ctx context.Context, i Intent) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO upload_intents (id, user_id, purpose, content_type, size_bytes, sha256, object_key, status,
		created_at, upload_by, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		i.ID, i.UserID, i.Purpose, i.ContentType, i.SizeBytes, i.SHA256, i.ObjectKey, i.Status, i.CreatedAt, i.UploadBy, i.ExpiresAt)
	return err
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Intent, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM upload_intents WHERE id = $1`, id))
}

// OpenCount implements Store.
func (s *PostgresStore) OpenCount(ctx context.Context, user uuid.UUID, now time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM upload_intents WHERE user_id = $1 AND status IN ('PENDING_UPLOAD', 'READY') AND expires_at > $2`,
		user, now).Scan(&n)
	return n, err
}

// Transition implements Store.
func (s *PostgresStore) Transition(ctx context.Context, id uuid.UUID, from []string, to, reason string, at time.Time) (bool, error) {
	var r *string
	if reason != "" {
		r = &reason
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE upload_intents SET status = $3, reason = $4,
			completed_at = CASE WHEN $3 IN ('READY', 'REJECTED') THEN $5::timestamptz ELSE completed_at END,
			used_at = CASE WHEN $3 = 'USED' THEN $5::timestamptz ELSE used_at END
		WHERE id = $1 AND status = ANY($2)`, id, from, to, r, at)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// Stale implements Store.
func (s *PostgresStore) Stale(ctx context.Context, now time.Time, limit int) ([]Intent, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM upload_intents
		WHERE status IN ('PENDING_UPLOAD', 'READY', 'REJECTED') AND expires_at <= $1 ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Intent{}
	for rows.Next() {
		i, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
