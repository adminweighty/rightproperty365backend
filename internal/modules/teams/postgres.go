package teams

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// PostgresStore is the production Store (migration 0021). Every change is
// one short transaction, so it works behind a transaction pooler.
//
// NOTE: compiled but not yet exercised against a real database, like the
// other Postgres stores of this repository.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
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

// memberColumns selects a Member from memberships m JOIN users u JOIN markets mk.
const memberColumns = `u.id, u.full_name, u.email, m.role, mk.code, m.status, u.status = 'ACTIVE', COALESCE(m.job_title, ''), m.created_at`

const memberFrom = ` FROM memberships m JOIN users u ON u.id = m.user_id JOIN markets mk ON mk.id = m.market_id `

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	var joined time.Time
	if err := row.Scan(&m.UserID, &m.Name, &m.Email, &m.Role, &m.MarketCode, &m.Status, &m.AccountActive, &m.JobTitle, &joined); err != nil {
		return Member{}, err
	}
	joined = joined.UTC()
	m.JoinedAt = &joined
	return m, nil
}

// Members implements Store.
func (s *PostgresStore) Members(ctx context.Context, organisationID uuid.UUID) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+memberColumns+memberFrom+`
		WHERE m.organisation_id = $1 AND m.status IN ('ACTIVE', 'SUSPENDED') AND u.status <> 'DELETED'`, organisationID)
	if err != nil {
		return nil, fmt.Errorf("load members: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Member, error) { return scanMember(row) })
	if err != nil {
		return nil, fmt.Errorf("load members: %w", err)
	}
	if out == nil {
		out = []Member{}
	}
	return out, nil
}

// MemberByEmail implements Store.
func (s *PostgresStore) MemberByEmail(ctx context.Context, organisationID uuid.UUID, email string) (*Member, error) {
	m, err := scanMember(s.pool.QueryRow(ctx, `SELECT `+memberColumns+memberFrom+`
		WHERE m.organisation_id = $1 AND lower(u.email) = $2 AND m.status IN ('ACTIVE', 'SUSPENDED')`, organisationID, email))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up member: %w", err)
	}
	return &m, nil
}

// OpenLeads implements Store.
func (s *PostgresStore) OpenLeads(ctx context.Context, organisationID uuid.UUID) (map[uuid.UUID]int, error) {
	return propertyrequests.NewPostgresStore(s.pool).OpenLeads(ctx, organisationID)
}

const invitationColumns = `i.id, i.organisation_id, o.name, o.kind, i.email, i.role, COALESCE(i.job_title, ''), i.status, i.token_hash,
	i.invited_by, i.invited_by_name, i.created_at, i.expires_at, i.accepted_at, i.cancelled_at`

const invitationFrom = ` FROM organisation_invitations i JOIN organisations o ON o.id = i.organisation_id `

func scanInvitation(row pgx.Row) (Invitation, error) {
	var inv Invitation
	err := row.Scan(&inv.ID, &inv.OrganisationID, &inv.OrganisationName, &inv.OrganisationKind, &inv.Email, &inv.Role, &inv.JobTitle,
		&inv.Status, &inv.TokenHash, &inv.InvitedBy, &inv.InvitedByName, &inv.CreatedAt, &inv.ExpiresAt, &inv.AcceptedAt, &inv.CancelledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invitation{}, ErrInvitationNotFound
	}
	if err != nil {
		return Invitation{}, err
	}
	inv.CreatedAt, inv.ExpiresAt = inv.CreatedAt.UTC(), inv.ExpiresAt.UTC()
	return inv, nil
}

