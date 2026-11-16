package leases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the production Store (migration 0022). Every change is
// one short transaction, so it works behind a transaction pooler.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
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

func uniqueViolation(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && (constraint == "" || pg.ConstraintName == constraint)
}

func jsonText(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func nullableJSON(v any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	s, err := jsonText(v)
	return &s, err
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

const templateColumns = `id, family_id, version, market_code, name, lease_kind, description, sections, optional_clauses, status,
	legal_review, created_by, created_by_name, approved_at, approved_by, approved_by_name, retired_at, created_at, updated_at,
	organisation_id`

func scanTemplate(row pgx.Row) (Template, error) {
	var t Template
	var sections, clauses, review []byte
	err := row.Scan(&t.ID, &t.FamilyID, &t.Version, &t.MarketCode, &t.Name, &t.LeaseKind, &t.Description, &sections, &clauses, &t.Status,
		&review, &t.CreatedBy, &t.CreatedByName, &t.ApprovedAt, &t.ApprovedBy, &t.ApprovedByName, &t.RetiredAt, &t.CreatedAt, &t.UpdatedAt,
		&t.OrganisationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, ErrTemplateNotFound
	}
	if err != nil {
		return Template{}, err
	}
	if err := json.Unmarshal(sections, &t.Sections); err != nil {
		return Template{}, fmt.Errorf("read template sections: %w", err)
	}
	if err := json.Unmarshal(clauses, &t.Clauses); err != nil {
		return Template{}, fmt.Errorf("read template clauses: %w", err)
	}
	if len(review) > 0 && string(review) != "null" {
		var r LegalReview
		if err := json.Unmarshal(review, &r); err != nil {
			return Template{}, fmt.Errorf("read legal review: %w", err)
		}
		t.LegalReview = &r
	}
	t.CreatedAt, t.UpdatedAt, t.ApprovedAt, t.RetiredAt = t.CreatedAt.UTC(), t.UpdatedAt.UTC(), utc(t.ApprovedAt), utc(t.RetiredAt)
	return t, nil
}

func templateArgs(t Template) ([]any, error) {
	sections, err := jsonText(t.Sections)
	if err != nil {
		return nil, err
	}
	clauses := t.Clauses
	if clauses == nil {
		clauses = []Clause{}
	}
	clauseText, err := jsonText(clauses)
	if err != nil {
		return nil, err
	}
	var review *string
	if t.LegalReview != nil {
		if review, err = nullableJSON(t.LegalReview); err != nil {
			return nil, err
		}
	}
	return []any{t.ID, t.FamilyID, t.Version, t.MarketCode, t.Name, t.LeaseKind, t.Description, sections, clauseText, t.Status,
		review, t.CreatedBy, t.CreatedByName, t.ApprovedAt, t.ApprovedBy, t.ApprovedByName, t.RetiredAt, t.CreatedAt, t.UpdatedAt,
		t.OrganisationID}, nil
}

func insertTemplate(ctx context.Context, q querier, t Template) error {
	args, err := templateArgs(t)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO lease_templates (`+templateColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`, args...)
	if uniqueViolation(err, "lease_templates_one_draft_key") {
		return ErrTemplateDraftExists
	}
	return err
}

func updateTemplate(ctx context.Context, q querier, t Template) error {
	args, err := templateArgs(t)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `UPDATE lease_templates SET family_id = $2, version = $3, market_code = $4, name = $5, lease_kind = $6,
		description = $7, sections = $8, optional_clauses = $9, status = $10, legal_review = $11, created_by = $12, created_by_name = $13,
		approved_at = $14, approved_by = $15, approved_by_name = $16, retired_at = $17, created_at = $18, updated_at = $19,
		organisation_id = $20
		WHERE id = $1`, args...)
	return err
}

func insertTemplateEvents(ctx context.Context, q querier, events []TemplateEvent) error {
	for _, e := range events {
		detail, err := nullableJSON(e.Detail)
		if e.Detail == nil {
			detail = nil
		}
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `INSERT INTO lease_template_events (id, template_id, action, actor_user_id, actor_name, detail, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, e.ID, e.TemplateID, e.Action, e.ActorUserID, e.ActorName, detail, e.At); err != nil {
			return fmt.Errorf("insert template event: %w", err)
		}
	}
	return nil
}

// CreateTemplate implements Store.
func (s *PostgresStore) CreateTemplate(ctx context.Context, t Template, ev TemplateEvent) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := insertTemplate(ctx, tx, t); err != nil {
			return fmt.Errorf("insert template: %w", err)
		}
		return insertTemplateEvents(ctx, tx, []TemplateEvent{ev})
	})
}

func orgs(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}

// Template implements Store.
func (s *PostgresStore) Template(ctx context.Context, id uuid.UUID) (Template, error) {
	return scanTemplate(s.pool.QueryRow(ctx, `SELECT `+templateColumns+` FROM lease_templates WHERE id = $1`, id))
}

// Templates implements Store.
func (s *PostgresStore) Templates(ctx context.Context, f TemplateFilter) ([]Template, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+templateColumns+` FROM lease_templates
		WHERE ($1 = '' OR market_code = $1) AND ($2 = '' OR status = $2) AND ($3::uuid IS NULL OR family_id = $3)
			AND (NOT $4 OR ($5 AND organisation_id IS NULL) OR organisation_id = ANY($6::uuid[]))
		ORDER BY market_code, name, version DESC`, f.MarketCode, f.Status, f.FamilyID, f.Scoped, f.Samples, orgs(f.Organisations))
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Template, error) { return scanTemplate(row) })
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}
	if out == nil {
		out = []Template{}
	}
	return out, nil
}

// ChangeTemplate implements Store.
func (s *PostgresStore) ChangeTemplate(ctx context.Context, id uuid.UUID, change func(t Template) (Template, []TemplateEvent, error)) (Template, error) {
	var out Template
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		t, err := scanTemplate(tx.QueryRow(ctx, `SELECT `+templateColumns+` FROM lease_templates WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		next, events, err := change(t)
		if err != nil {
			return err
		}
		next.ID = id
		if err := updateTemplate(ctx, tx, next); err != nil {
			return fmt.Errorf("update template: %w", err)
		}
		out = next
		return insertTemplateEvents(ctx, tx, events)
	})
	return out, err
}

