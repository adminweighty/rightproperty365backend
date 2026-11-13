package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresQueue is the outbox_events-backed Queue.
//
// Leasing is a single statement (CTE with FOR UPDATE SKIP LOCKED feeding an
// UPDATE), so it is safe with several workers and with PgBouncer transaction
// mode: no session state and no long-held transaction. The lease itself is a
// row-level locked_until timestamp, not a database lock, so a crashed worker's
// events become available again when the lease expires.
type PostgresQueue struct{ pool *pgxpool.Pool }

// NewPostgresQueue returns a Postgres-backed queue.
func NewPostgresQueue(pool *pgxpool.Pool) *PostgresQueue { return &PostgresQueue{pool: pool} }

// Lease implements Queue.
func (q *PostgresQueue) Lease(ctx context.Context, workerID string, limit int, leaseFor time.Duration) ([]Event, error) {
	rows, err := q.pool.Query(ctx, `
		WITH batch AS (
			SELECT id
			FROM outbox_events
			WHERE processed_at IS NULL
			  AND dead_letter_at IS NULL
			  AND available_at <= now()
			  AND (locked_until IS NULL OR locked_until < now())
			ORDER BY available_at, created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_events e
		SET locked_until = now() + ($2::int * interval '1 second'),
		    locked_by = $3,
		    attempts = e.attempts + 1
		FROM batch
		WHERE e.id = batch.id
		RETURNING e.id, e.event_type, e.aggregate_type, e.aggregate_id, e.payload::text, e.attempts, e.created_at`,
		limit, int(leaseFor.Seconds()), workerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var (
			ev      Event
			payload string
		)
		if err := rows.Scan(&ev.ID, &ev.Type, &ev.AggregateType, &ev.AggregateID, &payload, &ev.Attempts, &ev.CreatedAt); err != nil {
			return nil, err
		}
		ev.Payload = json.RawMessage(payload)
		events = append(events, ev)
	}
	return events, rows.Err()
}

// MarkProcessed implements Queue.
func (q *PostgresQueue) MarkProcessed(ctx context.Context, id uuid.UUID, workerID string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE outbox_events
		SET processed_at = now(), locked_until = NULL, locked_by = NULL, last_error = NULL
		WHERE id = $1 AND locked_by = $2`, id, workerID)
	return err
}

// MarkRetry implements Queue.
func (q *PostgresQueue) MarkRetry(ctx context.Context, id uuid.UUID, workerID, lastError string, retryAt time.Time) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE outbox_events
		SET available_at = $3, last_error = $4, locked_until = NULL, locked_by = NULL
		WHERE id = $1 AND locked_by = $2`, id, workerID, retryAt.UTC(), lastError)
	return err
}

// MarkDeadLetter implements Queue.
func (q *PostgresQueue) MarkDeadLetter(ctx context.Context, id uuid.UUID, workerID, lastError string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE outbox_events
		SET dead_letter_at = now(), last_error = $3, locked_until = NULL, locked_by = NULL
		WHERE id = $1 AND locked_by = $2`, id, workerID, lastError)
	return err
}
