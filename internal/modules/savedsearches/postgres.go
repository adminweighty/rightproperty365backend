package savedsearches

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the production Store.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const columns = `id, user_id, name, query, frequency, unsubscribe_token, last_checked_at, next_check_at, last_alert_at, created_at, updated_at`

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func scan(row pgx.Row) (SavedSearch, error) {
	var ss SavedSearch
	err := row.Scan(&ss.ID, &ss.UserID, &ss.Name, &ss.Query, &ss.Frequency, &ss.UnsubscribeToken, &ss.LastCheckedAt, &ss.NextCheckAt,
		&ss.LastAlertAt, &ss.CreatedAt, &ss.UpdatedAt)
	ss.LastCheckedAt, ss.CreatedAt, ss.UpdatedAt = ss.LastCheckedAt.UTC(), ss.CreatedAt.UTC(), ss.UpdatedAt.UTC()
	ss.NextCheckAt, ss.LastAlertAt = utc(ss.NextCheckAt), utc(ss.LastAlertAt)
	return ss, err
}

func collect(rows pgx.Rows, err error) ([]SavedSearch, error) {
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SavedSearch, error) { return scan(row) })
	if out == nil {
		out = []SavedSearch{}
	}
	return out, err
}

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, userID uuid.UUID) ([]SavedSearch, error) {
	return collect(s.pool.Query(ctx, `SELECT `+columns+` FROM saved_searches WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, userID))
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (SavedSearch, error) {
	ss, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM saved_searches WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return SavedSearch{}, ErrNotFound
	}
	return ss, err
}

// Create implements Store: the count runs under a per-user advisory lock.
func (s *PostgresStore) Create(ctx context.Context, ss SavedSearch) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, "saved_searches:"+ss.UserID.String()); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM saved_searches WHERE user_id = $1`, ss.UserID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxPerUser {
		return ErrLimit
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO saved_searches (id, user_id, name, query, frequency, unsubscribe_token, last_checked_at, next_check_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		ss.ID, ss.UserID, ss.Name, ss.Query, ss.Frequency, ss.UnsubscribeToken, ss.LastCheckedAt, ss.NextCheckAt, ss.CreatedAt, ss.UpdatedAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Update implements Store.
func (s *PostgresStore) Update(ctx context.Context, ss SavedSearch) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE saved_searches SET name = $2, frequency = $3, last_checked_at = $4, next_check_at = $5, updated_at = $6 WHERE id = $1`,
		ss.ID, ss.Name, ss.Frequency, ss.LastCheckedAt, ss.NextCheckAt, ss.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete implements Store.
func (s *PostgresStore) Delete(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM saved_searches WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Unsubscribe implements Store.
func (s *PostgresStore) Unsubscribe(ctx context.Context, token string, now time.Time) (SavedSearch, error) {
	ss, err := scan(s.pool.QueryRow(ctx, `
		UPDATE saved_searches SET
			frequency = 'OFF', next_check_at = NULL,
			updated_at = CASE WHEN frequency = 'OFF' THEN updated_at ELSE $2 END
		WHERE unsubscribe_token = $1
		RETURNING `+columns, token, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return SavedSearch{}, ErrInvalidToken
	}
	return ss, err
}

// Claim implements Store (SKIP LOCKED: safe with several workers).
func (s *PostgresStore) Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]SavedSearch, error) {
	return collect(s.pool.Query(ctx, `
		UPDATE saved_searches SET next_check_at = $2
		WHERE id IN (
			SELECT id FROM saved_searches
			WHERE frequency <> 'OFF' AND next_check_at <= $1
			ORDER BY next_check_at LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING `+columns, now, now.Add(lease), limit))
}

// Record implements Store: the watermark, the next check and the alert's
// outbox event commit together.
func (s *PostgresStore) Record(ctx context.Context, id uuid.UUID, checkedAt time.Time, alert *Alert) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var userID uuid.UUID
	var frequency string
	err = tx.QueryRow(ctx, `
		UPDATE saved_searches SET last_checked_at = $2::timestamptz,
			next_check_at = CASE frequency
				WHEN 'INSTANT' THEN $2::timestamptz + interval '10 minutes'
				WHEN 'DAILY' THEN $2::timestamptz + interval '1 day'
				WHEN 'WEEKLY' THEN $2::timestamptz + interval '7 days'
				ELSE NULL END,
			last_alert_at = CASE WHEN $3::boolean AND frequency <> 'OFF' THEN $2::timestamptz ELSE last_alert_at END
		WHERE id = $1
		RETURNING user_id, frequency`, id, checkedAt, alert != nil).Scan(&userID, &frequency)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return err
	}
	if alert != nil && frequency != FrequencyOff {
		payload, err := json.Marshal(EventPayload{SavedSearchID: id, UserID: userID, Total: alert.Total, More: alert.More, Lines: alert.Lines, OccurredAt: checkedAt})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
			uuid.New(), EventMatched, AggregateSavedSearch, id, string(payload), checkedAt); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
