package onboarding

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/auth"
)

// PostgresStore is the Postgres adapter (migrations/0009_onboarding.up.sql).
//
// NOTE: compiled but never run against a real database. An approval is one
// transaction here: the application, the account role, the organisation, the
// membership, the profile, the history row and the outbox event commit
// together or not at all.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the Postgres adapter.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

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
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Outcomes implements Store.
func (s *PostgresStore) Outcomes(ctx context.Context) ([]RoleOutcome, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.role, o.outcome, COALESCE(o.organisation_kind, ''), COALESCE(o.directory_kind, ''), COALESCE(o.membership_role, ''),
		       COALESCE(o.specialism, '')
		FROM onboarding_role_outcomes o
		LEFT JOIN reference_items r ON r.set_code = 'account_roles' AND r.code = o.role
		ORDER BY COALESCE(r.group_sort_order, 0), COALESCE(r.sort_order, 0), o.role`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RoleOutcome, error) {
		var o RoleOutcome
		err := row.Scan(&o.Role, &o.Outcome, &o.OrganisationKind, &o.DirectoryKind, &o.MembershipRole, &o.Specialism)
		return o, err
	})
}

// Requirements implements Store.
func (s *PostgresStore) Requirements(ctx context.Context) ([]Requirement, error) {
	rows, err := s.pool.Query(ctx, `SELECT role, document_code, required FROM onboarding_role_documents ORDER BY role, document_code`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Requirement, error) {
		var r Requirement
		err := row.Scan(&r.Role, &r.Document, &r.Required)
		return r, err
	})
}

const applicationColumns = `a.id, a.user_id, a.role, a.status, a.business_name, a.tagline, a.description, a.city_code, a.areas, a.services,
	a.trade_code, a.registration_number, a.contact_phone, a.agency_slug, COALESCE(a.agency_name, ''), a.job_title, a.bio,
	COALESCE(a.market_code, ''), COALESCE(a.decision_reason, ''), a.decided_at, a.submitted_at, a.profile_kind, a.profile_slug,
	a.created_at, a.updated_at, u.full_name, u.email, u.email_verified_at IS NOT NULL, u.created_at, a.country_code, a.city_name`

const applicationFrom = ` FROM onboarding_applications a JOIN users u ON u.id = a.user_id `

func scanApplication(row pgx.CollectableRow) (Application, error) {
	var a Application
	var profileKind, profileSlug *string
	if err := row.Scan(&a.ID, &a.UserID, &a.Role, &a.Status, &a.BusinessName, &a.Tagline, &a.Description, &a.City, &a.Areas, &a.Services,
		&a.Trade, &a.RegistrationNumber, &a.ContactPhone, &a.AgencySlug, &a.AgencyName, &a.JobTitle, &a.Bio,
		&a.MarketCode, &a.DecisionReason, &a.DecidedAt, &a.SubmittedAt, &profileKind, &profileSlug,
		&a.CreatedAt, &a.UpdatedAt, &a.Applicant.Name, &a.Applicant.Email, &a.Applicant.EmailVerified, &a.Applicant.CreatedAt, &a.Country, &a.CityName); err != nil {
		return Application{}, err
	}
	a.Applicant.ID = a.UserID
	if profileKind != nil && profileSlug != nil {
		a.Profile = &ProfileRef{Kind: *profileKind, Slug: *profileSlug}
	}
	return a, nil
}

// applications runs a query and attaches each application's documents.
func (s *PostgresStore) applications(ctx context.Context, sql string, args ...any) ([]Application, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	apps, err := pgx.CollectRows(rows, scanApplication)
	if err != nil || len(apps) == 0 {
		return apps, err
	}
	ids := make([]uuid.UUID, 0, len(apps))
	index := map[uuid.UUID]int{}
	for i, a := range apps {
		ids = append(ids, a.ID)
		index[a.ID] = i
	}
	rows, err = s.pool.Query(ctx, `
		SELECT id, application_id, document_code, file_name, content_type, size_bytes, sha256, object_key, uploaded_at
		FROM onboarding_documents WHERE application_id = ANY($1) ORDER BY uploaded_at DESC, id`, ids)
	if err != nil {
		return nil, fmt.Errorf("load documents: %w", err)
	}
	docs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Document, error) {
		var d Document
		err := row.Scan(&d.ID, &d.ApplicationID, &d.Type, &d.FileName, &d.ContentType, &d.Size, &d.SHA256, &d.ObjectKey, &d.UploadedAt)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("load documents: %w", err)
	}
	for _, d := range docs {
		i := index[d.ApplicationID]
		apps[i].Documents = append(apps[i].Documents, d)
	}
	return apps, nil
}

// Current implements Store.
func (s *PostgresStore) Current(ctx context.Context, userID uuid.UUID) ([]Application, error) {
	return s.applications(ctx, `
		SELECT DISTINCT ON (a.role) `+applicationColumns+applicationFrom+`
		WHERE a.user_id = $1
		ORDER BY a.role, a.created_at DESC, a.id DESC`, userID)
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Application, error) {
	apps, err := s.applications(ctx, `SELECT `+applicationColumns+applicationFrom+` WHERE a.id = $1`, id)
	if err != nil {
		return Application{}, err
	}
	if len(apps) == 0 {
		return Application{}, ErrNotFound
	}
	return apps[0], nil
}

// Reapply implements Store.
func (s *PostgresStore) Reapply(ctx context.Context, app Application) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `
			SELECT status FROM onboarding_applications WHERE user_id = $1 AND role = $2
			ORDER BY created_at DESC, id DESC LIMIT 1 FOR UPDATE`, app.UserID, app.Role).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != StatusRejected) {
			return ErrInvalidState
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO onboarding_applications (id, user_id, role, status, created_at, updated_at)
			VALUES ($1, $2, $3, 'DRAFT', $4, $4)`, app.ID, app.UserID, app.Role, app.CreatedAt); err != nil {
			return fmt.Errorf("insert application: %w", err)
		}
		return setAccountRole(ctx, tx, app.UserID, app.Role, RoleStatusPending, app.CreatedAt)
	})
}