// CreateInvitation implements Store.
func (s *PostgresStore) CreateInvitation(ctx context.Context, inv Invitation, audit orgaudit.Event) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE organisation_invitations SET status = 'REPLACED', updated_at = $3
			WHERE organisation_id = $1 AND email = $2 AND status = 'PENDING'`, inv.OrganisationID, inv.Email, inv.CreatedAt); err != nil {
			return fmt.Errorf("replace pending invitation: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO organisation_invitations (id, organisation_id, email, role, job_title, token_hash, status, invited_by, invited_by_name,
				expires_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, 'PENDING', $7, $8, $9, $10, $10)`,
			inv.ID, inv.OrganisationID, inv.Email, inv.Role, inv.JobTitle, inv.TokenHash, inv.InvitedBy, inv.InvitedByName,
			inv.ExpiresAt, inv.CreatedAt); err != nil {
			return fmt.Errorf("insert invitation: %w", err)
		}
		return orgaudit.Insert(ctx, tx, audit)
	})
}

// Invitations implements Store.
func (s *PostgresStore) Invitations(ctx context.Context, organisationID uuid.UUID) ([]Invitation, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+invitationColumns+invitationFrom+`
		WHERE i.organisation_id = $1 AND i.status = 'PENDING' ORDER BY i.created_at DESC, i.id DESC LIMIT 200`, organisationID)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Invitation, error) { return scanInvitation(row) })
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	if out == nil {
		out = []Invitation{}
	}
	return out, nil
}

// Invitation implements Store.
func (s *PostgresStore) Invitation(ctx context.Context, organisationID, id uuid.UUID) (Invitation, error) {
	return scanInvitation(s.pool.QueryRow(ctx, `SELECT `+invitationColumns+invitationFrom+`WHERE i.organisation_id = $1 AND i.id = $2`, organisationID, id))
}

// InvitationByToken implements Store.
func (s *PostgresStore) InvitationByToken(ctx context.Context, tokenHash []byte) (Invitation, error) {
	return scanInvitation(s.pool.QueryRow(ctx, `SELECT `+invitationColumns+invitationFrom+`WHERE i.token_hash = $1`, tokenHash))
}

// CancelInvitation implements Store.
func (s *PostgresStore) CancelInvitation(ctx context.Context, organisationID, id uuid.UUID, by Actor, at time.Time,
	allow func(Invitation) error, audit func(Invitation) orgaudit.Event) (Invitation, error) {
	var out Invitation
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		inv, err := scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+invitationFrom+`
			WHERE i.organisation_id = $1 AND i.id = $2 FOR UPDATE OF i`, organisationID, id))
		if err != nil {
			return err
		}
		if err := allow(inv); err != nil {
			return err
		}
		if inv.Status != InvitationPending {
			return ErrInvitationUsed
		}
		if _, err := tx.Exec(ctx, `
			UPDATE organisation_invitations SET status = 'CANCELLED', cancelled_at = $2, cancelled_by = $3, updated_at = $2 WHERE id = $1`,
			id, at, by.UserID); err != nil {
			return fmt.Errorf("cancel invitation: %w", err)
		}
		inv.Status, inv.CancelledAt = InvitationCancelled, &at
		out = inv
		return orgaudit.Insert(ctx, tx, audit(inv))
	})
	return out, err
}

