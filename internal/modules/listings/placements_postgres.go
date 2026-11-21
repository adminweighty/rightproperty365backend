package listings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

const placementColumns = `p.id, p.listing_id, p.kind, p.starts_at, p.ends_at, p.subscription_id, COALESCE(p.note, ''), p.granted_by, p.granted_by_name,
	p.granted_at, p.ended_at, p.ended_by, COALESCE(p.end_reason, ''), l.slug, l.title, COALESCE(l.reference, ''), m.code`

const placementFrom = ` FROM listing_placements p JOIN listings l ON l.id = p.listing_id JOIN markets m ON m.id = l.market_id`

func scanPlacement(row pgx.Row) (Placement, error) {
	var p Placement
	err := row.Scan(&p.ID, &p.ListingID, &p.Kind, &p.StartsAt, &p.EndsAt, &p.SubscriptionID, &p.Note, &p.GrantedBy, &p.GrantedByName,
		&p.GrantedAt, &p.EndedAt, &p.EndedBy, &p.EndReason, &p.ListingSlug, &p.ListingTitle, &p.ListingReference, &p.MarketCode)
	p.StartsAt, p.EndsAt, p.GrantedAt = p.StartsAt.UTC(), p.EndsAt.UTC(), p.GrantedAt.UTC()
	if p.EndedAt != nil {
		t := p.EndedAt.UTC()
		p.EndedAt = &t
	}
	return p, err
}

// placementUntil is the SQL of Placement.Until.
const placementUntil = `LEAST(p.ends_at, COALESCE(p.ended_at, p.ends_at))`

// ListPlacements implements PlacementStore.
func (s *PostgresStore) ListPlacements(ctx context.Context, f PlacementFilter) ([]Placement, int, error) {
	// $1 (now) is always referenced, also without a status filter.
	where, args := []string{"$1::timestamptz IS NOT NULL"}, []any{f.Now}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.ListingID != nil {
		add("p.listing_id = $%d", *f.ListingID)
	}
	if f.Markets != nil {
		add("m.code = ANY($%d)", f.Markets)
	}
	ended := `(` + placementUntil + ` <= $1 OR (p.ended_at IS NOT NULL AND p.ended_at <= p.starts_at))`
	switch f.Status {
	case PlacementEnded:
		where = append(where, ended)
	case PlacementScheduled:
		where = append(where, `NOT `+ended+` AND p.starts_at > $1`)
	case PlacementActive:
		where = append(where, `NOT `+ended+` AND p.starts_at <= $1`)
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+placementFrom+` WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	size := max(f.PageSize, 1)
	args = append(args, size, max(f.Page-1, 0)*size)
	rows, err := s.pool.Query(ctx, `SELECT `+placementColumns+placementFrom+` WHERE `+cond+
		fmt.Sprintf(` ORDER BY p.starts_at DESC, p.id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Placement, error) { return scanPlacement(row) })
	if out == nil {
		out = []Placement{}
	}
	return out, total, err
}