// CreateTemplateVersion implements Store.
func (s *PostgresStore) CreateTemplateVersion(ctx context.Context, t Template, ev TemplateEvent) (Template, error) {
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var highest int
		var drafts int
		// Locks the family's rows: two editors cannot both create a version.
		rows, err := tx.Query(ctx, `SELECT version, status FROM lease_templates WHERE family_id = $1 FOR UPDATE`, t.FamilyID)
		if err != nil {
			return fmt.Errorf("lock template family: %w", err)
		}
		for rows.Next() {
			var v int
			var status string
			if err := rows.Scan(&v, &status); err != nil {
				rows.Close()
				return err
			}
			highest = max(highest, v)
			if status == TemplateDraft {
				drafts++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if drafts > 0 {
			return ErrTemplateDraftExists
		}
		t.Version = highest + 1
		if err := insertTemplate(ctx, tx, t); err != nil {
			if errors.Is(err, ErrTemplateDraftExists) {
				return err
			}
			return fmt.Errorf("insert template version: %w", err)
		}
		if ev.Detail == nil {
			ev.Detail = map[string]any{}
		}
		ev.Detail["version"] = t.Version
		ev.TemplateID = t.ID
		return insertTemplateEvents(ctx, tx, []TemplateEvent{ev})
	})
	if err != nil {
		return Template{}, err
	}
	return t, nil
}

