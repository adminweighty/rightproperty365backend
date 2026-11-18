package applications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// PostgresStore is the production Store (migration 0031).
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

func pgCode(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// ---------------------------------------------------------------------------
// Forms
// ---------------------------------------------------------------------------

const formColumns = `f.id, f.family_id, f.version, f.status, f.organisation_id, f.owner_user_id, f.market_code, f.name, f.purpose,
	f.privacy_notice, f.retention_days, f.expires_at, f.sections, f.created_by, f.created_by_name, f.created_at, f.updated_at,
	f.published_at, f.retired_at,
	(SELECT count(*) FROM applications a WHERE a.form_id = f.id AND a.submitted_at IS NOT NULL)`

func scanForm(row pgx.Row) (Form, error) {
	var f Form
	var raw []byte
	if err := row.Scan(&f.ID, &f.FamilyID, &f.Version, &f.Status, &f.OrganisationID, &f.OwnerUserID, &f.MarketCode, &f.Name, &f.Purpose,
		&f.PrivacyNotice, &f.RetentionDays, &f.ExpiresAt, &raw, &f.CreatedBy, &f.CreatedByName, &f.CreatedAt, &f.UpdatedAt,
		&f.PublishedAt, &f.RetiredAt, &f.Submissions); err != nil {
		return Form{}, err
	}
	if err := json.Unmarshal(raw, &f.Sections); err != nil {
		return Form{}, fmt.Errorf("decode form sections: %w", err)
	}
	f.CreatedAt, f.UpdatedAt = f.CreatedAt.UTC(), f.UpdatedAt.UTC()
	f.ExpiresAt, f.PublishedAt, f.RetiredAt = utcPtr(f.ExpiresAt), utcPtr(f.PublishedAt), utcPtr(f.RetiredAt)
	return f, nil
}

func sectionsJSON(s []Section) (string, error) {
	if s == nil {
		s = []Section{}
	}
	raw, err := json.Marshal(s)
	return string(raw), err
}

// InsertFormTx writes a form (used by the store and the seed).
func InsertFormTx(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, f Form) error {
	sections, err := sectionsJSON(f.Sections)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO forms (id, family_id, version, status, organisation_id, owner_user_id, market_code, name, purpose, privacy_notice,
			retention_days, expires_at, sections, created_by, created_by_name, created_at, updated_at, published_at, retired_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14, $15, $16, $17, $18, $19)`,
		f.ID, f.FamilyID, f.Version, f.Status, f.OrganisationID, f.OwnerUserID, f.MarketCode, f.Name, f.Purpose, f.PrivacyNotice,
		f.RetentionDays, f.ExpiresAt, sections, f.CreatedBy, f.CreatedByName, f.CreatedAt, f.UpdatedAt, f.PublishedAt, f.RetiredAt); err != nil {
		if pgCode(err) == "23505" {
			return ErrDraftExists
		}
		return fmt.Errorf("insert form: %w", err)
	}
	return nil
}

// CreateForm implements Store.
func (s *PostgresStore) CreateForm(ctx context.Context, f Form) error {
	return InsertFormTx(ctx, s.pool, f)
}

// Form implements Store.
func (s *PostgresStore) Form(ctx context.Context, id uuid.UUID) (Form, error) {
	f, err := scanForm(s.pool.QueryRow(ctx, `SELECT `+formColumns+` FROM forms f WHERE f.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Form{}, ErrFormNotFound
	}
	if err != nil {
		return Form{}, fmt.Errorf("get form: %w", err)
	}
	return f, nil
}

func bookCond(b Book, col string, n int) (string, any) {
	if b.OrganisationID != nil {
		return fmt.Sprintf("%s.organisation_id = $%d", col, n), *b.OrganisationID
	}
	if b.UserID != nil {
		return fmt.Sprintf("%s.organisation_id IS NULL AND %s.owner_user_id = $%d", col, col, n), *b.UserID
	}
	return "false AND $" + fmt.Sprint(n) + "::uuid IS NULL", uuid.Nil
}

