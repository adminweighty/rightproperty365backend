package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// Administrator MFA on Postgres (migration 0042_admin_mfa).

// AdminMFA implements MFAStore.
func (s *PostgresStore) AdminMFA(ctx context.Context, userID uuid.UUID) (AdminMFA, error) {
	rec := AdminMFA{UserID: userID}
	err := s.pool.QueryRow(ctx, `
		SELECT secret_sealed, confirmed_at, last_used_step, created_at, updated_at
		FROM admin_mfa_totp WHERE user_id = $1`, userID).
		Scan(&rec.SecretSealed, &rec.ConfirmedAt, &rec.LastUsedStep, &rec.CreatedAt, &rec.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminMFA{}, ErrNotFound
	}
	if err != nil {
		return AdminMFA{}, err
	}
	rec.CreatedAt, rec.UpdatedAt = rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()
	return rec, nil
}

// PutPendingTOTP implements MFAStore. The upsert only replaces a PENDING row.
func (s *PostgresStore) PutPendingTOTP(ctx context.Context, userID uuid.UUID, sealed []byte, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO admin_mfa_totp (user_id, secret_sealed, confirmed_at, last_used_step, created_at, updated_at)
		VALUES ($1, $2, NULL, 0, $3, $3)
		ON CONFLICT (user_id) DO UPDATE
		SET secret_sealed = EXCLUDED.secret_sealed, last_used_step = 0, created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at
		WHERE admin_mfa_totp.confirmed_at IS NULL`, userID, sealed, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrMFAAlreadyEnrolled
	}
	return nil
}

func insertRecoveryCodesTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, hashes []string, now time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM admin_mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete recovery codes: %w", err)
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx, `
			INSERT INTO admin_mfa_recovery_codes (id, user_id, code_hash, created_at) VALUES ($1, $2, $3, $4)`,
			uuid.New(), userID, h, now); err != nil {
			return fmt.Errorf("insert recovery code: %w", err)
		}
	}
	return nil
}

// ConfirmTOTP implements MFAStore.
func (s *PostgresStore) ConfirmTOTP(ctx context.Context, userID uuid.UUID, step int64, hashes []string, notBefore, now time.Time, audit adminaudit.Event) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE admin_mfa_totp SET confirmed_at = $2, last_used_step = $3, updated_at = $2
			WHERE user_id = $1 AND confirmed_at IS NULL AND created_at >= $4`, userID, now, step, notBefore)
		if err != nil {
			return fmt.Errorf("confirm totp: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if err := insertRecoveryCodesTx(ctx, tx, userID, hashes, now); err != nil {
			return err
		}
		return adminaudit.Insert(ctx, tx, audit)
	})
}

// UseTOTPStep implements MFAStore. The conditional UPDATE is what makes a
// step usable once under concurrency.
func (s *PostgresStore) UseTOTPStep(ctx context.Context, userID uuid.UUID, step int64, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE admin_mfa_totp SET last_used_step = $2, updated_at = $3
		WHERE user_id = $1 AND confirmed_at IS NOT NULL AND last_used_step < $2`, userID, step, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	rec, err := s.AdminMFA(ctx, userID)
	if err != nil {
		return err
	}
	if rec.ConfirmedAt == nil {
		return ErrNotFound
	}
	return ErrMFACodeReused
}

// UnusedRecoveryCodes implements MFAStore.
func (s *PostgresStore) UnusedRecoveryCodes(ctx context.Context, userID uuid.UUID) ([]RecoveryCode, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, code_hash, used_at, created_at FROM admin_mfa_recovery_codes
		WHERE user_id = $1 AND used_at IS NULL ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RecoveryCode, error) {
		var c RecoveryCode
		err := row.Scan(&c.ID, &c.UserID, &c.Hash, &c.UsedAt, &c.CreatedAt)
		return c, err
	})
}

// UseRecoveryCode implements MFAStore.
func (s *PostgresStore) UseRecoveryCode(ctx context.Context, userID, codeID uuid.UUID, now time.Time, audit adminaudit.Event, event OutboxEvent) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE admin_mfa_recovery_codes SET used_at = $3 WHERE id = $1 AND user_id = $2 AND used_at IS NULL`, codeID, userID, now)
		if err != nil {
			return fmt.Errorf("use recovery code: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if err := adminaudit.Insert(ctx, tx, audit); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, event)
	})
}

