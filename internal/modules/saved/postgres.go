package saved

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the production Store (migration 0026).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (s *PostgresStore) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// InsertEventTx writes LISTING_SAVED or LISTING_UNSAVED in tx.
func InsertEventTx(ctx context.Context, tx pgx.Tx, eventType string, userID, listingID uuid.UUID, at time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
		uuid.New(), eventType, AggregateListing, listingID, string(eventPayload(userID, listingID, at)), at); err != nil {
		return fmt.Errorf("insert outbox event %s: %w", eventType, err)
	}
	return nil
}

const savedColumns = `user_id, listing_id, saved_at, snapshot, snapshot_at`

func scanSaved(row pgx.Row) (Saved, error) {
	var x Saved
	var raw []byte
	if err := row.Scan(&x.UserID, &x.ListingID, &x.SavedAt, &raw, &x.SnapshotAt); err != nil {
		return Saved{}, err
	}
	if err := json.Unmarshal(raw, &x.Snapshot); err != nil {
		return Saved{}, fmt.Errorf("decode saved listing snapshot: %w", err)
	}
	x.SavedAt, x.SnapshotAt = x.SavedAt.UTC(), x.SnapshotAt.UTC()
	return x, nil
}

// Save implements Store.
func (s *PostgresStore) Save(ctx context.Context, x Saved) (Saved, bool, error) {
	snap, err := json.Marshal(x.Snapshot)
	if err != nil {
		return Saved{}, false, err
	}
	var out Saved
	created := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		stored, err := scanSaved(tx.QueryRow(ctx, `
			INSERT INTO saved_listings (`+savedColumns+`) VALUES ($1, $2, $3, $4::jsonb, $5)
			ON CONFLICT (user_id, listing_id) DO NOTHING
			RETURNING `+savedColumns, x.UserID, x.ListingID, x.SavedAt, string(snap), x.SnapshotAt))
		switch {
		case err == nil:
			out, created = stored, true
			return InsertEventTx(ctx, tx, EventListingSaved, x.UserID, x.ListingID, x.SavedAt)
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("insert saved listing: %w", err)
		}
		// Already saved (possibly by a concurrent request that has just
		// committed): a new statement sees that row.
		if out, err = scanSaved(tx.QueryRow(ctx, `SELECT `+savedColumns+` FROM saved_listings WHERE user_id = $1 AND listing_id = $2`,
			x.UserID, x.ListingID)); err != nil {
			return fmt.Errorf("load saved listing: %w", err)
		}
		return nil
	})
	return out, created, err
}

// Remove implements Store.
func (s *PostgresStore) Remove(ctx context.Context, userID, listingID uuid.UUID, at time.Time) (bool, error) {
	removed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM saved_listings WHERE user_id = $1 AND listing_id = $2`, userID, listingID)
		if err != nil {
			return fmt.Errorf("delete saved listing: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		removed = true
		return InsertEventTx(ctx, tx, EventListingUnsaved, userID, listingID, at)
	})
	return removed, err
}

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, userID uuid.UUID, limit, offset int) ([]Saved, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM saved_listings WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count saved listings: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+savedColumns+` FROM saved_listings WHERE user_id = $1
		ORDER BY saved_at DESC, listing_id::text LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list saved listings: %w", err)
	}
	defer rows.Close()
	out := []Saved{}
	for rows.Next() {
		x, err := scanSaved(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, x)
	}
	return out, total, rows.Err()
}

// ListingIDs implements Store.
func (s *PostgresStore) ListingIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT listing_id FROM saved_listings WHERE user_id = $1 ORDER BY saved_at DESC, listing_id::text`, userID)
	if err != nil {
		return nil, fmt.Errorf("list saved listing ids: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("list saved listing ids: %w", err)
	}
	if ids == nil {
		ids = []uuid.UUID{}
	}
	return ids, nil
}

// UpdateSnapshot implements Store.
func (s *PostgresStore) UpdateSnapshot(ctx context.Context, userID, listingID uuid.UUID, snap Snapshot, at time.Time) error {
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE saved_listings SET snapshot = $3::jsonb, snapshot_at = $4 WHERE user_id = $1 AND listing_id = $2`,
		userID, listingID, string(raw), at); err != nil {
		return fmt.Errorf("refresh saved listing snapshot: %w", err)
	}
	return nil
}