// Forms implements Store.
func (s *PostgresStore) Forms(ctx context.Context, book Book, status, market string, familyID *uuid.UUID) ([]Form, error) {
	cond, id := bookCond(book, "f", 1)
	rows, err := s.pool.Query(ctx, `SELECT `+formColumns+` FROM forms f WHERE `+cond+`
		AND ($2 = '' OR f.status = $2) AND ($3 = '' OR f.market_code = $3) AND ($4::uuid IS NULL OR f.family_id = $4)
		ORDER BY f.created_at DESC, f.version DESC`, id, status, market, familyID)
	if err != nil {
		return nil, fmt.Errorf("list forms: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Form, error) { return scanForm(row) })
	if err != nil {
		return nil, fmt.Errorf("list forms: %w", err)
	}
	if out == nil {
		out = []Form{}
	}
	return out, nil
}

// UpdateDraft implements Store.
func (s *PostgresStore) UpdateDraft(ctx context.Context, f Form) error {
	sections, err := sectionsJSON(f.Sections)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE forms SET market_code = $2, name = $3, purpose = $4, privacy_notice = $5, retention_days = $6, expires_at = $7,
		       sections = $8::jsonb, updated_at = $9
		WHERE id = $1 AND status = 'DRAFT'`,
		f.ID, f.MarketCode, f.Name, f.Purpose, f.PrivacyNotice, f.RetentionDays, f.ExpiresAt, sections, f.UpdatedAt)
	if pgCode(err) == "23514" {
		return ErrFormLocked
	}
	if err != nil {
		return fmt.Errorf("update form: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrFormState
	}
	return nil
}

// CreateVersion implements Store.
func (s *PostgresStore) CreateVersion(ctx context.Context, f Form) (Form, error) {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Lock the family so two editors cannot number the same version.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM forms WHERE family_id = $1 FOR UPDATE`, f.FamilyID); err != nil {
			return fmt.Errorf("lock form family: %w", err)
		}
		var draft bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) + 1, bool_or(status = 'DRAFT') IS TRUE FROM forms WHERE family_id = $1`,
			f.FamilyID).Scan(&f.Version, &draft); err != nil {
			return fmt.Errorf("next form version: %w", err)
		}
		if draft {
			return ErrDraftExists
		}
		return InsertFormTx(ctx, tx, f)
	})
	return f, err
}

// Publish implements Store.
func (s *PostgresStore) Publish(ctx context.Context, id uuid.UUID, at time.Time) (Form, error) {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var family uuid.UUID
		var status string
		if err := tx.QueryRow(ctx, `SELECT family_id, status FROM forms WHERE id = $1 FOR UPDATE`, id).Scan(&family, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrFormNotFound
			}
			return fmt.Errorf("lock form: %w", err)
		}
		if status != FormDraft {
			return ErrFormState
		}
		rows, err := tx.Query(ctx, `
			UPDATE forms SET status = 'RETIRED', retired_at = $2, updated_at = $2
			WHERE family_id = $1 AND status = 'PUBLISHED' RETURNING id`, family, at)
		if err != nil {
			return fmt.Errorf("retire previous version: %w", err)
		}
		previous, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return fmt.Errorf("retire previous version: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE forms SET status = 'PUBLISHED', published_at = $2, updated_at = $2 WHERE id = $1`, id, at); err != nil {
			return fmt.Errorf("publish form: %w", err)
		}
		if len(previous) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE listing_application_intake SET form_id = $1 WHERE form_id = ANY($2)`, id, previous); err != nil {
				return fmt.Errorf("move intakes: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return Form{}, err
	}
	return s.Form(ctx, id)
}

// Retire implements Store.
func (s *PostgresStore) Retire(ctx context.Context, id uuid.UUID, at time.Time) (Form, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE forms SET status = 'RETIRED', retired_at = $2, updated_at = $2 WHERE id = $1 AND status <> 'RETIRED'`, id, at)
	if err != nil {
		return Form{}, fmt.Errorf("retire form: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Form(ctx, id); err != nil {
			return Form{}, err
		}
		return Form{}, ErrFormState
	}
	return s.Form(ctx, id)
}

// ---------------------------------------------------------------------------
// Intake
// ---------------------------------------------------------------------------

func scanIntake(row pgx.Row) (Intake, error) {
	var in Intake
	err := row.Scan(&in.ListingID, &in.FormID, &in.Status, &in.ClosesAt, &in.UpdatedBy, &in.UpdatedAt)
	in.ClosesAt, in.UpdatedAt = utcPtr(in.ClosesAt), in.UpdatedAt.UTC()
	return in, err
}

