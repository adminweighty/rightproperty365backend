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
// Account roles live in user_account_roles (0003_account_roles).
const (
	uniqueViolation      = "23505"
	usersEmailConstraint = "users_email_lower_key"
)

// PostgresStore is the production Store. Statements use no session state and
// explicit short transactions, so they work on a plain PostgreSQL connection
// and equally behind a transaction pooler (DB_POOL_MODE=transaction).
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

// replaceAccountRoles makes roles the user's complete set of account roles.
func replaceAccountRoles(ctx context.Context, q querier, u NewUser) error {
	if _, err := q.Exec(ctx, `DELETE FROM user_account_roles WHERE user_id = $1`, u.ID); err != nil {
		return fmt.Errorf("clear account roles: %w", err)
	}
	for _, r := range u.AccountRoles {
		if _, err := q.Exec(ctx, `
			INSERT INTO user_account_roles (id, user_id, role, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $5)`,
			uuid.New(), u.ID, r.Role, r.Status, u.CreatedAt); err != nil {
			return fmt.Errorf("insert account role: %w", err)
		}
	}
	// Onboarding: every role that waits for verification and has an outcome
	// gets a DRAFT application, in this same transaction. Roles are only
	// replaced while the account is unverified, so the only applications an
	// account can have here are drafts nobody has touched.
	if _, err := q.Exec(ctx, `DELETE FROM onboarding_applications WHERE user_id = $1 AND status = 'DRAFT'`, u.ID); err != nil {
		return fmt.Errorf("clear onboarding drafts: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO onboarding_applications (id, user_id, role, status, created_at, updated_at)
		SELECT gen_random_uuid(), r.user_id, r.role, 'DRAFT', $2, $2
		FROM user_account_roles r
		JOIN onboarding_role_outcomes o ON o.role = r.role
		WHERE r.user_id = $1 AND r.status = 'PENDING_VERIFICATION'
		  AND NOT EXISTS (SELECT 1 FROM onboarding_applications a
		                  WHERE a.user_id = r.user_id AND a.role = r.role AND a.status <> 'REJECTED')`,
		u.ID, u.CreatedAt); err != nil {
		return fmt.Errorf("create onboarding drafts: %w", err)
	}
	return nil
}

// insertOutbox writes an identity event into the transactional outbox.
func insertOutbox(ctx context.Context, q querier, e OutboxEvent) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
		uuid.New(), e.Type, AggregateUser, e.UserID, string(e.Payload()), e.OccurredAt); err != nil {
		return fmt.Errorf("insert outbox event %s: %w", e.Type, err)
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
		if err := replaceAccountRoles(ctx, tx, u); err != nil {
			return err
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
		if err := replaceAccountRoles(ctx, tx, u); err != nil {
			return err
		}
		return insertConsent(ctx, tx, u)
	})
}

const userColumns = `id, email, full_name, status, COALESCE(password_hash, ''), email_verified_at, created_at`

func (s *PostgresStore) loadUser(ctx context.Context, where string, arg any) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE `+where, arg).
		Scan(&u.ID, &u.Email, &u.Name, &u.Status, &u.PasswordHash, &u.EmailVerifiedAt, &u.CreatedAt)
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

	// Self-declared account roles (informational; never used for authorisation).
	rows, err = s.pool.Query(ctx, `
		SELECT r.role, r.status,
		       COALESCE((SELECT a.status FROM onboarding_applications a
		                 WHERE a.user_id = r.user_id AND a.role = r.role
		                 ORDER BY a.created_at DESC, a.id DESC LIMIT 1), '')
		FROM user_account_roles r WHERE r.user_id = $1 ORDER BY r.role`, u.ID)
	if err != nil {
		return User{}, fmt.Errorf("load account roles: %w", err)
	}
	u.AccountRoles, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AccountRole, error) {
		var r AccountRole
		err := row.Scan(&r.Role, &r.Status, &r.OnboardingStatus)
		return r, err
	})
	if err != nil {
		return User{}, fmt.Errorf("load account roles: %w", err)
	}
	sortAccountRoles(u.AccountRoles)
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