// AcceptInvitation implements Store.
func (s *PostgresStore) AcceptInvitation(ctx context.Context, a Acceptance) (Accepted, error) {
	var out Accepted
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		out = Accepted{}
		inv, err := scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+invitationFrom+`WHERE i.token_hash = $1 FOR UPDATE OF i`, a.TokenHash))
		if err != nil {
			return err
		}
		var state AccountState
		// The user row is locked so a concurrent change of their roles waits.
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM user_account_roles r WHERE r.user_id = u.id AND r.role IN ('CONSUMER', 'TENANT'))
			FROM users u WHERE u.id = $1 FOR UPDATE OF u`, a.Account.ID).Scan(&state.Seeker); err != nil {
			return fmt.Errorf("load account: %w", err)
		}
		existing, err := scanMember(tx.QueryRow(ctx, `SELECT `+memberColumns+memberFrom+`
			WHERE m.organisation_id = $1 AND m.user_id = $2 AND m.status IN ('ACTIVE', 'SUSPENDED') FOR UPDATE OF m`, inv.OrganisationID, a.Account.ID))
		switch {
		case err == nil:
			state.Membership = &existing
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("load membership: %w", err)
		}
		if err := a.Check(inv, state); err != nil {
			return err
		}
		plan := a.Plan(inv)

		if _, err := tx.Exec(ctx, `
			INSERT INTO memberships (id, organisation_id, user_id, role, status, market_id, job_title, created_at, updated_at)
			SELECT $1, o.id, $3, $4, 'ACTIVE', o.market_id, NULLIF($5, ''), $6, $6 FROM organisations o WHERE o.id = $2
			ON CONFLICT ON CONSTRAINT memberships_org_user_key
			DO UPDATE SET role = EXCLUDED.role, status = 'ACTIVE', market_id = EXCLUDED.market_id,
			    job_title = COALESCE(EXCLUDED.job_title, memberships.job_title), created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at`,
			uuid.New(), inv.OrganisationID, a.Account.ID, inv.Role, inv.JobTitle, a.At); err != nil {
			return fmt.Errorf("insert membership: %w", err)
		}
		if plan.AccountRole != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO user_account_roles (id, user_id, role, status, created_at, updated_at)
				VALUES ($1, $2, $3, 'ACTIVE', $4, $4)
				ON CONFLICT (user_id, role) DO UPDATE SET status = 'ACTIVE', updated_at = EXCLUDED.updated_at`,
				uuid.New(), a.Account.ID, plan.AccountRole, a.At); err != nil {
				return fmt.Errorf("activate account role: %w", err)
			}
		}
		if plan.AgentProfile {
			if out.Profile, err = agentProfileTx(ctx, tx, inv.OrganisationID, a.Account, plan.ProfileTitle, a.At); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE organisation_invitations SET status = 'ACCEPTED', accepted_at = $2, accepted_by = $3, updated_at = $2 WHERE id = $1`,
			inv.ID, a.At, a.Account.ID); err != nil {
			return fmt.Errorf("accept invitation: %w", err)
		}
		inv.Status, inv.AcceptedAt = InvitationAccepted, &a.At
		out.Invitation = inv
		return orgaudit.Insert(ctx, tx, plan.Audit)
	})
	if err != nil {
		return Accepted{}, err
	}
	return out, nil
}

// agentProfileTx gives the new agent a public, verified agent profile linked
// to the agency (or reactivates the one they had), as an approved onboarding
// application does: the agency's city, the organisation's country, no
// contact details.
func agentProfileTx(ctx context.Context, tx pgx.Tx, organisationID uuid.UUID, account Account, title string, at time.Time) (*ProfileRef, error) {
	var ref ProfileRef
	err := tx.QueryRow(ctx, `
		UPDATE directory_profiles SET status = 'ACTIVE', updated_at = $3
		WHERE id = (SELECT id FROM directory_profiles WHERE kind = 'agents' AND organisation_id = $1 AND agent_user_id = $2
		            ORDER BY created_at LIMIT 1)
		RETURNING kind, slug`, organisationID, account.ID, at).Scan(&ref.Kind, &ref.Slug)
	if err == nil {
		return &ref, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("reactivate agent profile: %w", err)
	}
	base := onboarding.Slugify(account.Name)
	slug := base
	for n := 2; ; n++ {
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM directory_profiles WHERE kind = 'agents' AND slug = $1)`, slug).Scan(&taken); err != nil {
			return nil, err
		}
		if !taken {
			break
		}
		if n > 1000 {
			return nil, errors.New("accept invitation: no free profile slug")
		}
		slug = fmt.Sprintf("%s-%d", base, n)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO directory_profiles (id, market_id, kind, slug, name, organisation_id, agent_user_id, status, role_title, city_code,
			verified, sponsored, areas, services, created_at, updated_at, verified_at)
		SELECT $1, o.market_id, 'agents', $2, $3, o.id, $4, 'ACTIVE', NULLIF($5, ''),
		       (SELECT a.city_code FROM directory_profiles a WHERE a.kind = 'agencies' AND a.organisation_id = o.id ORDER BY a.created_at LIMIT 1),
		       true, false, '{}', '{}', $6, $6, $6
		FROM organisations o WHERE o.id = $7`,
		uuid.New(), slug, account.Name, account.ID, title, at, organisationID); err != nil {
		return nil, fmt.Errorf("insert agent profile: %w", err)
	}
	return &ProfileRef{Kind: onboarding.KindAgents, Slug: slug}, nil
}

// ChangeMember implements Store.
func (s *PostgresStore) ChangeMember(ctx context.Context, organisationID, userID uuid.UUID, decide func(Member, int) (MemberChange, error)) (Member, Released, error) {
	var (
		out      Member
		released Released
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		released = Released{}
		// The owners are locked too, so two changes cannot both pass the
		// last-owner rule.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM memberships WHERE organisation_id = $1 AND role = 'OWNER' FOR UPDATE`, organisationID); err != nil {
			return fmt.Errorf("lock owners: %w", err)
		}
		target, err := scanMember(tx.QueryRow(ctx, `SELECT `+memberColumns+memberFrom+`
			WHERE m.organisation_id = $1 AND m.user_id = $2 AND m.status IN ('ACTIVE', 'SUSPENDED') FOR UPDATE OF m`, organisationID, userID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMemberNotFound
		}
		if err != nil {
			return fmt.Errorf("load member: %w", err)
		}
		var owners int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id
			WHERE m.organisation_id = $1 AND m.role = 'OWNER' AND m.status = 'ACTIVE' AND u.status = 'ACTIVE'`, organisationID).Scan(&owners); err != nil {
			return fmt.Errorf("count owners: %w", err)
		}
		change, err := decide(target, owners)
		if err != nil {
			return err
		}
		next := target
		if change.Role != nil {
			next.Role = *change.Role
		}
		if change.Status != nil {
			next.Status = *change.Status
		}
		if change.JobTitle != nil {
			next.JobTitle = *change.JobTitle
		}
		status := next.Status
		if change.Remove {
			status = MemberRemoved
		}
		if _, err := tx.Exec(ctx, `
			UPDATE memberships SET role = $3, status = $4, job_title = NULLIF($5, ''), updated_at = $6
			WHERE organisation_id = $1 AND user_id = $2`,
			organisationID, userID, next.Role, status, next.JobTitle, change.At); err != nil {
			return fmt.Errorf("update membership: %w", err)
		}
		actor := propertyrequests.Actor{UserID: change.Actor.UserID, Name: change.Actor.Name}
		if change.Release >= ReleaseRequests {
			if released.Requests, err = propertyrequests.ReleaseMemberTx(ctx, tx, organisationID, userID, actor, change.At); err != nil {
				return err
			}
		}
		if change.Release >= ReleaseAll {
			if released.Appointments, err = appointments.ReleaseMemberTx(ctx, tx, organisationID, userID, change.Actor.UserID, change.At); err != nil {
				return err
			}
		}
		for _, e := range change.Events {
			if err := orgaudit.Insert(ctx, tx, withReleased(e, change.Release, released)); err != nil {
				return err
			}
		}
		out = next
		if change.Remove {
			out.Status = MemberRemoved
		}
		return nil
	})
	if err != nil {
		return Member{}, Released{}, err
	}
	return out, released, nil
}

// Audit implements Store.
func (s *PostgresStore) Audit(ctx context.Context, organisationID uuid.UUID, limit, offset int) ([]orgaudit.Event, int, error) {
	return orgaudit.List(ctx, s.pool, organisationID, limit, offset)
}