const intakeColumns = `listing_id, form_id, status, closes_at, updated_by, updated_at`

// Intake implements Store.
func (s *PostgresStore) Intake(ctx context.Context, listingID uuid.UUID) (Intake, bool, error) {
	in, err := scanIntake(s.pool.QueryRow(ctx, `SELECT `+intakeColumns+` FROM listing_application_intake WHERE listing_id = $1`, listingID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Intake{}, false, nil
	}
	if err != nil {
		return Intake{}, false, fmt.Errorf("get intake: %w", err)
	}
	return in, true, nil
}

// Intakes implements Store.
func (s *PostgresStore) Intakes(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Intake, error) {
	out := map[uuid.UUID]Intake{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+intakeColumns+` FROM listing_application_intake WHERE listing_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("list intakes: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Intake, error) { return scanIntake(row) })
	if err != nil {
		return nil, fmt.Errorf("list intakes: %w", err)
	}
	for _, in := range list {
		out[in.ListingID] = in
	}
	return out, nil
}

// PutIntakeTx writes an intake (used by the store and the seed).
func PutIntakeTx(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, in Intake) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO listing_application_intake (listing_id, form_id, status, closes_at, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (listing_id) DO UPDATE SET form_id = EXCLUDED.form_id, status = EXCLUDED.status, closes_at = EXCLUDED.closes_at,
			updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
		in.ListingID, in.FormID, in.Status, in.ClosesAt, in.UpdatedBy, in.UpdatedAt); err != nil {
		return fmt.Errorf("put intake: %w", err)
	}
	return nil
}

// PutIntake implements Store.
func (s *PostgresStore) PutIntake(ctx context.Context, in Intake) error {
	return PutIntakeTx(ctx, s.pool, in)
}

// DeleteIntake implements Store.
func (s *PostgresStore) DeleteIntake(ctx context.Context, listingID uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM listing_application_intake WHERE listing_id = $1`, listingID); err != nil {
		return fmt.Errorf("delete intake: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Applications
// ---------------------------------------------------------------------------

const appColumns = `a.id, COALESCE(a.reference, ''), a.listing_id, a.form_id, a.form_version, a.applicant_user_id, a.applicant_name,
	a.organisation_id, a.owner_user_id, a.status, a.answers, a.listing_snapshot, a.created_at, a.updated_at, a.submitted_at,
	a.last_submitted_at, a.decided_at, a.erased_at, f.sections`

type rowScanner interface{ Scan(dest ...any) error }

func scanApp(row rowScanner) (Application, error) {
	var a Application
	var answers, snapshot, sections []byte
	if err := row.Scan(&a.ID, &a.Reference, &a.ListingID, &a.FormID, &a.FormVersion, &a.ApplicantUserID, &a.ApplicantName,
		&a.OrganisationID, &a.OwnerUserID, &a.Status, &answers, &snapshot, &a.CreatedAt, &a.UpdatedAt, &a.SubmittedAt,
		&a.LastSubmittedAt, &a.DecidedAt, &a.ErasedAt, &sections); err != nil {
		return Application{}, err
	}
	var f Form
	if err := json.Unmarshal(sections, &f.Sections); err != nil {
		return Application{}, fmt.Errorf("decode form sections: %w", err)
	}
	var err error
	if a.Answers, err = DecodeAnswers(f, answers); err != nil {
		return Application{}, err
	}
	if len(snapshot) > 0 {
		if err := json.Unmarshal(snapshot, &a.Listing); err != nil {
			return Application{}, fmt.Errorf("decode listing snapshot: %w", err)
		}
	}
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	a.SubmittedAt, a.LastSubmittedAt, a.DecidedAt, a.ErasedAt = utcPtr(a.SubmittedAt), utcPtr(a.LastSubmittedAt), utcPtr(a.DecidedAt), utcPtr(a.ErasedAt)
	return a, nil
}

const appFrom = ` FROM applications a JOIN forms f ON f.id = a.form_id`

// Application implements Store.
func (s *PostgresStore) Application(ctx context.Context, id uuid.UUID) (Application, error) {
	a, err := scanApp(s.pool.QueryRow(ctx, `SELECT `+appColumns+appFrom+` WHERE a.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, ErrNotFound
	}
	if err != nil {
		return Application{}, fmt.Errorf("get application: %w", err)
	}
	return a, nil
}

// Existing implements Store.
func (s *PostgresStore) Existing(ctx context.Context, applicant, listingID, formID uuid.UUID) (Application, bool, error) {
	a, err := scanApp(s.pool.QueryRow(ctx, `SELECT `+appColumns+appFrom+`
		WHERE a.applicant_user_id = $1 AND a.listing_id = $2 AND a.form_id = $3`, applicant, listingID, formID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, false, nil
	}
	if err != nil {
		return Application{}, false, fmt.Errorf("get application: %w", err)
	}
	return a, true, nil
}

func collectApps(rows pgx.Rows) ([]Application, error) {
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Application, error) { return scanApp(row) })
	if out == nil {
		out = []Application{}
	}
	return out, err
}

// Mine implements Store.
func (s *PostgresStore) Mine(ctx context.Context, applicant uuid.UUID, status string, limit, offset int) ([]Application, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM applications WHERE applicant_user_id = $1 AND ($2 = '' OR status = $2)`,
		applicant, status).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count applications: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+appColumns+appFrom+` WHERE a.applicant_user_id = $1 AND ($2 = '' OR a.status = $2)
		ORDER BY COALESCE(a.last_submitted_at, a.updated_at) DESC, a.id DESC LIMIT $3 OFFSET $4`, applicant, status, limit, max(offset, 0))
	if err != nil {
		return nil, 0, fmt.Errorf("list applications: %w", err)
	}
	out, err := collectApps(rows)
	if err != nil {
		return nil, 0, fmt.Errorf("list applications: %w", err)
	}
	return out, total, nil
}

// Received implements Store.
func (s *PostgresStore) Received(ctx context.Context, f AppFilter) ([]Application, int, map[string]int, error) {
	cond, id := bookCond(f.Book, "a", 1)
	where := ` WHERE ` + cond + ` AND a.status <> 'DRAFT'
		AND ($2::uuid[] IS NULL OR a.listing_id = ANY($2))
		AND ($3::uuid IS NULL OR a.listing_id = $3)
		AND ($4 = '' OR a.reference ILIKE $4 OR a.applicant_name ILIKE $4 OR l.title ILIKE $4 OR a.listing_snapshot->>'title' ILIKE $4)`
	q := ""
	if t := strings.TrimSpace(f.Q); t != "" {
		q = "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(t) + "%"
	}
	var listingIDs any
	if f.ListingIDs != nil {
		listingIDs = f.ListingIDs
	}
	from := ` FROM applications a JOIN forms f ON f.id = a.form_id LEFT JOIN listings l ON l.id = a.listing_id`
	args := []any{id, listingIDs, f.ListingID, q}
	counts := map[string]int{}
	rows, err := s.pool.Query(ctx, `SELECT a.status, count(*)`+from+where+` GROUP BY a.status`, args...)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("count applications: %w", err)
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return nil, 0, nil, fmt.Errorf("count applications: %w", err)
		}
		counts[st] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, nil, fmt.Errorf("count applications: %w", err)
	}
	total := 0
	for st, n := range counts {
		if f.Status == "" || st == f.Status {
			total += n
		}
	}
	rows, err = s.pool.Query(ctx, `SELECT `+appColumns+from+where+` AND ($5 = '' OR a.status = $5)
		ORDER BY COALESCE(a.last_submitted_at, a.updated_at) DESC, a.id DESC LIMIT $6 OFFSET $7`,
		append(args, f.Status, f.Limit, max(f.Offset, 0))...)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("list applications: %w", err)
	}
	out, err := collectApps(rows)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("list applications: %w", err)
	}
	return out, total, counts, nil
}