// ReplaceRecoveryCodes implements MFAStore.
func (s *PostgresStore) ReplaceRecoveryCodes(ctx context.Context, userID uuid.UUID, hashes []string, now time.Time, audit adminaudit.Event) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var confirmed *time.Time
		err := tx.QueryRow(ctx, `SELECT confirmed_at FROM admin_mfa_totp WHERE user_id = $1 FOR UPDATE`, userID).Scan(&confirmed)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && confirmed == nil) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock totp: %w", err)
		}
		if err := insertRecoveryCodesTx(ctx, tx, userID, hashes, now); err != nil {
			return err
		}
		return adminaudit.Insert(ctx, tx, audit)
	})
}

// DeleteAdminMFATx deletes the user's authenticator and recovery codes
// inside the caller's transaction (account deletion: the users row is
// anonymised, not deleted, so its ON DELETE CASCADE never fires). It returns
// whether an authenticator row existed.
func DeleteAdminMFATx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (bool, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM admin_mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
		return false, fmt.Errorf("delete recovery codes: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM admin_mfa_totp WHERE user_id = $1`, userID)
	if err != nil {
		return false, fmt.Errorf("delete totp: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteAdminMFA implements MFAStore.
func (s *PostgresStore) DeleteAdminMFA(ctx context.Context, userID uuid.UUID, now time.Time, audit adminaudit.Event, revokeSessions bool, events ...OutboxEvent) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		existed, err := DeleteAdminMFATx(ctx, tx, userID)
		if err != nil {
			return err
		}
		if !existed {
			return ErrNotFound
		}
		if revokeSessions {
			if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, now); err != nil {
				return fmt.Errorf("revoke sessions: %w", err)
			}
		}
		if err := adminaudit.Insert(ctx, tx, audit); err != nil {
			return err
		}
		for _, e := range events {
			if err := insertOutbox(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecordAdminAudit implements MFAStore.
func (s *PostgresStore) RecordAdminAudit(ctx context.Context, e adminaudit.Event) error {
	return adminaudit.Insert(ctx, s.pool, e)
}

// MarkLoginChallengeCodeVerified implements MFAStore.
func (s *PostgresStore) MarkLoginChallengeCodeVerified(ctx context.Context, idHash []byte, now, expiresAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sign_in_challenges SET code_verified_at = $2, expires_at = $3
		WHERE id = $1 AND scope = 'admin' AND consumed_at IS NULL AND code_verified_at IS NULL AND expires_at > $2
		  AND attempts < $4 AND mfa_attempts < $4`, idHash, now, expiresAt, SignInCodeMaxAttempts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FailLoginChallengeMFA implements MFAStore.
func (s *PostgresStore) FailLoginChallengeMFA(ctx context.Context, idHash []byte, _ time.Time) (int, error) {
	var attempts int
	err := s.pool.QueryRow(ctx, `
		UPDATE sign_in_challenges SET mfa_attempts = mfa_attempts + 1
		WHERE id = $1 AND consumed_at IS NULL
		RETURNING mfa_attempts`, idHash).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return attempts, err
}

// PutConfirmedAdminMFATx stores a CONFIRMED authenticator and its recovery
// code hashes in the caller's transaction, replacing any (the development
// seed only).
func PutConfirmedAdminMFATx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, sealed []byte, hashes []string, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_mfa_totp (user_id, secret_sealed, confirmed_at, last_used_step, created_at, updated_at)
		VALUES ($1, $2, $3, 0, $3, $3)
		ON CONFLICT (user_id) DO UPDATE
		SET secret_sealed = EXCLUDED.secret_sealed, confirmed_at = EXCLUDED.confirmed_at, last_used_step = 0,
		    created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at`, userID, sealed, now); err != nil {
		return fmt.Errorf("seed totp: %w", err)
	}
	return insertRecoveryCodesTx(ctx, tx, userID, hashes, now)
}