func setAccountRole(ctx context.Context, q querier, userID uuid.UUID, role, status string, now time.Time) error {
	tag, err := q.Exec(ctx, `UPDATE user_account_roles SET status = $3, updated_at = $4 WHERE user_id = $1 AND role = $2`, userID, role, status, now)
	if err != nil {
		return fmt.Errorf("update account role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update account role: %w", ErrNotFound)
	}
	return nil
}

// notEditable turns "no row was changed" into ErrNotFound or ErrInvalidState.
func notEditable(ctx context.Context, q querier, id uuid.UUID) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM onboarding_applications WHERE id = $1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return ErrInvalidState
}

const editableStatuses = `('DRAFT', 'CHANGES_REQUESTED')`

// SaveDetails implements Store.
func (s *PostgresStore) SaveDetails(ctx context.Context, id uuid.UUID, d Details, marketCode, agencyName string, now time.Time) error {
	areas, services := d.Areas, d.Services
	if areas == nil {
		areas = []string{}
	}
	if services == nil {
		services = []string{}
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE onboarding_applications SET
			business_name = $2, tagline = $3, description = $4, city_code = $5, areas = $6, services = $7, trade_code = $8,
			registration_number = $9, contact_phone = $10, agency_slug = $11, agency_name = NULLIF($12, ''), job_title = $13, bio = $14,
			market_code = NULLIF($15, ''), updated_at = $16, country_code = $17, city_name = $18
		WHERE id = $1 AND status IN `+editableStatuses,
		id, d.BusinessName, d.Tagline, d.Description, d.City, areas, services, d.Trade,
		d.RegistrationNumber, d.ContactPhone, d.AgencySlug, agencyName, d.JobTitle, d.Bio, marketCode, now, d.Country, d.CityName)
	if err != nil {
		return fmt.Errorf("save details: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return notEditable(ctx, s.pool, id)
	}
	return nil
}

// lockEditable locks the application row and checks it is editable.
func lockEditable(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM onboarding_applications WHERE id = $1 FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != StatusDraft && status != StatusChangesRequested {
		return ErrInvalidState
	}
	return nil
}

// AddDocument implements Store.
func (s *PostgresStore) AddDocument(ctx context.Context, doc Document) ([]string, error) {
	var replaced []string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockEditable(ctx, tx, doc.ApplicationID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			DELETE FROM onboarding_documents WHERE application_id = $1 AND document_code = $2 RETURNING object_key`,
			doc.ApplicationID, doc.Type)
		if err != nil {
			return fmt.Errorf("replace document: %w", err)
		}
		if replaced, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return fmt.Errorf("replace document: %w", err)
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM onboarding_documents WHERE application_id = $1`, doc.ApplicationID).Scan(&count); err != nil {
			return err
		}
		if count >= MaxDocumentsPerApp {
			return ErrTooManyDocuments
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO onboarding_documents (id, application_id, document_code, file_name, content_type, size_bytes, sha256, object_key, uploaded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			doc.ID, doc.ApplicationID, doc.Type, doc.FileName, doc.ContentType, doc.Size, doc.SHA256, doc.ObjectKey, doc.UploadedAt); err != nil {
			return fmt.Errorf("insert document: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE onboarding_applications SET updated_at = $2 WHERE id = $1`, doc.ApplicationID, doc.UploadedAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return replaced, nil
}