// ApprovedDirectOwners implements Store.
func (s *PostgresStore) ApprovedDirectOwners(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT r.user_id) FROM user_account_roles r JOIN users u ON u.id = r.user_id
		WHERE u.status = 'ACTIVE' AND r.status = 'ACTIVE' AND r.role IN ('PRIVATE_SELLER', 'LANDLORD')`).Scan(&n)
	return n, err
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
		// Only a row that was not verified before is updated, which tells us
		// whether this is the first verification (and so whether to welcome).
		tag, err := tx.Exec(ctx, `
			UPDATE users SET email_verified_at = $2, updated_at = $2
			WHERE id = $1 AND email_verified_at IS NULL`, userID, now)
		if err != nil {
			return fmt.Errorf("mark email verified: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return insertOutbox(ctx, tx, OutboxEvent{Type: EventUserEmailVerified, UserID: userID, OccurredAt: now})
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
		return insertOutbox(ctx, tx, OutboxEvent{Type: EventPasswordChanged, UserID: userID, OccurredAt: now})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return userID, nil
}

// CreateSession implements Store.
func (s *PostgresStore) CreateSession(ctx context.Context, sess Session, events ...OutboxEvent) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO sessions (id, user_id, token_hash, delivery, created_at, last_seen_at, expires_at, scope)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			sess.ID, sess.UserID, sess.TokenHash, sess.Delivery, sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt, sess.Scope); err != nil {
			return fmt.Errorf("insert session: %w", err)
		}
		for _, e := range events {
			if err := insertOutbox(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
}

// SessionByTokenHash implements Store.
func (s *PostgresStore) SessionByTokenHash(ctx context.Context, tokenHash []byte) (Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, token_hash, delivery, created_at, last_seen_at, expires_at, revoked_at, scope
		FROM sessions WHERE token_hash = $1`, tokenHash).
		Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &sess.Delivery, &sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt, &sess.RevokedAt, &sess.Scope)
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

// CreatePlatformAdmin creates a verified administrator account holding one
// platform role, in one transaction. It is used only by cmd/adminctl; no HTTP
// endpoint can reach it. ErrEmailTaken if the email already has an account,
// ErrMarketNotFound if the market code is unknown.
//
// NOTE: compiled but not yet exercised against a real database.
func (s *PostgresStore) CreatePlatformAdmin(ctx context.Context, a NewPlatformAdmin) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var marketID *uuid.UUID
		if a.MarketCode != "" {
			var id uuid.UUID
			err := tx.QueryRow(ctx, `SELECT id FROM markets WHERE code = $1`, a.MarketCode).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrMarketNotFound
			}
			if err != nil {
				return fmt.Errorf("look up market: %w", err)
			}
			marketID = &id
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO users (id, email, full_name, status, password_hash, email_verified_at, created_at, updated_at)
			VALUES ($1, $2, $3, 'ACTIVE', $4, $5, $5, $5)`,
			a.ID, a.Email, a.Name, a.PasswordHash, a.CreatedAt)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == usersEmailConstraint {
				return ErrEmailTaken
			}
			return fmt.Errorf("insert user: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO platform_role_assignments (id, user_id, role, market_id, created_at)
			VALUES ($1, $2, $3, $4, $5)`,
			uuid.New(), a.ID, a.Role, marketID, a.CreatedAt); err != nil {
			return fmt.Errorf("insert platform role: %w", err)
		}
		return nil
	})
}

// SetPlatformAdminPassword replaces an administrator's password, invalidates
// their outstanding reset tokens and revokes all their sessions, in one
// transaction. ErrNotFound if no such account, ErrNotPlatformAdmin if it holds
// no platform role (customers use the password-reset flow instead).
//
// NOTE: compiled but not yet exercised against a real database.
func (s *PostgresStore) SetPlatformAdminPassword(ctx context.Context, email, passwordHash string, now time.Time) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var (
			userID  uuid.UUID
			isAdmin bool
		)
		err := tx.QueryRow(ctx, `
			SELECT u.id, EXISTS (SELECT 1 FROM platform_role_assignments a WHERE a.user_id = u.id)
			FROM users u WHERE lower(u.email) = $1
			FOR UPDATE OF u`, email).Scan(&userID, &isAdmin)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("look up user: %w", err)
		}
		if !isAdmin {
			return ErrNotPlatformAdmin
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = $3 WHERE id = $1`, userID, passwordHash, now); err != nil {
			return fmt.Errorf("set password: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE password_reset_tokens SET used_at = $2 WHERE user_id = $1 AND used_at IS NULL`, userID, now); err != nil {
			return fmt.Errorf("invalidate reset tokens: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, now); err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		return insertOutbox(ctx, tx, OutboxEvent{Type: EventPasswordChanged, UserID: userID, OccurredAt: now})
	})
}
