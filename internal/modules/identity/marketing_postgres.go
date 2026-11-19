package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Marketing consent on Postgres (migration 0041_marketing_consent).

// insertMarketingEvent appends one marketing_consent_events row.
func insertMarketingEvent(ctx context.Context, q querier, e MarketingConsentEvent) error {
	var version *string
	if e.ConsentTextVersion != "" {
		version = &e.ConsentTextVersion
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO marketing_consent_events (id, user_id, opt_in, changed_at, source, context, consent_text_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.ID, e.UserID, e.OptIn, e.ChangedAt.UTC(), e.Source, e.Context, version); err != nil {
		return fmt.Errorf("insert marketing consent: %w", err)
	}
	return nil
}

// MarketingState implements Store.
func (s *PostgresStore) MarketingState(ctx context.Context, userID uuid.UUID, limit int) (MarketingState, error) {
	var out MarketingState
	err := s.pool.QueryRow(ctx, `SELECT marketing_opt_in FROM users WHERE id = $1`, userID).Scan(&out.OptIn)
	if errors.Is(err, pgx.ErrNoRows) {
		return MarketingState{}, ErrNotFound
	}
	if err != nil {
		return MarketingState{}, fmt.Errorf("load marketing opt-in: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, opt_in, changed_at, source, context, COALESCE(consent_text_version, '')
		FROM marketing_consent_events WHERE user_id = $1
		ORDER BY changed_at DESC, id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return MarketingState{}, fmt.Errorf("load marketing consent: %w", err)
	}
	out.History, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (MarketingConsentEvent, error) {
		var e MarketingConsentEvent
		err := row.Scan(&e.ID, &e.UserID, &e.OptIn, &e.ChangedAt, &e.Source, &e.Context, &e.ConsentTextVersion)
		e.ChangedAt = e.ChangedAt.UTC()
		return e, err
	})
	if err != nil {
		return MarketingState{}, fmt.Errorf("load marketing consent: %w", err)
	}
	return out, nil
}

// ChangeMarketingOptIn implements Store.
func (s *PostgresStore) ChangeMarketingOptIn(ctx context.Context, e MarketingConsentEvent) (bool, error) {
	changed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		changed = false
		var current bool
		err := tx.QueryRow(ctx, `SELECT marketing_opt_in FROM users WHERE id = $1 FOR UPDATE`, e.UserID).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		if current == e.OptIn {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET marketing_opt_in = $2, updated_at = $3 WHERE id = $1`,
			e.UserID, e.OptIn, e.ChangedAt.UTC()); err != nil {
			return fmt.Errorf("update marketing opt-in: %w", err)
		}
		if err := insertMarketingEvent(ctx, tx, e); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// MarketingEligibility implements Store.
func (s *PostgresStore) MarketingEligibility(ctx context.Context, userID uuid.UUID) (MarketingEligibility, error) {
	var out MarketingEligibility
	err := s.pool.QueryRow(ctx, `SELECT status, email_verified_at IS NOT NULL, marketing_opt_in FROM users WHERE id = $1`, userID).
		Scan(&out.Status, &out.EmailVerified, &out.OptIn)
	if errors.Is(err, pgx.ErrNoRows) {
		return MarketingEligibility{}, ErrNotFound
	}
	if err != nil {
		return MarketingEligibility{}, fmt.Errorf("load marketing eligibility: %w", err)
	}
	return out, nil
}

// DeleteMarketingConsentTx deletes the user's marketing consent records
// inside the account deletion transaction: the users row is anonymised, not
// deleted, so its ON DELETE CASCADE never fires. (The anonymisation itself
// sets marketing_opt_in to false.)
func DeleteMarketingConsentTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM marketing_consent_events WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete marketing consent: %w", err)
	}
	return nil
}

// MarketingConsentText implements Store.
func (s *PostgresStore) MarketingConsentText(ctx context.Context, version string) (MarketingConsentText, error) {
	var t MarketingConsentText
	err := s.pool.QueryRow(ctx, `SELECT version, text, status, created_at FROM marketing_consent_texts WHERE version = $1`, version).
		Scan(&t.Version, &t.Text, &t.Status, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MarketingConsentText{}, ErrConsentTextNotFound
	}
	if err != nil {
		return MarketingConsentText{}, fmt.Errorf("load marketing consent text: %w", err)
	}
	t.CreatedAt = t.CreatedAt.UTC()
	return t, nil
}
