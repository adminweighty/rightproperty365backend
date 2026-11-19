package maintenance

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

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/money"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// PostgresStore keeps maintenance requests in Postgres (migration 0037).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// NextReference implements Store.
func (s *PostgresStore) NextReference(ctx context.Context, year int) (string, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT nextval('maintenance_reference_seq')`).Scan(&n); err != nil {
		return "", fmt.Errorf("next maintenance reference: %w", err)
	}
	return fmt.Sprintf("%s-%d-%06d", ReferencePrefix, year, n), nil
}

const requestColumns = `r.id, r.reference, r.organisation_id, r.book_user_id, r.property_id, r.unit_id, r.tenancy_id, r.category,
	r.category_label, r.priority, r.priority_label, r.title, r.description, r.status, r.raised_by_kind, r.raised_by, r.raised_by_name,
	r.provider_profile_id, COALESCE(r.provider_kind, ''), COALESCE(r.provider_slug, ''), r.provider_name, r.enquiry_id,
	COALESCE(r.enquiry_reference, ''), r.assignments, r.assigned_at, r.assigned_by, r.assigned_by_name, r.status_changed_at,
	r.resolved_at, r.closed_at, r.created_at, r.updated_at,
	(SELECT count(*) FROM maintenance_photos p WHERE p.request_id = r.id),
	(SELECT count(*) FROM maintenance_events e WHERE e.request_id = r.id AND e.type = 'COMMENT')`

func scanRequest(row pgx.Row) (Request, error) {
	var r Request
	var providerName *string
	var providerID *uuid.UUID
	var providerKind, providerSlug string
	if err := row.Scan(&r.ID, &r.Reference, &r.Book.OrganisationID, &r.Book.UserID, &r.PropertyID, &r.UnitID, &r.TenancyID, &r.Category,
		&r.CategoryLabel, &r.Priority, &r.PriorityLabel, &r.Title, &r.Description, &r.Status, &r.RaisedByKind, &r.RaisedBy, &r.RaisedByName,
		&providerID, &providerKind, &providerSlug, &providerName, &r.EnquiryID, &r.EnquiryReference, &r.Assignments, &r.AssignedAt,
		&r.AssignedBy, &r.AssignedByName, &r.StatusChangedAt, &r.ResolvedAt, &r.ClosedAt, &r.CreatedAt, &r.UpdatedAt,
		&r.PhotoCount, &r.CommentCount); err != nil {
		return Request{}, err
	}
	if providerName != nil {
		r.Provider = &Provider{ProfileID: providerID, Kind: providerKind, Slug: providerSlug, Name: *providerName}
	}
	r.StatusChangedAt, r.CreatedAt, r.UpdatedAt = r.StatusChangedAt.UTC(), r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	for _, t := range []**time.Time{&r.AssignedAt, &r.ResolvedAt, &r.ClosedAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	return r, nil
}

func (s *PostgresStore) requests(ctx context.Context, sql string, args ...any) ([]Request, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read maintenance requests: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Request, error) { return scanRequest(row) })
}

// Request implements Store.
func (s *PostgresStore) Request(ctx context.Context, id uuid.UUID) (Request, error) {
	r, err := scanRequest(s.pool.QueryRow(ctx, `SELECT `+requestColumns+` FROM maintenance_requests r WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	if err != nil {
		return Request{}, fmt.Errorf("read maintenance request: %w", err)
	}
	return r, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Requests implements Store.
func (s *PostgresStore) Requests(ctx context.Context, book tenancies.Book, f Filter) ([]Request, int, map[string]int, error) {
	args := []any{}
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	conds := []string{}
	if book.OrganisationID != nil {
		conds = append(conds, "r.organisation_id = "+add(*book.OrganisationID))
	} else {
		conds = append(conds, "r.book_user_id = "+add(book.ID()))
	}
	if f.Category != "" {
		conds = append(conds, "r.category = "+add(f.Category))
	}
	if f.Priority != "" {
		conds = append(conds, "r.priority = "+add(f.Priority))
	}
	if f.PropertyID != nil {
		conds = append(conds, "r.property_id = "+add(*f.PropertyID))
	}
	if f.TenancyID != nil {
		conds = append(conds, "r.tenancy_id = "+add(*f.TenancyID))
	}
	if f.Q != "" {
		p := add("%" + likeEscape(f.Q) + "%")
		conds = append(conds, "(r.reference ILIKE "+p+" OR r.title ILIKE "+p+")")
	}
	where := strings.Join(conds, " AND ")
	counts := map[string]int{}
	rows, err := s.pool.Query(ctx, `SELECT r.status, count(*) FROM maintenance_requests r WHERE `+where+` GROUP BY r.status`, args...)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("count maintenance requests: %w", err)
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
		where += " AND r.status = " + add(f.Status)
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM maintenance_requests r WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, nil, fmt.Errorf("count maintenance requests: %w", err)
	}
	sql := `SELECT ` + requestColumns + ` FROM maintenance_requests r WHERE ` + where + ` ORDER BY r.created_at DESC, r.id DESC`
	if f.Limit > 0 {
		sql += " LIMIT " + add(f.Limit) + " OFFSET " + add(f.Offset)
	}
	list, err := s.requests(ctx, sql, args...)
	return list, total, counts, err
}