// ApproveTemplate implements Store.
func (s *PostgresStore) ApproveTemplate(ctx context.Context, id uuid.UUID, review *LegalReview, by *uuid.UUID, byName string, at time.Time,
	events func(t Template, retired []Template) []TemplateEvent) (Template, error) {
	var out Template
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		t, err := scanTemplate(tx.QueryRow(ctx, `SELECT `+templateColumns+` FROM lease_templates WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if t.Status != TemplateDraft {
			return ErrTemplateState
		}
		rows, err := tx.Query(ctx, `SELECT `+templateColumns+` FROM lease_templates
			WHERE family_id = $1 AND status = 'APPROVED' AND id <> $2 FOR UPDATE`, t.FamilyID, id)
		if err != nil {
			return fmt.Errorf("load approved versions: %w", err)
		}
		retired, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Template, error) { return scanTemplate(row) })
		if err != nil {
			return fmt.Errorf("load approved versions: %w", err)
		}
		// Retire first: at most one APPROVED version per family (unique index).
		for i := range retired {
			retired[i].Status, retired[i].RetiredAt, retired[i].UpdatedAt = TemplateRetired, &at, at
			if err := updateTemplate(ctx, tx, retired[i]); err != nil {
				return fmt.Errorf("retire template version: %w", err)
			}
		}
		t.Status, t.LegalReview, t.ApprovedAt, t.ApprovedBy, t.ApprovedByName, t.UpdatedAt = TemplateApproved, review, &at, by, byName, at
		if err := updateTemplate(ctx, tx, t); err != nil {
			return fmt.Errorf("approve template: %w", err)
		}
		out = t
		return insertTemplateEvents(ctx, tx, events(t, retired))
	})
	return out, err
}

// TemplateEvents implements Store.
func (s *PostgresStore) TemplateEvents(ctx context.Context, templateID uuid.UUID) ([]TemplateEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, template_id, action, actor_user_id, actor_name, detail, created_at
		FROM lease_template_events WHERE template_id = $1 ORDER BY created_at, id`, templateID)
	if err != nil {
		return nil, fmt.Errorf("load template events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TemplateEvent, error) {
		var e TemplateEvent
		var detail []byte
		if err := row.Scan(&e.ID, &e.TemplateID, &e.Action, &e.ActorUserID, &e.ActorName, &detail, &e.At); err != nil {
			return TemplateEvent{}, err
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &e.Detail)
		}
		e.At = e.At.UTC()
		return e, nil
	})
	if err != nil {
		return nil, fmt.Errorf("load template events: %w", err)
	}
	if out == nil {
		out = []TemplateEvent{}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Leases
// ---------------------------------------------------------------------------

// NextReference implements Store.
func (s *PostgresStore) NextReference(ctx context.Context, year int) (string, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT nextval('lease_reference_seq')`).Scan(&n); err != nil {
		return "", fmt.Errorf("next lease reference: %w", err)
	}
	return fmt.Sprintf("%s-%d-%06d", ReferencePrefix, year, n), nil
}

const leaseColumns = `id, reference, status, template_id, template_version, template_name, market_code, lease_kind, organisation_id,
	organisation_name, listing_id, listing_reference, listing_title, created_by, created_by_name, input_snapshot, agreement_id, sent_at, sent_by,
	completed_at, declined_at, cancelled_at, cancelled_by, cancel_reason, created_at, updated_at`

func scanLease(row pgx.Row, extra ...any) (Lease, error) {
	var l Lease
	var input []byte
	var createdBy *uuid.UUID
	dest := []any{&l.ID, &l.Reference, &l.Status, &l.TemplateID, &l.TemplateVersion, &l.TemplateName, &l.MarketCode, &l.LeaseKind,
		&l.OrganisationID, &l.OrganisationName, &l.ListingID, &l.ListingReference, &l.ListingTitle, &createdBy, &l.CreatedByName, &input,
		&l.AgreementID, &l.SentAt, &l.SentBy, &l.CompletedAt, &l.DeclinedAt, &l.CancelledAt, &l.CancelledBy, &l.CancelReason, &l.CreatedAt, &l.UpdatedAt}
	err := row.Scan(append(dest, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrLeaseNotFound
	}
	if err != nil {
		return Lease{}, err
	}
	if createdBy != nil {
		l.CreatedBy = *createdBy
	}
	if err := json.Unmarshal(input, &l.Input); err != nil {
		return Lease{}, fmt.Errorf("read lease input: %w", err)
	}
	l.CreatedAt, l.UpdatedAt = l.CreatedAt.UTC(), l.UpdatedAt.UTC()
	l.SentAt, l.CompletedAt, l.DeclinedAt, l.CancelledAt = utc(l.SentAt), utc(l.CompletedAt), utc(l.DeclinedAt), utc(l.CancelledAt)
	l.Documents = map[string]Document{}
	return l, nil
}

// children loads the documents of the leases.
func children(ctx context.Context, q querier, leases []Lease) error {
	if len(leases) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(leases))
	index := map[uuid.UUID]int{}
	for i, l := range leases {
		ids = append(ids, l.ID)
		index[l.ID] = i
	}
	rows, err := q.Query(ctx, `SELECT lease_id, kind, template_id, template_version, input_snapshot, object_key, sha256, size_bytes, created_at
		FROM lease_documents WHERE lease_id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("load lease documents: %w", err)
	}
	for rows.Next() {
		var leaseID uuid.UUID
		var d Document
		if err := rows.Scan(&leaseID, &d.Kind, &d.TemplateID, &d.TemplateVersion, &d.Snapshot, &d.ObjectKey, &d.SHA256, &d.Size, &d.GeneratedAt); err != nil {
			rows.Close()
			return fmt.Errorf("load lease documents: %w", err)
		}
		d.GeneratedAt = d.GeneratedAt.UTC()
		leases[index[leaseID]].Documents[d.Kind] = d
	}
	rows.Close()
	return rows.Err()
}

func loadLease(ctx context.Context, q querier, id uuid.UUID, lock bool) (Lease, error) {
	sql := `SELECT ` + leaseColumns + ` FROM leases WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	l, err := scanLease(q.QueryRow(ctx, sql, id))
	if err != nil {
		return Lease{}, err
	}
	list := []Lease{l}
	if err := children(ctx, q, list); err != nil {
		return Lease{}, err
	}
	return list[0], nil
}

func insertEvents(ctx context.Context, q querier, events []Event) error {
	for _, e := range events {
		var detail *string
		if e.Detail != nil {
			var err error
			if detail, err = nullableJSON(e.Detail); err != nil {
				return err
			}
		}
		if _, err := q.Exec(ctx, `INSERT INTO lease_events (id, lease_id, action, actor_type, actor_user_id, actor_name, detail, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, e.ID, e.LeaseID, e.Action, e.ActorType, e.ActorUserID, e.ActorName, detail, e.At); err != nil {
			return fmt.Errorf("insert lease event: %w", err)
		}
	}
	return nil
}

