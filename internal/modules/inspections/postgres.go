package inspections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// PostgresStore keeps inspections in Postgres (migration 0038).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// NextReference implements Store.
func (s *PostgresStore) NextReference(ctx context.Context, year int) (string, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT nextval('inspection_reference_seq')`).Scan(&n); err != nil {
		return "", fmt.Errorf("next inspection reference: %w", err)
	}
	return fmt.Sprintf("%s-%d-%06d", ReferencePrefix, year, n), nil
}

const inspectionColumns = `i.id, i.reference, i.organisation_id, i.book_user_id, i.property_id, i.unit_id, i.tenancy_id, i.type, i.status,
	i.scheduled_for, i.inspector_user_id, i.inspector_name, i.template_code, i.template_label, i.checklist, i.summary, i.tenant_sign_off,
	i.agent_sign_off, i.cancel_reason, i.version, i.started_at, i.completed_at, i.completed_by, i.completed_by_name, i.cancelled_at,
	i.cancelled_by, i.cancelled_by_name, i.created_by, i.created_by_name, i.created_at, i.updated_at,
	(SELECT count(*) FROM inspection_photos p WHERE p.inspection_id = i.id)`

func scanInspection(row pgx.Row) (Inspection, error) {
	var in Inspection
	var checklist []byte
	if err := row.Scan(&in.ID, &in.Reference, &in.Book.OrganisationID, &in.Book.UserID, &in.PropertyID, &in.UnitID, &in.TenancyID, &in.Type,
		&in.Status, &in.ScheduledFor, &in.InspectorID, &in.InspectorName, &in.TemplateCode, &in.TemplateLabel, &checklist, &in.Summary,
		&in.TenantSignOff, &in.AgentSignOff, &in.CancelReason, &in.Version, &in.StartedAt, &in.CompletedAt, &in.CompletedBy,
		&in.CompletedByName, &in.CancelledAt, &in.CancelledBy, &in.CancelledByName, &in.CreatedBy, &in.CreatedByName, &in.CreatedAt,
		&in.UpdatedAt, &in.PhotoCount); err != nil {
		return Inspection{}, err
	}
	var err error
	if in.Checklist, err = UnmarshalChecklist(checklist); err != nil {
		return Inspection{}, err
	}
	in.ScheduledFor, in.CreatedAt, in.UpdatedAt = in.ScheduledFor.UTC(), in.CreatedAt.UTC(), in.UpdatedAt.UTC()
	for _, t := range []**time.Time{&in.StartedAt, &in.CompletedAt, &in.CancelledAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	return in, nil
}

func (s *PostgresStore) inspections(ctx context.Context, sql string, args ...any) ([]Inspection, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read inspections: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Inspection, error) { return scanInspection(row) })
}

// Inspection implements Store.
func (s *PostgresStore) Inspection(ctx context.Context, id uuid.UUID) (Inspection, error) {
	in, err := scanInspection(s.pool.QueryRow(ctx, `SELECT `+inspectionColumns+` FROM inspections i WHERE i.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Inspection{}, ErrNotFound
	}
	if err != nil {
		return Inspection{}, fmt.Errorf("read inspection: %w", err)
	}
	return in, nil
}

// Inspections implements Store.
func (s *PostgresStore) Inspections(ctx context.Context, book tenancies.Book, f Filter) ([]Inspection, int, map[string]int, error) {
	args := []any{}
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	conds := []string{}
	if book.OrganisationID != nil {
		conds = append(conds, "i.organisation_id = "+add(*book.OrganisationID))
	} else {
		conds = append(conds, "i.book_user_id = "+add(book.ID()))
	}
	if f.Type != "" {
		conds = append(conds, "i.type = "+add(f.Type))
	}
	if f.PropertyID != nil {
		conds = append(conds, "i.property_id = "+add(*f.PropertyID))
	}
	if f.TenancyID != nil {
		conds = append(conds, "i.tenancy_id = "+add(*f.TenancyID))
	}
	if f.From != "" {
		conds = append(conds, "(i.scheduled_for AT TIME ZONE 'UTC')::date >= "+add(f.From)+"::date")
	}
	if f.To != "" {
		conds = append(conds, "(i.scheduled_for AT TIME ZONE 'UTC')::date <= "+add(f.To)+"::date")
	}
	where := strings.Join(conds, " AND ")
	counts := map[string]int{}
	rows, err := s.pool.Query(ctx, `SELECT i.status, count(*) FROM inspections i WHERE `+where+` GROUP BY i.status`, args...)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("count inspections: %w", err)
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return nil, 0, nil, err
		}
		counts[st] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, nil, err
	}
	if f.Status != "" {
		where += " AND i.status = " + add(f.Status)
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM inspections i WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, nil, fmt.Errorf("count inspections: %w", err)
	}
	sql := `SELECT ` + inspectionColumns + ` FROM inspections i WHERE ` + where + ` ORDER BY i.scheduled_for DESC, i.id DESC`
	if f.Limit > 0 {
		sql += " LIMIT " + add(f.Limit) + " OFFSET " + add(f.Offset)
	}
	list, err := s.inspections(ctx, sql, args...)
	return list, total, counts, err
}