// RequestsOfTenancy implements Store.
func (s *PostgresStore) RequestsOfTenancy(ctx context.Context, tenancyID uuid.UUID) ([]Request, error) {
	return s.requests(ctx, `SELECT `+requestColumns+` FROM maintenance_requests r WHERE r.tenancy_id = $1 ORDER BY r.created_at DESC, r.id DESC`,
		tenancyID)
}

// Events implements Store.
func (s *PostgresStore) Events(ctx context.Context, requestID uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, request_id, type, actor_kind, actor_user_id, actor_name, COALESCE(from_status, ''),
		COALESCE(to_status, ''), COALESCE(body, ''), internal, details, created_at
		FROM maintenance_events WHERE request_id = $1 ORDER BY created_at, id`, requestID)
	if err != nil {
		return nil, fmt.Errorf("read maintenance timeline: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		var details []byte
		if err := row.Scan(&e.ID, &e.RequestID, &e.Type, &e.ActorKind, &e.ActorUserID, &e.ActorName, &e.FromStatus, &e.ToStatus, &e.Text,
			&e.Internal, &details, &e.At); err != nil {
			return Event{}, err
		}
		if len(details) > 0 {
			if err := json.Unmarshal(details, &e.Details); err != nil {
				return Event{}, err
			}
		}
		e.At = e.At.UTC()
		return e, nil
	})
}

// Photos implements Store.
func (s *PostgresStore) Photos(ctx context.Context, requestID uuid.UUID) ([]Photo, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, request_id, object_key, content_type, width, height, size_bytes, sha256, uploader_kind,
		uploaded_by, uploaded_by_name, uploaded_at FROM maintenance_photos WHERE request_id = $1 ORDER BY uploaded_at, id`, requestID)
	if err != nil {
		return nil, fmt.Errorf("read maintenance photos: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Photo, error) {
		var p Photo
		err := row.Scan(&p.ID, &p.RequestID, &p.ObjectKey, &p.ContentType, &p.Width, &p.Height, &p.Size, &p.SHA256, &p.UploaderKind,
			&p.UploadedBy, &p.UploadedByName, &p.UploadedAt)
		p.UploadedAt = p.UploadedAt.UTC()
		return p, err
	})
}