// DeleteDocument implements Store.
func (s *PostgresStore) DeleteDocument(ctx context.Context, applicationID, documentID uuid.UUID, now time.Time) (string, error) {
	var key string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockEditable(ctx, tx, applicationID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
			DELETE FROM onboarding_documents WHERE id = $1 AND application_id = $2 RETURNING object_key`, documentID, applicationID).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDocumentNotFound
		}
		if err != nil {
			return fmt.Errorf("delete document: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE onboarding_applications SET updated_at = $2 WHERE id = $1`, applicationID, now)
		return err
	})
	return key, err
}

func insertHistory(ctx context.Context, q querier, id uuid.UUID, from, to string, actor uuid.UUID, reason string, at time.Time) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO onboarding_history (id, application_id, from_status, to_status, actor_user_id, reason, created_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7)`, uuid.New(), id, from, to, actor, reason, at); err != nil {
		return fmt.Errorf("insert history: %w", err)
	}
	return nil
}

func insertOutbox(ctx context.Context, q querier, event string, id uuid.UUID, at time.Time) error {
	payload := fmt.Sprintf(`{"application_id":%q,"occurred_at":%q}`, id, at.UTC().Format(time.RFC3339Nano))
	if _, err := q.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`, uuid.New(), event, AggregateApplication, id, payload, at); err != nil {
		return fmt.Errorf("insert outbox event %s: %w", event, err)
	}
	return nil
}

// lock locks the application row and returns its owner, role and status.
func lock(ctx context.Context, tx pgx.Tx, id uuid.UUID) (userID uuid.UUID, role, status string, err error) {
	err = tx.QueryRow(ctx, `SELECT user_id, role, status FROM onboarding_applications WHERE id = $1 FOR UPDATE`, id).Scan(&userID, &role, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return userID, role, status, err
}

// Transition implements Store.
func (s *PostgresStore) Transition(ctx context.Context, t Transition) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		userID, role, status, err := lock(ctx, tx, t.ApplicationID)
		if err != nil {
			return err
		}
		ok := false
		for _, from := range t.From {
			ok = ok || from == status
		}
		if !ok {
			return ErrInvalidState
		}
		if t.To == StatusSubmitted {
			_, err = tx.Exec(ctx, `
				UPDATE onboarding_applications SET status = $2, submitted_at = $3, decision_reason = NULL, decided_at = NULL, updated_at = $3
				WHERE id = $1`, t.ApplicationID, t.To, t.At)
		} else {
			_, err = tx.Exec(ctx, `
				UPDATE onboarding_applications SET status = $2, decision_reason = NULLIF($4, ''), decided_at = $3, updated_at = $3
				WHERE id = $1`, t.ApplicationID, t.To, t.At, t.Reason)
		}
		if err != nil {
			return fmt.Errorf("update application: %w", err)
		}
		if t.AccountRoleStatus != "" {
			if err := setAccountRole(ctx, tx, userID, role, t.AccountRoleStatus, t.At); err != nil {
				return err
			}
		}
		if err := insertHistory(ctx, tx, t.ApplicationID, status, t.To, t.ActorID, t.Reason, t.At); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, t.Event, t.ApplicationID, t.At)
	})
}