type execQuerier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// InsertApplicationTx writes a new application (used by the store and the seed).
func InsertApplicationTx(ctx context.Context, q execQuerier, a Application) error {
	answers, err := json.Marshal(a.Answers)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(a.Listing)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO applications (id, reference, listing_id, form_id, form_version, applicant_user_id, applicant_name, organisation_id,
			owner_user_id, status, answers, listing_snapshot, created_at, updated_at, submitted_at, last_submitted_at, decided_at, erased_at)
		VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12::jsonb, $13, $14, $15, $16, $17, $18)`,
		a.ID, a.Reference, a.ListingID, a.FormID, a.FormVersion, a.ApplicantUserID, a.ApplicantName, a.OrganisationID, a.OwnerUserID,
		a.Status, string(answers), string(snapshot), a.CreatedAt, a.UpdatedAt, a.SubmittedAt, a.LastSubmittedAt, a.DecidedAt, a.ErasedAt); err != nil {
		if pgCode(err) == "23505" {
			return ErrConflict
		}
		return fmt.Errorf("insert application: %w", err)
	}
	return nil
}

// InsertEventTx writes a timeline event.
func InsertEventTx(ctx context.Context, q execQuerier, e Event) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO application_events (id, application_id, kind, from_status, to_status, message, actor_type, actor_user_id, actor_name, created_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), $7, $8, $9, $10)`,
		e.ID, e.ApplicationID, e.Kind, e.FromStatus, e.ToStatus, e.Message, e.ActorType, e.ActorUserID, e.ActorName, e.At); err != nil {
		return fmt.Errorf("insert application event: %w", err)
	}
	return nil
}

