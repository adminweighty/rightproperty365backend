package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/inapp"
	"rightproperty365.com/backend/internal/modules/inspections"
	"rightproperty365.com/backend/internal/modules/legal"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/packages"
	"rightproperty365.com/backend/internal/modules/payments"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reviews"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/malware"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// PostgresStore is the production Store. Every change is one transaction
// across the tables it touches (users, sessions, memberships, listings,
// requests, appointments, directory profiles, subscriptions, the outbox and
// both audit trails).
type PostgresStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresStore returns the Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool, now: time.Now}
}

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

// likePattern turns a search term into an ILIKE pattern that matches it
// literally anywhere.
func likePattern(q string) string {
	if q == "" {
		return ""
	}
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q)) + "%"
}

const searchWhere = `
	($1 = '' OR lower(u.full_name) LIKE $1 OR lower(u.email) LIKE $1)
	AND ($2 = '' OR EXISTS (SELECT 1 FROM user_account_roles r WHERE r.user_id = u.id AND r.role = $2)
	             OR EXISTS (SELECT 1 FROM platform_role_assignments a WHERE a.user_id = u.id AND a.role = $2))
	AND ($3 = '' OR u.status = $3)
	AND ($4 = '' OR EXISTS (SELECT 1 FROM memberships m JOIN markets mk ON mk.id = m.market_id
	                        WHERE m.user_id = u.id AND m.status <> 'REMOVED' AND mk.code = $4))`

// Search implements Store.
func (s *PostgresStore) Search(ctx context.Context, f Filter) ([]Summary, int, error) {
	args := []any{likePattern(f.Q), f.Role, f.Status, f.Market}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users u WHERE `+searchWhere, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.full_name, u.email, u.status, u.email_verified_at IS NOT NULL, u.created_at,
		       (SELECT max(se.created_at) FROM sessions se WHERE se.user_id = u.id),
		       COALESCE((SELECT array_agg(r.role ORDER BY r.role) FROM user_account_roles r WHERE r.user_id = u.id), '{}'),
		       COALESCE((SELECT array_agg(DISTINCT a.role) FROM platform_role_assignments a WHERE a.user_id = u.id), '{}'),
		       COALESCE((SELECT array_agg(DISTINCT mk.code) FROM memberships m JOIN markets mk ON mk.id = m.market_id
		                 WHERE m.user_id = u.id AND m.status <> 'REMOVED'), '{}')
		FROM users u WHERE `+searchWhere+`
		ORDER BY u.created_at DESC, u.id DESC LIMIT $5 OFFSET $6`, append(args, limit, max(f.Offset, 0))...)
	if err != nil {
		return nil, 0, fmt.Errorf("search users: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Summary, error) {
		var x Summary
		err := row.Scan(&x.ID, &x.Name, &x.Email, &x.Status, &x.EmailVerified, &x.CreatedAt, &x.LastSignInAt,
			&x.AccountRoles, &x.PlatformRoles, &x.MarketCodes)
		x.CreatedAt = x.CreatedAt.UTC()
		if x.LastSignInAt != nil {
			t := x.LastSignInAt.UTC()
			x.LastSignInAt = &t
		}
		return x, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("search users: %w", err)
	}
	if out == nil {
		out = []Summary{}
	}
	return out, total, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func loadAccount(ctx context.Context, q querier, id uuid.UUID, now time.Time) (Account, error) {
	var a Account
	err := q.QueryRow(ctx, `
		SELECT id, full_name, email, status, email_verified_at IS NOT NULL, created_at, suspended_at, COALESCE(suspension_reason, ''),
		       deleted_at, COALESCE(password_hash, ''), (SELECT max(se.created_at) FROM sessions se WHERE se.user_id = users.id)
		FROM users WHERE id = $1`, id).
		Scan(&a.ID, &a.Name, &a.Email, &a.Status, &a.EmailVerified, &a.CreatedAt, &a.SuspendedAt, &a.SuspensionReason,
			&a.DeletedAt, &a.PasswordHash, &a.LastSignInAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("load user: %w", err)
	}
	a.CreatedAt = a.CreatedAt.UTC()
	for _, t := range []**time.Time{&a.SuspendedAt, &a.DeletedAt, &a.LastSignInAt} {
		if *t != nil {
			utc := (**t).UTC()
			*t = &utc
		}
	}
	rows, err := q.Query(ctx, `
		SELECT a.role, COALESCE(m.code, '') FROM platform_role_assignments a LEFT JOIN markets m ON m.id = a.market_id
		WHERE a.user_id = $1 ORDER BY a.role, m.code`, id)
	if err != nil {
		return Account{}, fmt.Errorf("load platform roles: %w", err)
	}
	if a.PlatformRoles, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (auth.PlatformRole, error) {
		var r auth.PlatformRole
		err := row.Scan(&r.Role, &r.MarketCode)
		return r, err
	}); err != nil {
		return Account{}, fmt.Errorf("load platform roles: %w", err)
	}
	rows, err = q.Query(ctx, `
		SELECT r.role, r.status,
		       COALESCE((SELECT o.status FROM onboarding_applications o WHERE o.user_id = r.user_id AND o.role = r.role
		                 ORDER BY o.created_at DESC, o.id DESC LIMIT 1), '')
		FROM user_account_roles r WHERE r.user_id = $1 ORDER BY r.role`, id)
	if err != nil {
		return Account{}, fmt.Errorf("load account roles: %w", err)
	}
	if a.AccountRoles, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AccountRole, error) {
		var r AccountRole
		err := row.Scan(&r.Role, &r.Status, &r.OnboardingStatus)
		return r, err
	}); err != nil {
		return Account{}, fmt.Errorf("load account roles: %w", err)
	}
	rows, err = q.Query(ctx, `
		SELECT o.id, o.name, o.kind, mk.code, m.role, m.status
		FROM memberships m JOIN organisations o ON o.id = m.organisation_id JOIN markets mk ON mk.id = m.market_id
		WHERE m.user_id = $1 ORDER BY o.name, o.id`, id)
	if err != nil {
		return Account{}, fmt.Errorf("load memberships: %w", err)
	}
	if a.Memberships, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Membership, error) {
		var m Membership
		err := row.Scan(&m.OrganisationID, &m.OrganisationName, &m.OrganisationKind, &m.MarketCode, &m.Role, &m.Status)
		return m, err
	}); err != nil {
		return Account{}, fmt.Errorf("load memberships: %w", err)
	}
	if a.ListingCounts, err = listings.UserListingCounts(ctx, q, id, now); err != nil {
		return Account{}, err
	}
	normalise(&a)
	return a, nil
}