func creator(l Lease) *uuid.UUID {
	if l.CreatedBy == uuid.Nil {
		return nil
	}
	id := l.CreatedBy
	return &id
}

// saveChildren writes the documents.
func saveChildren(ctx context.Context, q querier, l Lease) error {
	for _, d := range l.Documents {
		if _, err := q.Exec(ctx, `
			INSERT INTO lease_documents (id, lease_id, kind, template_id, template_version, input_snapshot, object_key, sha256, size_bytes, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (lease_id, kind) DO UPDATE SET template_id = EXCLUDED.template_id, template_version = EXCLUDED.template_version,
				input_snapshot = EXCLUDED.input_snapshot, object_key = EXCLUDED.object_key, sha256 = EXCLUDED.sha256,
				size_bytes = EXCLUDED.size_bytes, created_at = EXCLUDED.created_at
			WHERE lease_documents.kind = 'DRAFT' OR lease_documents.kind = 'FINAL'`,
			newID(), l.ID, d.Kind, d.TemplateID, d.TemplateVersion, string(d.Snapshot), d.ObjectKey, d.SHA256, d.Size, d.GeneratedAt); err != nil {
			return fmt.Errorf("save lease document: %w", err)
		}
	}
	return nil
}

// CreateLease implements Store.
func (s *PostgresStore) CreateLease(ctx context.Context, l Lease, events []Event) error {
	input, err := jsonText(l.Input)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO leases (`+leaseColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)`,
			l.ID, l.Reference, l.Status, l.TemplateID, l.TemplateVersion, l.TemplateName, l.MarketCode, l.LeaseKind, l.OrganisationID,
			l.OrganisationName, l.ListingID, l.ListingReference, l.ListingTitle, creator(l), l.CreatedByName, input, l.AgreementID, l.SentAt, l.SentBy,
			l.CompletedAt, l.DeclinedAt, l.CancelledAt, l.CancelledBy, l.CancelReason, l.CreatedAt, l.UpdatedAt)
		if uniqueViolation(err, "leases_reference_key") {
			return ErrReferenceTaken
		}
		if err != nil {
			return fmt.Errorf("insert lease: %w", err)
		}
		if err := saveChildren(ctx, tx, l); err != nil {
			return err
		}
		return insertEvents(ctx, tx, events)
	})
}

// Lease implements Store.
func (s *PostgresStore) Lease(ctx context.Context, id uuid.UUID) (Lease, error) {
	return loadLease(ctx, s.pool, id, false)
}

