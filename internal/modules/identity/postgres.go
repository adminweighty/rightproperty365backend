package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/auth"
)

// Constraint names from migrations/0001_init.up.sql and 0002_identity.up.sql.
const (
	uniqueViolation      = "23505"
	usersEmailConstraint = "users_email_lower_key"
)

// PostgresStore is the production Store. All statements are compatible with
// PgBouncer transaction mode (no session state, explicit short transactions).
//
// NOTE: written against migrations 0001 + 0002 and compiled, but not yet
// exercised against a real database.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// querier is satisfied by both the pool and a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *PostgresStore) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// No-op after a successful commit. WithoutCancel so a cancelled request
	// still releases the connection cleanly.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func insertConsent(ctx context.Context, q querier, u NewUser) error {
	_, err := q.Exec(ctx, `
		INSERT INTO user_consents (id, user_id, consent_type, version, accepted_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $5)`,
		uuid.New(), u.ID, ConsentPrivacyNotice, u.PrivacyVersion, u.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert consent: %w", err)
	}
	return nil
}

// CreateUser implements Store.
func (s *PostgresStore) CreateUser(ctx context.Context, u NewUser) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO users (id, email, full_name, status, password_hash, email_verified_at, marketing_opt_in, created_at, updated_at)
			VALUES ($1, $2, $3, 'ACTIVE', $4, $5, $6, $7, $7)`,
			u.ID, u.Email, u.Name, u.PasswordHash, u.EmailVerifiedAt, u.MarketingOptIn, u.CreatedAt)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == usersEmailConstraint {
				return ErrEmailTaken
			}
			return fmt.Errorf("insert user: %w", err)
		}
		return insertConsent(ctx, tx, u)
	})
}

// UpdatePendingRegistration implements Store.
func (s *PostgresStore) UpdatePendingRegistration(ctx context.Context, u NewUser) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE users
			SET full_name = $2, password_hash = $3, marketing_opt_in = $4, updated_at = $5
			WHERE id = $1 AND email_verified_at IS NULL`,
			u.ID, u.Name, u.PasswordHash, u.MarketingOptIn, u.CreatedAt)
		if err != nil {
			return fmt.Errorf("update pending user: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `
			UPDATE email_verification_tokens SET used_at = $2
			WHERE user_id = $1 AND used_at IS NULL`, u.ID, u.CreatedAt); err != nil {
			return fmt.Errorf("invalidate verification tokens: %w", err)
		}
		return insertConsent(ctx, tx, u)
	})
}

const userColumns = `id, email, full_name, status, COALESCE(password_hash, ''), email_verified_at`

func (s *PostgresStore) loadUser(ctx context.Context, where string, arg any) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE `+where, arg).
		Scan(&u.ID, &u.Email, &u.Name, &u.Status, &u.PasswordHash, &u.EmailVerifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	if u.EmailVerifiedAt != nil {
		utc := u.EmailVerifiedAt.UTC()
		u.EmailVerifiedAt = &utc
	}

	// Platform roles. market_id is only set for market-scoped roles.
	rows, err := s.pool.Query(ctx, `
		SELECT a.role, COALESCE(m.code, '')
		FROM platform_role_assignments a
		LEFT JOIN markets m ON m.id = a.market_id
		WHERE a.user_id = $1
		ORDER BY a.role, m.code`, u.ID)
	if err != nil {
		return User{}, fmt.Errorf("load platform roles: %w", err)
	}
	u.PlatformRoles, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (auth.PlatformRole, error) {
		var r auth.PlatformRole
		err := row.Scan(&r.Role, &r.MarketCode)
		return r, err
	})
	if err != nil {
		return User{}, fmt.Errorf("load platform roles: %w", err)
	}

	// Active memberships of active organisations only.
	rows, err = s.pool.Query(ctx, `
		SELECT o.id, o.name, o.kind, mk.code, m.role
		FROM memberships m
		JOIN organisations o ON o.id = m.organisation_id
		JOIN markets mk ON mk.id = m.market_id
		WHERE m.user_id = $1 AND m.status = 'ACTIVE' AND o.status = 'ACTIVE'
		ORDER BY o.name, o.id`, u.ID)
	if err != nil {
		return User{}, fmt.Errorf("load memberships: %w", err)
	}
	u.Memberships, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (auth.Membership, error) {
		var m auth.Membership
		err := row.Scan(&m.OrganisationID, &m.OrganisationName, &m.OrganisationKind, &m.MarketCode, &m.Role)
		return m, err
	})
	if err != nil {
		return User{}, fmt.Errorf("load memberships: %w", err)
	}
	return u, nil
}