// Account implements Store.
func (s *PostgresStore) Account(ctx context.Context, id uuid.UUID) (Account, error) {
	return loadAccount(ctx, s.pool, id, s.now())
}

// endAccessTx revokes every session of the user and voids their sign-in
// challenges and unused reset and verification tokens.
func endAccessTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, at time.Time) (int, error) {
	tag, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, id, at)
	if err != nil {
		return 0, fmt.Errorf("revoke sessions: %w", err)
	}
	for _, stmt := range []string{
		`DELETE FROM sign_in_challenges WHERE user_id = $1 AND $2::timestamptz IS NOT NULL`,
		`UPDATE password_reset_tokens SET used_at = $2 WHERE user_id = $1 AND used_at IS NULL`,
		`UPDATE email_verification_tokens SET used_at = $2 WHERE user_id = $1 AND used_at IS NULL`,
	} {
		if _, err := tx.Exec(ctx, stmt, id, at); err != nil {
			return 0, fmt.Errorf("end sign-in: %w", err)
		}
	}
	return int(tag.RowsAffected()), nil
}

func insertUserEvent(ctx context.Context, tx pgx.Tx, eventType string, userID uuid.UUID, at time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		VALUES ($1, $2, 'user', $3, jsonb_build_object('user_id', $6::text, 'occurred_at', $4::text), $5, $5)`,
		uuid.New(), eventType, userID, at.UTC().Format(time.RFC3339Nano), at, userID.String()); err != nil {
		return fmt.Errorf("insert outbox event %s: %w", eventType, err)
	}
	return nil
}

func lockStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID) (string, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lock user: %w", err)
	}
	return status, nil
}

// Suspend implements Store.
func (s *PostgresStore) Suspend(ctx context.Context, x Suspension) (Account, error) {
	var out Account
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		status, err := lockStatus(ctx, tx, x.UserID)
		if err != nil {
			return err
		}
		if status != StatusActive {
			return ErrState
		}
		var by *uuid.UUID
		if x.Actor.UserID != uuid.Nil {
			by = &x.Actor.UserID
		}
		if _, err := tx.Exec(ctx, `
			UPDATE users SET status = 'SUSPENDED', suspended_at = $2, suspension_reason = $3, suspended_by = $4, updated_at = $2
			WHERE id = $1`, x.UserID, x.At, x.Reason, by); err != nil {
			return fmt.Errorf("suspend user: %w", err)
		}
		if _, err := endAccessTx(ctx, tx, x.UserID, x.At); err != nil {
			return err
		}
		if _, err := listings.RefreshOwnerListingsTx(ctx, tx, x.UserID, x.At); err != nil {
			return err
		}
		if err := adminaudit.Insert(ctx, tx, x.Audit); err != nil {
			return err
		}
		if err := insertUserEvent(ctx, tx, EventAccountSuspended, x.UserID, x.At); err != nil {
			return err
		}
		out, err = loadAccount(ctx, tx, x.UserID, x.At)
		return err
	})
	return out, err
}

// Reactivate implements Store.
func (s *PostgresStore) Reactivate(ctx context.Context, x Reactivation) (Account, error) {
	var out Account
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		status, err := lockStatus(ctx, tx, x.UserID)
		if err != nil {
			return err
		}
		if status != StatusSuspended {
			return ErrState
		}
		if _, err := tx.Exec(ctx, `
			UPDATE users SET status = 'ACTIVE', suspended_at = NULL, suspension_reason = NULL, suspended_by = NULL, updated_at = $2
			WHERE id = $1`, x.UserID, x.At); err != nil {
			return fmt.Errorf("reactivate user: %w", err)
		}
		if _, err := listings.RefreshOwnerListingsTx(ctx, tx, x.UserID, x.At); err != nil {
			return err
		}
		if err := adminaudit.Insert(ctx, tx, x.Audit); err != nil {
			return err
		}
		if err := insertUserEvent(ctx, tx, EventAccountReactivated, x.UserID, x.At); err != nil {
			return err
		}
		out, err = loadAccount(ctx, tx, x.UserID, x.At)
		return err
	})
	return out, err
}

// soleOwnerSQL names the organisations where the user is the only active
// owner (no other ACTIVE owner with an ACTIVE account) and someone else is
// still a member.
const soleOwnerSQL = `
	SELECT o.name FROM memberships m JOIN organisations o ON o.id = m.organisation_id
	WHERE m.user_id = $1 AND m.role = 'OWNER' AND m.status = 'ACTIVE'
	  AND NOT EXISTS (SELECT 1 FROM memberships m2 JOIN users u2 ON u2.id = m2.user_id
	                  WHERE m2.organisation_id = m.organisation_id AND m2.user_id <> $1 AND m2.role = 'OWNER'
	                    AND m2.status = 'ACTIVE' AND u2.status = 'ACTIVE')
	  AND EXISTS (SELECT 1 FROM memberships m3 JOIN users u3 ON u3.id = m3.user_id
	              WHERE m3.organisation_id = m.organisation_id AND m3.user_id <> $1
	                AND m3.status IN ('ACTIVE', 'INVITED', 'SUSPENDED') AND u3.status <> 'DELETED')
	ORDER BY o.name`

// Delete implements Store.
func (s *PostgresStore) Delete(ctx context.Context, d Deletion) (Deleted, error) {
	var out Deleted
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		out = Deleted{}
		status, err := lockStatus(ctx, tx, d.UserID)
		if err != nil {
			return err
		}
		if status == StatusDeleted {
			return ErrState
		}
		// The owners of the person's organisations are locked too, so a
		// concurrent change cannot leave an organisation without one.
		if _, err := tx.Exec(ctx, `
			SELECT 1 FROM memberships WHERE role = 'OWNER' AND organisation_id IN (SELECT organisation_id FROM memberships WHERE user_id = $1)
			FOR UPDATE`, d.UserID); err != nil {
			return fmt.Errorf("lock owners: %w", err)
		}
		rows, err := tx.Query(ctx, soleOwnerSQL, d.UserID)
		if err != nil {
			return fmt.Errorf("check owners: %w", err)
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("check owners: %w", err)
		}
		if len(names) > 0 {
			return &SoleOwnerError{Organisations: names}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE users SET full_name = $2, email = $3, phone = NULL, password_hash = NULL, marketing_opt_in = false,
			       status = 'DELETED', deleted_at = $4, suspended_at = NULL, suspension_reason = NULL, suspended_by = NULL, updated_at = $4
			WHERE id = $1`, d.UserID, DeletedName, DeletedEmail(d.UserID), d.At); err != nil {
			return fmt.Errorf("anonymise user: %w", err)
		}
		if out.SessionsRevoked, err = endAccessTx(ctx, tx, d.UserID, d.At); err != nil {
			return err
		}

		// Every team: removed, with their open work given back.
		rows, err = tx.Query(ctx, `SELECT organisation_id FROM memberships WHERE user_id = $1 AND status <> 'REMOVED' FOR UPDATE`, d.UserID)
		if err != nil {
			return fmt.Errorf("load memberships: %w", err)
		}
		orgs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return fmt.Errorf("load memberships: %w", err)
		}
		actor := propertyrequests.Actor{UserID: d.Actor.UserID, Name: d.Actor.Name}
		for _, org := range orgs {
			if _, err := tx.Exec(ctx, `UPDATE memberships SET status = 'REMOVED', updated_at = $3 WHERE organisation_id = $1 AND user_id = $2`,
				org, d.UserID, d.At); err != nil {
				return fmt.Errorf("remove membership: %w", err)
			}
			requests, err := propertyrequests.ReleaseMemberTx(ctx, tx, org, d.UserID, actor, d.At)
			if err != nil {
				return err
			}
			appts, err := appointments.ReleaseMemberTx(ctx, tx, org, d.UserID, d.Actor.UserID, d.At)
			if err != nil {
				return err
			}
			if d.TeamAudit != nil {
				if err := orgaudit.Insert(ctx, tx, d.TeamAudit(org, map[string]any{"released_requests": requests, "released_appointments": appts})); err != nil {
					return err
				}
			}
		}
		out.OrganisationsLeft = len(orgs)

		actorKind := listings.ActorReviewer
		if d.SelfService {
			actorKind = listings.ActorAdvertiser
		}
		var actorID *uuid.UUID
		if d.Actor.UserID != uuid.Nil {
			actorID = &d.Actor.UserID
		}
		if out.ListingsWithdrawn, err = listings.WithdrawOwnerListingsTx(ctx, tx, d.UserID, actorID, actorKind, "The advertiser's account was deleted.", d.At); err != nil {
			return err
		}
		if out.ListingsUnassigned, err = listings.UnassignAgentTx(ctx, tx, d.UserID, d.At); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE directory_profiles SET status = 'SUSPENDED', updated_at = $2
			WHERE kind = 'agents' AND agent_user_id = $1 AND status <> 'SUSPENDED'`, d.UserID, d.At); err != nil {
			return fmt.Errorf("hide agent profile: %w", err)
		}

		// The person's own subscriptions end with the account.
		tag, err := tx.Exec(ctx, `
			WITH cancelled AS (
				UPDATE subscriptions SET status = 'CANCELLED', cancelled_at = $2, cancel_reason = 'The account was deleted.',
				       cancelled_by_kind = 'SYSTEM', updated_at = $2
				WHERE subscriber_kind = 'INDIVIDUAL' AND subscriber_id = $1 AND status IN ('PENDING_PAYMENT', 'ACTIVE')
				RETURNING id)
			INSERT INTO subscription_events (id, subscription_id, action, actor_type, note, created_at)
			SELECT gen_random_uuid(), c.id, 'CANCELLED', 'SYSTEM', 'The account was deleted.', $2 FROM cancelled c`, d.UserID, d.At)
		if err != nil {
			return fmt.Errorf("cancel subscriptions: %w", err)
		}
		out.SubscriptionsCanceled = int(tag.RowsAffected())

		if _, err := tx.Exec(ctx, `DELETE FROM user_signatures WHERE user_id = $1`, d.UserID); err != nil {
			return fmt.Errorf("delete saved signature: %w", err)
		}
		// The users row is anonymised, not deleted, so its ON DELETE CASCADE
		// does not fire: the saved listings (migration 0026) go here.
		if _, err := tx.Exec(ctx, `DELETE FROM saved_listings WHERE user_id = $1`, d.UserID); err != nil {
			return fmt.Errorf("delete saved listings: %w", err)
		}
		// Applications (migration 0031): drafts are deleted; the others are
		// withdrawn when open, their answers and files erased (the objects are
		// queued for the worker) and the applicant unlinked.
		if _, _, err := applications.EraseApplicantTx(ctx, tx, d.UserID, d.At); err != nil {
			return err
		}
		// Hourly venue bookings (migration 0032) were meant to go with the
		// users row by cascade, which never fires here: delete them (their
		// events cascade; quotes that used them are unlinked).
		if _, err := tx.Exec(ctx, `DELETE FROM venue_bookings WHERE customer_user_id = $1`, d.UserID); err != nil {
			return fmt.Errorf("delete venue bookings: %w", err)
		}
		// Push devices, in-app notifications (their deliveries cascade) and
		// notification preferences (migration 0034): the same, never by
		// cascade here (FR-045: tokens are revoked on deletion).
		if err := inapp.DeleteUserTx(ctx, tx, d.UserID); err != nil {
			return err
		}
		// Properties and tenancies (migration 0035): a direct owner's personal
		// book is kept (rent records), but its DRAFT tenancies go, the manager
		// name its tenants see is anonymised, and the person is unlinked as
		// creator, activator and ender (ON DELETE SET NULL never fires here).
		if err := tenancies.ForgetUserTx(ctx, tx, d.UserID, DeletedName); err != nil {
			return err
		}
		// Maintenance requests (migration 0037): kept with the book; the
		// person is unlinked, and a tenant's name replaced.
		if err := maintenance.ForgetUserTx(ctx, tx, d.UserID, DeletedName); err != nil {
			return err
		}
		// Inspections (migration 0038): kept with the book; the person is
		// unlinked (the freeze trigger allows exactly this).
		if err := inspections.ForgetUserTx(ctx, tx, d.UserID, DeletedName); err != nil {
			return err
		}
		// Reviews of service providers (migration 0039): kept; a customer's
		// shows "Former customer", the person is unlinked everywhere.
		if err := reviews.ForgetUserTx(ctx, tx, d.UserID, DeletedName); err != nil {
			return err
		}
		// Listing packages (migration 0040): the person's own current and
		// scheduled assignments end; they are unlinked as an assigner.
		if err := packages.ForgetUserTx(ctx, tx, d.UserID, DeletedName, d.At); err != nil {
			return err
		}
		// Online payments (migration 0044): KEPT for finance; the person is
		// unlinked as who opened a checkout or asked for a refund, and their
		// open checkouts expire.
		if err := payments.ForgetUserTx(ctx, tx, d.UserID, DeletedName, d.At); err != nil {
			return err
		}
		// Legal document versions (migration 0045): the person is unlinked as
		// creator and publisher (names replaced); the versions are kept.
		// Legal acceptances (user_consents) are KEPT, as the privacy-notice
		// records always were: the anonymised users row names nobody.
		if err := legal.ForgetUserTx(ctx, tx, d.UserID, DeletedName); err != nil {
			return err
		}
		// Marketing consent records (migration 0041): deleted (the users row
		// above already has marketing_opt_in false).
		if err := identity.DeleteMarketingConsentTx(ctx, tx, d.UserID); err != nil {
			return err
		}
		// An administrator's authenticator and recovery codes (migration
		// 0042): deleted explicitly, the cascade never fires here.
		if _, err := identity.DeleteAdminMFATx(ctx, tx, d.UserID); err != nil {
			return err
		}
		// Scan records of the person's uploads (migration 0043): kept (an
		// INFECTED one stays on the administrators' list); the person is
		// unlinked (ON DELETE SET NULL never fires here).
		if err := malware.ForgetUploaderTx(ctx, tx, d.UserID); err != nil {
			return err
		}
		return adminaudit.Insert(ctx, tx, d.Audit)
	})
	return out, err
}