// Leases implements Store.
func (s *PostgresStore) Leases(ctx context.Context, f LeaseFilter) ([]Lease, int, error) {
	orgs, corgs := f.Organisations, f.CreatorOrganisations
	if orgs == nil {
		orgs = []uuid.UUID{}
	}
	if corgs == nil {
		corgs = []uuid.UUID{}
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 1 << 30
	}
	rows, err := s.pool.Query(ctx, `SELECT `+leaseColumns+`, count(*) OVER () FROM leases
		WHERE ($1 = '' OR status = $1)
		  AND ($2 OR organisation_id = ANY($3)
		       OR (created_by = $4 AND (organisation_id = ANY($5) OR ($6 AND organisation_id IS NULL))))
		ORDER BY created_at DESC, id DESC LIMIT $7 OFFSET $8`,
		f.Status, f.All, orgs, f.Creator, corgs, f.CreatorDirect, limit, f.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("load leases: %w", err)
	}
	total := 0
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Lease, error) { return scanLease(row, &total) })
	if err != nil {
		return nil, 0, fmt.Errorf("load leases: %w", err)
	}
	if out == nil {
		out = []Lease{}
	}
	if err := children(ctx, s.pool, out); err != nil {
		return nil, 0, err
	}
	if len(out) == 0 && f.Offset > 0 {
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM leases
			WHERE ($1 = '' OR status = $1)
			  AND ($2 OR organisation_id = ANY($3)
			       OR (created_by = $4 AND (organisation_id = ANY($5) OR ($6 AND organisation_id IS NULL))))`,
			f.Status, f.All, orgs, f.Creator, corgs, f.CreatorDirect).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count leases: %w", err)
		}
	}
	return out, total, nil
}

// ChangeLease implements Store.
func (s *PostgresStore) ChangeLease(ctx context.Context, id uuid.UUID, change func(l *Lease) ([]Event, error)) (Lease, error) {
	var out Lease
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		l, err := loadLease(ctx, tx, id, true)
		if err != nil {
			return err
		}
		events, err := change(&l)
		if err != nil {
			return err
		}
		l.ID = id
		input, err := jsonText(l.Input)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE leases SET status = $2, template_id = $3, template_version = $4, template_name = $5,
				lease_kind = $6, input_snapshot = $7, sent_at = $8, sent_by = $9, completed_at = $10, declined_at = $11,
				cancelled_at = $12, cancelled_by = $13, cancel_reason = $14, updated_at = $15, agreement_id = $16
			WHERE id = $1`, id, l.Status, l.TemplateID, l.TemplateVersion, l.TemplateName, l.LeaseKind, input, l.SentAt, l.SentBy,
			l.CompletedAt, l.DeclinedAt, l.CancelledAt, l.CancelledBy, l.CancelReason, l.UpdatedAt, l.AgreementID); err != nil {
			return fmt.Errorf("update lease: %w", err)
		}
		if err := saveChildren(ctx, tx, l); err != nil {
			return err
		}
		if err := insertEvents(ctx, tx, events); err != nil {
			return err
		}
		out = l
		return nil
	})
	return out, err
}

// DeleteLease implements Store.
func (s *PostgresStore) DeleteLease(ctx context.Context, id uuid.UUID) (Lease, error) {
	var out Lease
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		l, err := loadLease(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if l.Status != StatusDraft {
			return ErrLeaseNotEditable
		}
		if _, err := tx.Exec(ctx, `DELETE FROM leases WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete lease: %w", err)
		}
		out = l
		return nil
	})
	return out, err
}

// Events implements Store.
func (s *PostgresStore) Events(ctx context.Context, leaseID uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, lease_id, action, actor_type, actor_user_id, actor_name, detail, created_at
		FROM lease_events WHERE lease_id = $1 ORDER BY created_at, id`, leaseID)
	if err != nil {
		return nil, fmt.Errorf("load lease events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		var detail []byte
		if err := row.Scan(&e.ID, &e.LeaseID, &e.Action, &e.ActorType, &e.ActorUserID, &e.ActorName, &detail, &e.At); err != nil {
			return Event{}, err
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &e.Detail)
		}
		e.At = e.At.UTC()
		return e, nil
	})
	if err != nil {
		return nil, fmt.Errorf("load lease events: %w", err)
	}
	if out == nil {
		out = []Event{}
	}
	return out, nil
}