// GetPlacement implements PlacementStore.
func (s *PostgresStore) GetPlacement(ctx context.Context, id uuid.UUID) (Placement, error) {
	p, err := scanPlacement(s.pool.QueryRow(ctx, `SELECT `+placementColumns+placementFrom+` WHERE p.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Placement{}, ErrPlacementNotFound
	}
	return p, err
}

// GrantPlacement implements PlacementStore. The listing row is locked, so
// two grants on one listing run one after the other; a subscription's count
// runs under an advisory lock on the subscription.
func (s *PostgresStore) GrantPlacement(ctx context.Context, p Placement, limit *int, audit adminaudit.Event, now time.Time) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM listings WHERE id = $1 FOR UPDATE`, p.ListingID); err != nil {
		return err
	}
	var clash bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM listing_placements p WHERE p.listing_id = $1
		AND p.starts_at < $3 AND `+placementUntil+` > $2 AND `+placementUntil+` > p.starts_at)`, p.ListingID, p.StartsAt, p.EndsAt).Scan(&clash); err != nil {
		return err
	}
	if clash {
		return ErrPlacementOverlap
	}
	if limit != nil && p.SubscriptionID != nil {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, "placements:"+p.SubscriptionID.String()); err != nil {
			return err
		}
		var tied int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM listing_placements p WHERE p.subscription_id = $1
			AND p.starts_at < $3 AND `+placementUntil+` > $2 AND `+placementUntil+` > p.starts_at`, *p.SubscriptionID, p.StartsAt, p.EndsAt).Scan(&tied); err != nil {
			return err
		}
		if tied >= *limit {
			return ErrPlacementLimit
		}
	}
	var note *string
	if p.Note != "" {
		note = &p.Note
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO listing_placements (id, listing_id, kind, starts_at, ends_at, subscription_id, note, granted_by, granted_by_name, granted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		p.ID, p.ListingID, p.Kind, p.StartsAt, p.EndsAt, p.SubscriptionID, note, p.GrantedBy, p.GrantedByName, p.GrantedAt); err != nil {
		return err
	}
	if err := adminaudit.Insert(ctx, tx, audit); err != nil {
		return err
	}
	if _, err := syncPlacementsTx(ctx, tx, now, &p.ListingID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EndPlacement implements PlacementStore.
func (s *PostgresStore) EndPlacement(ctx context.Context, id uuid.UUID, by uuid.UUID, reason string, audit func(Placement) adminaudit.Event, now time.Time) (Placement, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Placement{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	p, err := scanPlacement(tx.QueryRow(ctx, `SELECT `+placementColumns+placementFrom+` WHERE p.id = $1 FOR UPDATE OF p`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Placement{}, ErrPlacementNotFound
	}
	if err != nil {
		return Placement{}, err
	}
	if p.Status(now) == PlacementEnded {
		return Placement{}, ErrPlacementEnded
	}
	if _, err := tx.Exec(ctx, `UPDATE listing_placements SET ended_at = $2, ended_by = $3, end_reason = $4 WHERE id = $1`, id, now, by, reason); err != nil {
		return Placement{}, err
	}
	at := now
	p.EndedAt, p.EndedBy, p.EndReason = &at, &by, reason
	if err := adminaudit.Insert(ctx, tx, audit(p)); err != nil {
		return Placement{}, err
	}
	if _, err := syncPlacementsTx(ctx, tx, now, &p.ListingID); err != nil {
		return Placement{}, err
	}
	return p, tx.Commit(ctx)
}

// SyncPlacements implements PlacementStore.
func (s *PostgresStore) SyncPlacements(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	n, err := syncPlacementsTx(ctx, tx, now, nil)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}

// syncPlacementsTx sets featured and placement_kind of every listing that
// has placements (or only of one) to its active placement, and writes
// LISTING_UPDATED for each listing it changed.
func syncPlacementsTx(ctx context.Context, tx pgx.Tx, now time.Time, only *uuid.UUID) (int, error) {
	tag, err := tx.Exec(ctx, `
		WITH wanted AS (
			SELECT p.listing_id,
			       (array_agg(p.kind ORDER BY p.starts_at DESC) FILTER (
			           WHERE p.starts_at <= $1 AND `+placementUntil+` > $1 AND `+placementUntil+` > p.starts_at))[1] AS kind
			FROM listing_placements p
			WHERE $2::uuid IS NULL OR p.listing_id = $2
			GROUP BY p.listing_id
		), changed AS (
			UPDATE listings l SET placement_kind = w.kind, featured = (w.kind IS NOT NULL)
			FROM wanted w
			WHERE l.id = w.listing_id AND (l.placement_kind IS DISTINCT FROM w.kind OR l.featured <> (w.kind IS NOT NULL))
			RETURNING l.id
		)
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		SELECT gen_random_uuid(), $3, $4, c.id, jsonb_build_object('listing_id', c.id), $1, $1 FROM changed c`,
		now, only, EventListingUpdated, AggregateListing)
	if err != nil {
		return 0, fmt.Errorf("sync placements: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