// InspectionsOfTenancy implements Store.
func (s *PostgresStore) InspectionsOfTenancy(ctx context.Context, tenancyID uuid.UUID) ([]Inspection, error) {
	return s.inspections(ctx, `SELECT `+inspectionColumns+` FROM inspections i WHERE i.tenancy_id = $1
		ORDER BY i.scheduled_for DESC, i.id DESC`, tenancyID)
}

// Photos implements Store.
func (s *PostgresStore) Photos(ctx context.Context, inspectionID uuid.UUID) ([]Photo, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, inspection_id, item_id, object_key, content_type, width, height, size_bytes, sha256,
		uploaded_by, uploaded_by_name, uploaded_at FROM inspection_photos WHERE inspection_id = $1 ORDER BY uploaded_at, id`, inspectionID)
	if err != nil {
		return nil, fmt.Errorf("read inspection photos: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Photo, error) {
		var p Photo
		err := row.Scan(&p.ID, &p.InspectionID, &p.ItemID, &p.ObjectKey, &p.ContentType, &p.Width, &p.Height, &p.Size, &p.SHA256,
			&p.UploadedBy, &p.UploadedByName, &p.UploadedAt)
		p.UploadedAt = p.UploadedAt.UTC()
		return p, err
	})
}

// Apply implements Store.
func (s *PostgresStore) Apply(ctx context.Context, ch Change) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := ApplyTx(ctx, tx, ch); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ApplyTx writes a Change inside the caller's transaction (see Change for the order).
func ApplyTx(ctx context.Context, tx pgx.Tx, ch Change) error {
	for id, want := range ch.Expect {
		var status string
		var version int
		err := tx.QueryRow(ctx, `SELECT status, version FROM inspections WHERE id = $1 FOR UPDATE`, id).Scan(&status, &version)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (status != want.Status || (!want.AnyVersion && version != want.Version))) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("lock inspection: %w", err)
		}
	}
	if in := ch.Create; in != nil {
		checklist, err := in.Checklist.Marshal()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO inspections (id, reference, organisation_id, book_user_id, property_id, unit_id, tenancy_id, type, status, scheduled_for,
				inspector_user_id, inspector_name, template_code, template_label, checklist, summary, tenant_sign_off, agent_sign_off,
				cancel_reason, version, started_at, completed_at, completed_by, completed_by_name, cancelled_at, cancelled_by,
				cancelled_by_name, created_by, created_by_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::jsonb, $16, $17, $18, $19, $20, $21, $22, $23, $24,
				$25, $26, $27, $28, $29, $30, $31)`,
			in.ID, in.Reference, in.Book.OrganisationID, in.Book.UserID, in.PropertyID, in.UnitID, in.TenancyID, in.Type, in.Status,
			in.ScheduledFor, in.InspectorID, in.InspectorName, in.TemplateCode, in.TemplateLabel, string(checklist), in.Summary,
			in.TenantSignOff, in.AgentSignOff, in.CancelReason, in.Version, in.StartedAt, in.CompletedAt, in.CompletedBy,
			in.CompletedByName, in.CancelledAt, in.CancelledBy, in.CancelledByName, in.CreatedBy, in.CreatedByName, in.CreatedAt,
			in.UpdatedAt); err != nil {
			return fmt.Errorf("insert inspection: %w", err)
		}
	}
	for _, in := range ch.Inspections {
		checklist, err := in.Checklist.Marshal()
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE inspections SET property_id = $2, unit_id = $3, tenancy_id = $4, type = $5, status = $6, scheduled_for = $7,
				inspector_user_id = $8, inspector_name = $9, template_code = $10, template_label = $11, checklist = $12::jsonb,
				summary = $13, tenant_sign_off = $14, agent_sign_off = $15, cancel_reason = $16, version = $17, started_at = $18,
				completed_at = $19, completed_by = $20, completed_by_name = $21, cancelled_at = $22, cancelled_by = $23,
				cancelled_by_name = $24, updated_at = $25
			WHERE id = $1`,
			in.ID, in.PropertyID, in.UnitID, in.TenancyID, in.Type, in.Status, in.ScheduledFor, in.InspectorID, in.InspectorName,
			in.TemplateCode, in.TemplateLabel, string(checklist), in.Summary, in.TenantSignOff, in.AgentSignOff, in.CancelReason,
			in.Version, in.StartedAt, in.CompletedAt, in.CompletedBy, in.CompletedByName, in.CancelledAt, in.CancelledBy,
			in.CancelledByName, in.UpdatedAt)
		if err != nil {
			return fmt.Errorf("update inspection: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
	}
	for id, at := range ch.Touch {
		tag, err := tx.Exec(ctx, `UPDATE inspections SET updated_at = $2 WHERE id = $1`, id, at)
		if err != nil {
			return fmt.Errorf("touch inspection: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
	}
	if len(ch.DeletePhotos) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM inspection_photos WHERE id = ANY($1)`, ch.DeletePhotos); err != nil {
			return fmt.Errorf("delete inspection photos: %w", err)
		}
	}
	locked := map[uuid.UUID]bool{}
	for _, p := range ch.Photos {
		if !locked[p.InspectionID] {
			// One upload at a time per inspection, so the limits hold.
			if _, err := tx.Exec(ctx, `SELECT 1 FROM inspections WHERE id = $1 FOR UPDATE`, p.InspectionID); err != nil {
				return fmt.Errorf("lock inspection: %w", err)
			}
			locked[p.InspectionID] = true
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO inspection_photos (id, inspection_id, item_id, object_key, content_type, width, height, size_bytes, sha256,
				uploaded_by, uploaded_by_name, uploaded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			p.ID, p.InspectionID, p.ItemID, p.ObjectKey, p.ContentType, p.Width, p.Height, p.Size, p.SHA256, p.UploadedBy,
			p.UploadedByName, p.UploadedAt); err != nil {
			return fmt.Errorf("insert inspection photo: %w", err)
		}
	}
	for id := range locked {
		var all, perItem int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(n), 0), COALESCE(max(n), 0) FROM (
				SELECT count(*) AS n FROM inspection_photos WHERE inspection_id = $1 GROUP BY item_id) t`, id).Scan(&all, &perItem); err != nil {
			return fmt.Errorf("count inspection photos: %w", err)
		}
		if all > MaxPhotos || perItem > MaxPhotosPerItem {
			return ErrPhotoLimit
		}
	}
	for _, e := range ch.Audit {
		if err := orgaudit.Insert(ctx, tx, e); err != nil {
			return err
		}
	}
	for _, n := range ch.Notices {
		payload, err := json.Marshal(n.Payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
			uuid.New(), n.Type, AggregateInspection, n.Payload.InspectionID, string(payload), n.Payload.OccurredAt); err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}
	}
	return nil
}

// ForgetUserTx handles a deleted account's rows inside the account
// deletion's transaction (the users row is anonymised, so nothing
// cascades): the person is unlinked as inspector, creator, completer,
// canceller and photo uploader. The names stay (they acted for the book,
// as the ledger's author names do); the freeze trigger allows exactly this
// on completed inspections.
func ForgetUserTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, _ string) error {
	for _, q := range []string{
		`UPDATE inspections SET inspector_user_id = NULL WHERE inspector_user_id = $1`,
		`UPDATE inspections SET created_by = NULL WHERE created_by = $1`,
		`UPDATE inspections SET completed_by = NULL WHERE completed_by = $1`,
		`UPDATE inspections SET cancelled_by = NULL WHERE cancelled_by = $1`,
		`UPDATE inspection_photos SET uploaded_by = NULL WHERE uploaded_by = $1`,
	} {
		if _, err := tx.Exec(ctx, q, userID); err != nil {
			return fmt.Errorf("forget the deleted account's inspection rows: %w", err)
		}
	}
	return nil
}