// Approve implements Store.
func (s *PostgresStore) Approve(ctx context.Context, p Approval) (Created, error) {
	var created Created
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		created = Created{}
		userID, role, status, err := lock(ctx, tx, p.ApplicationID)
		if err != nil {
			return err
		}
		if status != StatusSubmitted || userID != p.UserID || role != p.Role {
			return ErrInvalidState
		}
		marketID := func(code string) (uuid.UUID, error) {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM markets WHERE code = $1`, code).Scan(&id); err != nil {
				return uuid.Nil, fmt.Errorf("market %q: %w", code, err)
			}
			return id, nil
		}
		if p.AgencySlug != "" && p.Membership != nil {
			// The agency must still be public and verified when this commits.
			var ok bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM directory_profiles d JOIN organisations o ON o.id = d.organisation_id
				               WHERE d.kind = 'agencies' AND d.slug = $1 AND d.organisation_id = $2
				                 AND d.status = 'ACTIVE' AND d.verified AND o.status = 'ACTIVE')`,
				p.AgencySlug, p.Membership.OrganisationID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return ErrAgencyNotVerified
			}
		}
		if p.Organisation != nil {
			market, err := marketID(p.Organisation.MarketCode)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO organisations (id, market_id, name, kind, status, created_at, updated_at)
				VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $5)`,
				p.Organisation.ID, market, p.Organisation.Name, p.Organisation.Kind, p.At); err != nil {
				return fmt.Errorf("insert organisation: %w", err)
			}
			id := p.Organisation.ID
			created.OrganisationID = &id
		}
		if p.Membership != nil {
			market, err := marketID(p.Membership.MarketCode)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO memberships (id, organisation_id, user_id, role, status, market_id, created_at, updated_at)
				VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $6, $6)
				ON CONFLICT ON CONSTRAINT memberships_org_user_key
				DO UPDATE SET role = EXCLUDED.role, status = 'ACTIVE', market_id = EXCLUDED.market_id, updated_at = EXCLUDED.updated_at`,
				uuid.New(), p.Membership.OrganisationID, p.UserID, p.Membership.Role, market, p.At); err != nil {
				return fmt.Errorf("insert membership: %w", err)
			}
			membership := p.Membership.Role
			created.MembershipRole = &membership
		}
		var profileKind, profileSlug *string
		if p.Profile != nil {
			market, err := marketID(p.Profile.MarketCode)
			if err != nil {
				return err
			}
			// A unique slug: the wanted one, then -2, -3, ... The unique
			// constraint is the backstop against a concurrent approval.
			slug := p.Profile.Slug
			for n := 2; ; n++ {
				var taken bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM directory_profiles WHERE kind = $1 AND slug = $2)`, p.Profile.Kind, slug).Scan(&taken); err != nil {
					return err
				}
				if !taken {
					break
				}
				if n > 1000 {
					return errors.New("approve onboarding: no free profile slug")
				}
				slug = fmt.Sprintf("%s-%d", p.Profile.Slug, n)
			}
			areas, services := p.Profile.Areas, p.Profile.Services
			if areas == nil {
				areas = []string{}
			}
			if services == nil {
				services = []string{}
			}
			// The logo columns stay NULL when the applicant attached no logo.
			logo := directory.Logo{}
			var logoKey *string
			var logoAt *time.Time
			if p.Profile.Logo != nil {
				logo, logoKey, logoAt = *p.Profile.Logo, &p.Profile.Logo.ObjectKey, &p.At
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO directory_profiles (id, market_id, kind, slug, name, organisation_id, agent_user_id, status,
					role_title, tagline, about, city_code, trade_code, verified, sponsored, areas, services, created_at, updated_at, specialism,
					city_name, verified_at, logo_object_key, logo_content_type, logo_width, logo_height, logo_checksum, logo_size_bytes, logo_updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 'ACTIVE', NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''),
					true, false, $13, $14, $15, $15, NULLIF($16, ''), NULLIF($17, ''), $15,
					$18, NULLIF($19, ''), NULLIF($20, 0), NULLIF($21, 0), NULLIF($22, ''), NULLIF($23::bigint, 0), $24)`,
				p.Profile.ID, market, p.Profile.Kind, slug, p.Profile.Name, p.Profile.OrganisationID, p.Profile.AgentUserID,
				p.Profile.RoleTitle, p.Profile.Tagline, p.Profile.Description, p.Profile.CityCode, p.Profile.TradeCode,
				areas, services, p.At, p.Profile.Specialism, p.Profile.CityName,
				logoKey, logo.ContentType, logo.Width, logo.Height, logo.Checksum, logo.Size, logoAt); err != nil {
				return fmt.Errorf("insert profile: %w", err)
			}
			created.Profile = &ProfileRef{Kind: p.Profile.Kind, Slug: slug}
			profileKind, profileSlug = &created.Profile.Kind, &created.Profile.Slug
		}
		if err := setAccountRole(ctx, tx, p.UserID, p.Role, RoleStatusActive, p.At); err != nil {
			return err
		}
		if slices.Contains(auth.DirectOwnerRoles(), p.Role) {
			// A direct owner's listings become public with this approval: each is
			// touched (the search index orders writes by updated_at) and the
			// index is told, in this same transaction.
			if _, err := tx.Exec(ctx, `
				WITH touched AS (UPDATE listings SET updated_at = $2 WHERE seller_user_id = $1 AND organisation_id IS NULL RETURNING id)
				INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
				SELECT gen_random_uuid(), $3, $4, t.id, jsonb_build_object('listing_id', t.id), $2, $2 FROM touched t`,
				p.UserID, p.At, listings.EventListingUpdated, listings.AggregateListing); err != nil {
				return fmt.Errorf("refresh the owner's listings: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE onboarding_applications
			SET status = 'APPROVED', decision_reason = NULLIF($2, ''), decided_at = $3, updated_at = $3, profile_kind = $4, profile_slug = $5
			WHERE id = $1`, p.ApplicationID, p.Note, p.At, profileKind, profileSlug); err != nil {
			return fmt.Errorf("update application: %w", err)
		}
		if err := insertHistory(ctx, tx, p.ApplicationID, StatusSubmitted, StatusApproved, p.ActorID, p.Note, p.At); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, EventApproved, p.ApplicationID, p.At)
	})
	if err != nil {
		return Created{}, err
	}
	return created, nil
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, f ListFilter) ([]Application, int, map[string]int, error) {
	conds, args := []string{`true`}, []any{}
	add := func(format string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(format, len(args)))
	}
	if f.Role != "" {
		add(`a.role = $%d`, f.Role)
	}
	if f.Markets != nil {
		add(`a.market_code = ANY($%d)`, f.Markets)
	}
	if f.Q != "" {
		add(`(u.full_name || ' ' || u.email || ' ' || COALESCE(a.business_name, '')) ILIKE $%d`, "%"+likeEscaper.Replace(f.Q)+"%")
	}
	scope := strings.Join(conds, " AND ")

	counts := map[string]int{}
	rows, err := s.pool.Query(ctx, `SELECT a.status, count(*)`+applicationFrom+`WHERE `+scope+` GROUP BY a.status`, args...)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("count applications: %w", err)
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return nil, 0, nil, err
		}
		counts[status] = n
	}
	if err := rows.Err(); err != nil {
		return nil, 0, nil, err
	}

	args = append(args, f.Status, f.Limit, f.Offset)
	apps, err := s.applications(ctx, `SELECT `+applicationColumns+applicationFrom+`WHERE `+scope+
		fmt.Sprintf(` AND a.status = $%d ORDER BY COALESCE(a.submitted_at, a.created_at), a.id LIMIT $%d OFFSET $%d`, len(args)-2, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("list applications: %w", err)
	}
	return apps, counts[f.Status], counts, nil
}

// History implements Store.
func (s *PostgresStore) History(ctx context.Context, id uuid.UUID) ([]HistoryEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT h.from_status, h.to_status, COALESCE(h.actor_user_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       COALESCE(u.full_name, ''), COALESCE(h.reason, ''), h.created_at
		FROM onboarding_history h LEFT JOIN users u ON u.id = h.actor_user_id
		WHERE h.application_id = $1 ORDER BY h.created_at, h.id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (HistoryEntry, error) {
		var h HistoryEntry
		err := row.Scan(&h.From, &h.To, &h.ActorID, &h.ActorName, &h.Reason, &h.At)
		return h, err
	})
}