// UserByEmail implements Store.
func (s *PostgresStore) UserByEmail(ctx context.Context, email string) (User, error) {
	return s.loadUser(ctx, `lower(email) = $1`, email)
}

// UserByID implements Store.
func (s *PostgresStore) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	return s.loadUser(ctx, `id = $1`, id)
}

// CreateVerificationToken implements Store.
func (s *PostgresStore) CreateVerificationToken(ctx context.Context, userID uuid.UUID, tokenHash []byte, expiresAt, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO email_verification_tokens (id, user_id, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5)`, uuid.New(), userID, tokenHash, expiresAt, now)
	return err
}

// ConsumeVerificationToken implements Store. The single UPDATE ... RETURNING
// is what makes the token single-use under concurrency.
func (s *PostgresStore) ConsumeVerificationToken(ctx context.Context, tokenHash []byte, now time.Time) (uuid.UUID, error) {
	var userID uuid.UUID
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE email_verification_tokens SET used_at = $2
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
			RETURNING user_id`, tokenHash, now).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		}
		if err != nil {
			return fmt.Errorf("consume verification token: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE users SET email_verified_at = COALESCE(email_verified_at, $2), updated_at = $2
			WHERE id = $1`, userID, now); err != nil {
			return fmt.Errorf("mark email verified: %w", err)
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return userID, nil
}

// CreatePasswordResetToken implements Store.
func (s *PostgresStore) CreatePasswordResetToken(ctx context.Context, userID uuid.UUID, tokenHash []byte, expiresAt, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO password_reset_tokens (id, user_id, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5)`, uuid.New(), userID, tokenHash, expiresAt, now)
	return err
}

// ResetPassword implements Store: token consumption, the new password, the
// invalidation of other reset tokens and the revocation of every session are
// one transaction.
func (s *PostgresStore) ResetPassword(ctx context.Context, tokenHash []byte, passwordHash string, now time.Time) (uuid.UUID, error) {
	var userID uuid.UUID
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE password_reset_tokens SET used_at = $2
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
			RETURNING user_id`, tokenHash, now).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		}
		if err != nil {
			return fmt.Errorf("consume reset token: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE users
			SET password_hash = $2, email_verified_at = COALESCE(email_verified_at, $3), updated_at = $3
			WHERE id = $1`, userID, passwordHash, now); err != nil {
			return fmt.Errorf("set password: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE password_reset_tokens SET used_at = $2
			WHERE user_id = $1 AND used_at IS NULL`, userID, now); err != nil {
			return fmt.Errorf("invalidate reset tokens: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE sessions SET revoked_at = $2
			WHERE user_id = $1 AND revoked_at IS NULL`, userID, now); err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return userID, nil
}

// CreateSession implements Store.
func (s *PostgresStore) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, token_hash, delivery, created_at, last_seen_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sess.ID, sess.UserID, sess.TokenHash, sess.Delivery, sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt)
	return err
}

// SessionByTokenHash implements Store.
func (s *PostgresStore) SessionByTokenHash(ctx context.Context, tokenHash []byte) (Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, token_hash, delivery, created_at, last_seen_at, expires_at, revoked_at
		FROM sessions WHERE token_hash = $1`, tokenHash).
		Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &sess.Delivery, &sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt, &sess.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = sess.CreatedAt.UTC(), sess.LastSeenAt.UTC(), sess.ExpiresAt.UTC()
	return sess, nil
}

// TouchSession implements Store.
func (s *PostgresStore) TouchSession(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET last_seen_at = $2
		WHERE id = $1 AND revoked_at IS NULL AND last_seen_at < $2`, id, now)
	return err
}

// RevokeSession implements Store.
func (s *PostgresStore) RevokeSession(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET revoked_at = $2
		WHERE id = $1 AND revoked_at IS NULL`, id, now)
	return err
}