// InsertFileTx writes a file row.
func InsertFileTx(ctx context.Context, q execQuerier, f File) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO application_files (id, application_id, field_key, file_name, content_type, size_bytes, sha256, object_key, uploaded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		f.ID, f.ApplicationID, f.FieldKey, f.FileName, f.ContentType, f.Size, f.SHA256, f.ObjectKey, f.UploadedAt); err != nil {
		return fmt.Errorf("insert application file: %w", err)
	}
	return nil
}

// Apply implements Store.
func (s *PostgresStore) Apply(ctx context.Context, ch Change) (Application, error) {
	var out Application
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if a := ch.Application; a != nil {
			next := *a
			if ch.NextReference && next.Reference == "" {
				var n int64
				if err := tx.QueryRow(ctx, `SELECT nextval('application_reference_seq')`).Scan(&n); err != nil {
					return fmt.Errorf("next application reference: %w", err)
				}
				year := next.CreatedAt.Year()
				if next.LastSubmittedAt != nil {
					year = next.LastSubmittedAt.Year()
				}
				next.Reference = fmt.Sprintf("%s-%d-%06d", ReferencePrefix, year, n)
			}
			if ch.Insert {
				if err := InsertApplicationTx(ctx, tx, next); err != nil {
					return err
				}
			} else {
				answers, err := json.Marshal(next.Answers)
				if err != nil {
					return err
				}
				snapshot, err := json.Marshal(next.Listing)
				if err != nil {
					return err
				}
				tag, err := tx.Exec(ctx, `
					UPDATE applications SET reference = NULLIF($2, ''), status = $3, answers = $4::jsonb, listing_snapshot = $5::jsonb,
					       updated_at = $6, submitted_at = $7, last_submitted_at = $8, decided_at = $9, erased_at = $10,
					       applicant_user_id = $11, applicant_name = $12
					WHERE id = $1 AND ($13::timestamptz IS NULL OR updated_at = $13)`,
					next.ID, next.Reference, next.Status, string(answers), string(snapshot), next.UpdatedAt, next.SubmittedAt,
					next.LastSubmittedAt, next.DecidedAt, next.ErasedAt, next.ApplicantUserID, next.ApplicantName, nullTime(ch.Expect))
				if err != nil {
					return fmt.Errorf("update application: %w", err)
				}
				if tag.RowsAffected() == 0 {
					return ErrConflict
				}
			}
			out = next
		}
		for _, f := range ch.AddFiles {
			if err := InsertFileTx(ctx, tx, f); err != nil {
				return err
			}
		}
		if len(ch.RemoveFiles) > 0 {
			if err := removeFilesTx(ctx, tx, `id = ANY($1)`, ch.RemoveFiles); err != nil {
				return err
			}
		}
		for _, e := range ch.Events {
			if err := InsertEventTx(ctx, tx, e); err != nil {
				return err
			}
		}
		for _, n := range ch.Notes {
			if _, err := tx.Exec(ctx, `
				INSERT INTO application_notes (id, application_id, text, author_user_id, author_name, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
				n.ID, n.ApplicationID, n.Text, n.AuthorUserID, n.AuthorName, n.At); err != nil {
				return fmt.Errorf("insert application note: %w", err)
			}
		}
		for _, o := range ch.Outbox {
			if _, err := tx.Exec(ctx, `
				INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
				VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
				o.ID, o.Type, AggregateApplication, o.Payload.ApplicationID, string(o.PayloadJSON()), o.Payload.OccurredAt); err != nil {
				return fmt.Errorf("insert outbox event %s: %w", o.Type, err)
			}
		}
		for _, e := range ch.Audit {
			if err := orgaudit.Insert(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Application{}, err
	}
	return out, nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// removeFilesTx deletes file rows and queues their objects for the purge.
func removeFilesTx(ctx context.Context, tx pgx.Tx, cond string, arg any) error {
	if _, err := tx.Exec(ctx, `
		WITH gone AS (DELETE FROM application_files WHERE `+cond+` RETURNING object_key)
		INSERT INTO application_file_purges (object_key, queued_at) SELECT object_key, now() FROM gone
		ON CONFLICT (object_key) DO NOTHING`, arg); err != nil {
		return fmt.Errorf("remove application files: %w", err)
	}
	return nil
}

// Files implements Store.
func (s *PostgresStore) Files(ctx context.Context, applicationID uuid.UUID) ([]File, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, application_id, field_key, file_name, content_type, size_bytes, sha256, object_key, uploaded_at
		FROM application_files WHERE application_id = $1 ORDER BY uploaded_at, id`, applicationID)
	if err != nil {
		return nil, fmt.Errorf("list application files: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (File, error) {
		var f File
		err := row.Scan(&f.ID, &f.ApplicationID, &f.FieldKey, &f.FileName, &f.ContentType, &f.Size, &f.SHA256, &f.ObjectKey, &f.UploadedAt)
		f.UploadedAt = f.UploadedAt.UTC()
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("list application files: %w", err)
	}
	if out == nil {
		out = []File{}
	}
	return out, nil
}

// Events implements Store.
func (s *PostgresStore) Events(ctx context.Context, applicationID uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, application_id, kind, COALESCE(from_status, ''), COALESCE(to_status, ''), COALESCE(message, ''), actor_type,
		       actor_user_id, actor_name, created_at
		FROM application_events WHERE application_id = $1 ORDER BY created_at, id`, applicationID)
	if err != nil {
		return nil, fmt.Errorf("list application events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.ID, &e.ApplicationID, &e.Kind, &e.FromStatus, &e.ToStatus, &e.Message, &e.ActorType, &e.ActorUserID, &e.ActorName, &e.At)
		e.At = e.At.UTC()
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("list application events: %w", err)
	}
	return out, nil
}

// Notes implements Store.
func (s *PostgresStore) Notes(ctx context.Context, applicationID uuid.UUID) ([]Note, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, application_id, text, author_user_id, author_name, created_at
		FROM application_notes WHERE application_id = $1 ORDER BY created_at, id`, applicationID)
	if err != nil {
		return nil, fmt.Errorf("list application notes: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Note, error) {
		var n Note
		err := row.Scan(&n.ID, &n.ApplicationID, &n.Text, &n.AuthorUserID, &n.AuthorName, &n.At)
		n.At = n.At.UTC()
		return n, err
	})
	if err != nil {
		return nil, fmt.Errorf("list application notes: %w", err)
	}
	return out, nil
}

// RecordAudit implements Store.
func (s *PostgresStore) RecordAudit(ctx context.Context, events []orgaudit.Event) error {
	for _, e := range events {
		if err := orgaudit.Insert(ctx, s.pool, e); err != nil {
			return err
		}
	}
	return nil
}

// DueForErasure implements Store.
func (s *PostgresStore) DueForErasure(ctx context.Context, now time.Time, limit int) ([]Application, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+appColumns+appFrom+`
		WHERE a.erased_at IS NULL AND a.status IN ('ACCEPTED', 'REJECTED', 'WITHDRAWN') AND a.decided_at IS NOT NULL
		  AND a.decided_at + make_interval(days => f.retention_days) <= $1
		ORDER BY a.decided_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("applications due for erasure: %w", err)
	}
	out, err := collectApps(rows)
	if err != nil {
		return nil, fmt.Errorf("applications due for erasure: %w", err)
	}
	return out, nil
}

// eraseTx removes the answers and files of the applications matched by
// cond and records ERASED; it returns how many it erased.
func eraseTx(ctx context.Context, tx pgx.Tx, cond string, arg any, reason string, at time.Time) (int, error) {
	if _, err := tx.Exec(ctx, `
		WITH gone AS (DELETE FROM application_files WHERE application_id IN (SELECT id FROM applications a WHERE `+cond+` AND a.erased_at IS NULL)
		              RETURNING object_key)
		INSERT INTO application_file_purges (object_key, queued_at) SELECT object_key, $2::timestamptz FROM gone ON CONFLICT (object_key) DO NOTHING`,
		arg, at); err != nil {
		return 0, fmt.Errorf("erase application files: %w", err)
	}
	rows, err := tx.Query(ctx, `
		UPDATE applications a SET answers = '{}'::jsonb, erased_at = $2, updated_at = $2
		WHERE `+cond+` AND a.erased_at IS NULL RETURNING a.id`, arg, at)
	if err != nil {
		return 0, fmt.Errorf("erase applications: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, fmt.Errorf("erase applications: %w", err)
	}
	for _, id := range ids {
		if err := InsertEventTx(ctx, tx, Event{ID: newID(), ApplicationID: id, Kind: EventErased, Message: reason, ActorType: ActorSystem, At: at}); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// Erase implements Store.
func (s *PostgresStore) Erase(ctx context.Context, id uuid.UUID, reason string, at time.Time) (bool, error) {
	n := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		n, err = eraseTx(ctx, tx, `a.id = $1`, id, reason, at)
		return err
	})
	return n > 0, err
}

// EraseApplicantTx is the account deletion of an applicant, inside the
// accounts module's transaction: their DRAFTs are deleted; every other
// application is withdrawn when not final, its answers and files erased
// and the applicant unlinked. Files are queued for the worker's purge.
func EraseApplicantTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, at time.Time) (deleted, anonymised int, err error) {
	if _, err := tx.Exec(ctx, `
		WITH gone AS (DELETE FROM application_files WHERE application_id IN (
		                  SELECT id FROM applications WHERE applicant_user_id = $1 AND status = 'DRAFT') RETURNING object_key)
		INSERT INTO application_file_purges (object_key, queued_at) SELECT object_key, $2::timestamptz FROM gone ON CONFLICT (object_key) DO NOTHING`,
		userID, at); err != nil {
		return 0, 0, fmt.Errorf("delete draft application files: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM applications WHERE applicant_user_id = $1 AND status = 'DRAFT'`, userID)
	if err != nil {
		return 0, 0, fmt.Errorf("delete draft applications: %w", err)
	}
	deleted = int(tag.RowsAffected())
	rows, err := tx.Query(ctx, `
		UPDATE applications SET status = 'WITHDRAWN', decided_at = $2, updated_at = $2
		WHERE applicant_user_id = $1 AND status NOT IN ('ACCEPTED', 'REJECTED', 'WITHDRAWN') RETURNING id`, userID, at)
	if err != nil {
		return 0, 0, fmt.Errorf("withdraw applications: %w", err)
	}
	withdrawn, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, 0, fmt.Errorf("withdraw applications: %w", err)
	}
	for _, id := range withdrawn {
		if err := InsertEventTx(ctx, tx, Event{ID: newID(), ApplicationID: id, Kind: EventWithdrawn, ToStatus: StatusWithdrawn, ActorType: ActorSystem, At: at}); err != nil {
			return 0, 0, err
		}
	}
	if _, err := eraseTx(ctx, tx, `a.applicant_user_id = $1`, userID, ErasedByAccountDeletion, at); err != nil {
		return 0, 0, err
	}
	tag, err = tx.Exec(ctx, `UPDATE applications SET applicant_user_id = NULL, applicant_name = $2 WHERE applicant_user_id = $1`, userID, DeletedApplicantName)
	if err != nil {
		return 0, 0, fmt.Errorf("anonymise applications: %w", err)
	}
	return deleted, int(tag.RowsAffected()), nil
}

// PendingPurges implements Store.
func (s *PostgresStore) PendingPurges(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT object_key FROM application_file_purges ORDER BY queued_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list file purges: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list file purges: %w", err)
	}
	return out, nil
}

// DonePurges implements Store.
func (s *PostgresStore) DonePurges(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM application_file_purges WHERE object_key = ANY($1)`, keys); err != nil {
		return fmt.Errorf("drop file purges: %w", err)
	}
	return nil
}