// Costs implements Store.
func (s *PostgresStore) Costs(ctx context.Context, ids []uuid.UUID) ([]Cost, error) {
	if len(ids) == 0 {
		return []Cost{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, request_id, amount::text, currency, description, method, incurred_on::text, ledger_entry_id,
		recorded_by, recorded_by_name, recorded_at FROM maintenance_costs WHERE request_id = ANY($1) ORDER BY recorded_at DESC, id DESC`, ids)
	if err != nil {
		return nil, fmt.Errorf("read maintenance costs: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Cost, error) {
		var c Cost
		if err := row.Scan(&c.ID, &c.RequestID, &c.Amount, &c.Currency, &c.Description, &c.Method, &c.IncurredOn, &c.LedgerEntryID,
			&c.RecordedBy, &c.RecordedByName, &c.RecordedAt); err != nil {
			return Cost{}, err
		}
		c.Currency = strings.TrimSpace(c.Currency)
		c.Amount = money.Normalise(c.Amount, c.Currency)
		c.RecordedAt = c.RecordedAt.UTC()
		return c, nil
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

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func providerColumns(r Request) (id *uuid.UUID, kind, slug, name *string) {
	if r.Provider == nil {
		return nil, nil, nil, nil
	}
	return r.Provider.ProfileID, nullable(r.Provider.Kind), nullable(r.Provider.Slug), &r.Provider.Name
}

// ApplyTx writes a Change inside the caller's transaction (see Change for the order).
func ApplyTx(ctx context.Context, tx pgx.Tx, ch Change) error {
	for id, want := range ch.Expect {
		var status string
		var n int
		err := tx.QueryRow(ctx, `SELECT status, assignments FROM maintenance_requests WHERE id = $1 FOR UPDATE`, id).Scan(&status, &n)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (status != want.Status || n != want.Assignments)) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("lock maintenance request: %w", err)
		}
	}
	if ch.Ledger != nil {
		if err := ledger.ApplyTx(ctx, tx, *ch.Ledger); err != nil {
			return err
		}
	}
	if r := ch.Create; r != nil {
		pid, pkind, pslug, pname := providerColumns(*r)
		if _, err := tx.Exec(ctx, `
			INSERT INTO maintenance_requests (id, reference, organisation_id, book_user_id, property_id, unit_id, tenancy_id, category,
				category_label, priority, priority_label, title, description, status, raised_by_kind, raised_by, raised_by_name,
				provider_profile_id, provider_kind, provider_slug, provider_name, enquiry_id, enquiry_reference, assignments, assigned_at,
				assigned_by, assigned_by_name, status_changed_at, resolved_at, closed_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25,
				$26, $27, $28, $29, $30, $31, $32)`,
			r.ID, r.Reference, r.Book.OrganisationID, r.Book.UserID, r.PropertyID, r.UnitID, r.TenancyID, r.Category, r.CategoryLabel,
			r.Priority, r.PriorityLabel, r.Title, r.Description, r.Status, r.RaisedByKind, r.RaisedBy, r.RaisedByName, pid, pkind, pslug,
			pname, r.EnquiryID, nullable(r.EnquiryReference), r.Assignments, r.AssignedAt, r.AssignedBy, r.AssignedByName,
			r.StatusChangedAt, r.ResolvedAt, r.ClosedAt, r.CreatedAt, r.UpdatedAt); err != nil {
			return fmt.Errorf("insert maintenance request: %w", err)
		}
	}
	for _, r := range ch.Requests {
		pid, pkind, pslug, pname := providerColumns(r)
		tag, err := tx.Exec(ctx, `
			UPDATE maintenance_requests SET property_id = $2, unit_id = $3, tenancy_id = $4, category = $5, category_label = $6,
				priority = $7, priority_label = $8, title = $9, description = $10, status = $11, raised_by_kind = $12, raised_by = $13,
				raised_by_name = $14, provider_profile_id = $15, provider_kind = $16, provider_slug = $17, provider_name = $18,
				enquiry_id = $19, enquiry_reference = $20, assignments = $21, assigned_at = $22, assigned_by = $23, assigned_by_name = $24,
				status_changed_at = $25, resolved_at = $26, closed_at = $27, updated_at = $28
			WHERE id = $1`,
			r.ID, r.PropertyID, r.UnitID, r.TenancyID, r.Category, r.CategoryLabel, r.Priority, r.PriorityLabel, r.Title, r.Description,
			r.Status, r.RaisedByKind, r.RaisedBy, r.RaisedByName, pid, pkind, pslug, pname, r.EnquiryID, nullable(r.EnquiryReference),
			r.Assignments, r.AssignedAt, r.AssignedBy, r.AssignedByName, r.StatusChangedAt, r.ResolvedAt, r.ClosedAt, r.UpdatedAt)
		if err != nil {
			return fmt.Errorf("update maintenance request: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
	}
	for id, at := range ch.Touch {
		tag, err := tx.Exec(ctx, `UPDATE maintenance_requests SET updated_at = $2 WHERE id = $1`, id, at)
		if err != nil {
			return fmt.Errorf("touch maintenance request: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
	}
	if len(ch.DeletePhotos) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM maintenance_photos WHERE id = ANY($1)`, ch.DeletePhotos); err != nil {
			return fmt.Errorf("delete maintenance photos: %w", err)
		}
	}
	locked := map[uuid.UUID]bool{}
	for _, p := range ch.Photos {
		if !locked[p.RequestID] {
			// One upload at a time per request, so the limit holds.
			if _, err := tx.Exec(ctx, `SELECT 1 FROM maintenance_requests WHERE id = $1 FOR UPDATE`, p.RequestID); err != nil {
				return fmt.Errorf("lock maintenance request: %w", err)
			}
			locked[p.RequestID] = true
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO maintenance_photos (id, request_id, object_key, content_type, width, height, size_bytes, sha256, uploader_kind,
				uploaded_by, uploaded_by_name, uploaded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			p.ID, p.RequestID, p.ObjectKey, p.ContentType, p.Width, p.Height, p.Size, p.SHA256, p.UploaderKind, p.UploadedBy,
			p.UploadedByName, p.UploadedAt); err != nil {
			return fmt.Errorf("insert maintenance photo: %w", err)
		}
	}
	for id := range locked {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM maintenance_photos WHERE request_id = $1`, id).Scan(&n); err != nil {
			return fmt.Errorf("count maintenance photos: %w", err)
		}
		if n > MaxPhotos {
			return ErrPhotoLimit
		}
	}
	for _, c := range ch.Costs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO maintenance_costs (id, request_id, amount, currency, description, method, incurred_on, ledger_entry_id, recorded_by,
				recorded_by_name, recorded_at)
			VALUES ($1, $2, $3::numeric, $4, $5, $6, $7::date, $8, $9, $10, $11)`,
			c.ID, c.RequestID, c.Amount, c.Currency, c.Description, c.Method, c.IncurredOn, c.LedgerEntryID, c.RecordedBy, c.RecordedByName,
			c.RecordedAt); err != nil {
			return fmt.Errorf("insert maintenance cost: %w", err)
		}
	}
	for _, e := range ch.Events {
		var details []byte
		if e.Details != nil {
			var err error
			if details, err = json.Marshal(e.Details); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO maintenance_events (id, request_id, type, actor_kind, actor_user_id, actor_name, from_status, to_status, body,
				internal, details, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12)`,
			e.ID, e.RequestID, e.Type, e.ActorKind, e.ActorUserID, e.ActorName, nullable(e.FromStatus), nullable(e.ToStatus),
			nullable(e.Text), e.Internal, nullableJSON(details), e.At); err != nil {
			return fmt.Errorf("insert maintenance event: %w", err)
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
			uuid.New(), n.Type, AggregateMaintenance, n.Payload.RequestID, string(payload), n.Payload.OccurredAt); err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}
	}
	return nil
}

func nullableJSON(b []byte) *string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	return &s
}

// ForgetUserTx handles a deleted account's rows inside the account
// deletion's transaction (the users row is anonymised, so nothing
// cascades): the person is unlinked as raiser, assigner, actor, uploader
// and cost recorder; where they acted as a tenant their name is replaced
// with deletedName (managers' names stay, as the ledger's author names do).
func ForgetUserTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, deletedName string) error {
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE maintenance_requests SET raised_by_name = $2 WHERE raised_by = $1 AND raised_by_kind = 'TENANT'`, []any{userID, deletedName}},
		{`UPDATE maintenance_requests SET raised_by = NULL WHERE raised_by = $1`, []any{userID}},
		{`UPDATE maintenance_requests SET assigned_by = NULL WHERE assigned_by = $1`, []any{userID}},
		{`UPDATE maintenance_events SET actor_name = $2 WHERE actor_user_id = $1 AND actor_kind = 'TENANT'`, []any{userID, deletedName}},
		{`UPDATE maintenance_events SET actor_user_id = NULL WHERE actor_user_id = $1`, []any{userID}},
		{`UPDATE maintenance_photos SET uploaded_by_name = $2 WHERE uploaded_by = $1 AND uploader_kind = 'TENANT'`, []any{userID, deletedName}},
		{`UPDATE maintenance_photos SET uploaded_by = NULL WHERE uploaded_by = $1`, []any{userID}},
		{`UPDATE maintenance_costs SET recorded_by = NULL WHERE recorded_by = $1`, []any{userID}},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
			return fmt.Errorf("forget the deleted account's maintenance rows: %w", err)
		}
	}
	return nil
}
