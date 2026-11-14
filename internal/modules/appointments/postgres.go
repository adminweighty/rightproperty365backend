package appointments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

// Constraint names from migrations/0011_appointments.up.sql.
const (
	uniqueViolation     = "23505"
	referenceConstraint = "appointments_reference_key"
)

// PostgresStore is the production Store. Every write is one explicit short
// transaction without session state, so it works behind a transaction pooler.
//
// NOTE: compiled but never exercised against a real database (there is no
// PostgreSQL on the development machine).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// querier is what both a pool and a transaction offer.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const columns = `
	a.id, a.reference, a.type, a.status, a.market_id, a.timezone,
	a.listing_id, COALESCE(a.listing_slug, ''), COALESCE(a.listing_title, ''), COALESCE(a.listing_reference, ''),
	COALESCE(a.profile_kind, ''), COALESCE(a.profile_slug, ''),
	COALESCE(a.with_kind, ''), COALESCE(a.with_slug, ''), COALESCE(a.with_name, ''),
	a.property_request_id, a.business_enquiry_id, COALESCE(a.linked_reference, ''),
	a.customer_user_id, a.customer_name, a.customer_email, COALESCE(a.customer_phone, ''),
	a.preferred_contact_method, COALESCE(a.customer_note, ''),
	a.recipient_type, a.recipient_organisation_id, a.recipient_user_id,
	a.start_at, a.duration_minutes, COALESCE(a.location_note, ''),
	a.proposed_start_at, COALESCE(a.proposed_by, ''), COALESCE(a.cancelled_by, ''), COALESCE(a.cancel_reason, ''),
	COALESCE(a.idempotency_key, ''), COALESCE(a.request_hash, ''), COALESCE(a.consent_privacy_version, ''), a.consent_accepted_at,
	a.created_at, a.updated_at`

func scan(row pgx.Row) (Appointment, error) {
	var a Appointment
	err := row.Scan(
		&a.ID, &a.Reference, &a.Type, &a.Status, &a.MarketID, &a.Timezone,
		&a.ListingID, &a.ListingSlug, &a.ListingTitle, &a.ListingReference,
		&a.ProfileKind, &a.ProfileSlug,
		&a.WithKind, &a.WithSlug, &a.WithName,
		&a.PropertyRequestID, &a.BusinessEnquiryID, &a.LinkedReference,
		&a.CustomerUserID, &a.CustomerName, &a.CustomerEmail, &a.CustomerPhone,
		&a.PreferredContactMethod, &a.CustomerNote,
		&a.RecipientType, &a.OrganisationID, &a.AssignedUserID,
		&a.StartUTC, &a.DurationMinutes, &a.LocationNote,
		&a.ProposedStartUTC, &a.ProposedBy, &a.CancelledBy, &a.CancelReason,
		&a.IdempotencyKey, &a.RequestHash, &a.PrivacyVersion, &a.ConsentAcceptedAt,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return Appointment{}, err
	}
	a.StartUTC, a.CreatedAt, a.UpdatedAt = a.StartUTC.UTC(), a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	if a.ProposedStartUTC != nil {
		utc := a.ProposedStartUTC.UTC()
		a.ProposedStartUTC = &utc
	}
	return a, nil
}

// null turns "" into NULL.
func null(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// insert writes the appointment row. With an idempotency key it does nothing
// when the key exists; inserted reports whether a row was written.
func insert(ctx context.Context, q querier, a Appointment) (inserted bool, err error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO appointments (
			id, reference, market_id, timezone, type, status,
			listing_id, listing_slug, listing_title, listing_reference,
			profile_kind, profile_slug, with_kind, with_slug, with_name,
			property_request_id, business_enquiry_id, linked_reference,
			customer_user_id, customer_name, customer_email, customer_phone, preferred_contact_method, customer_note,
			recipient_type, recipient_organisation_id, recipient_user_id,
			start_at, duration_minutes, end_at, location_note,
			proposed_start_at, proposed_by, cancelled_by, cancel_reason,
			idempotency_key, request_hash, consent_privacy_version, consent_accepted_at,
			created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
		        $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		a.ID, a.Reference, a.MarketID, a.Timezone, a.Type, a.Status,
		a.ListingID, null(a.ListingSlug), null(a.ListingTitle), null(a.ListingReference),
		null(a.ProfileKind), null(a.ProfileSlug), null(a.WithKind), null(a.WithSlug), null(a.WithName),
		a.PropertyRequestID, a.BusinessEnquiryID, null(a.LinkedReference),
		a.CustomerUserID, a.CustomerName, a.CustomerEmail, null(a.CustomerPhone), a.PreferredContactMethod, null(a.CustomerNote),
		a.RecipientType, a.OrganisationID, a.AssignedUserID,
		a.StartUTC, a.DurationMinutes, a.EndUTC(), null(a.LocationNote),
		a.ProposedStartUTC, null(a.ProposedBy), null(a.CancelledBy), null(a.CancelReason),
		null(a.IdempotencyKey), null(a.RequestHash), null(a.PrivacyVersion), a.ConsentAcceptedAt,
		a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func insertEntry(ctx context.Context, q querier, e TimelineEntry) error {
	_, err := q.Exec(ctx, `
		INSERT INTO appointment_timeline (id, appointment_id, event, actor_type, actor_user_id, from_status, to_status, note, start_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.ID, e.AppointmentID, e.Event, e.ActorType, e.ActorUserID, null(e.FromStatus), e.ToStatus, null(e.Note), e.StartUTC, e.At)
	return err
}

func insertEvent(ctx context.Context, q querier, eventType string, a Appointment, e TimelineEntry) error {
	payload, err := eventPayload(a, e)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
		uuid.New(), eventType, AggregateAppointment, a.ID, string(payload), e.At)
	return err
}

// InsertLinkedTx is the hook the Postgres property request store calls inside
// its own transaction: the request and its appointment commit together. A
// taken reference surfaces as the unique violation on
// appointments_reference_key, which that store maps to a retry.
func InsertLinkedTx(ctx context.Context, tx pgx.Tx, req propertyrequests.NewRequest) error {
	if req.Appointment == nil {
		return nil
	}
	linked, ok := req.Appointment.Data.(Linked)
	if !ok {
		return errors.New("appointments: unexpected linked appointment data")
	}
	if _, err := insert(ctx, tx, linked.Appointment); err != nil {
		return err
	}
	if err := insertEntry(ctx, tx, linked.Entry); err != nil {
		return err
	}
	return insertEvent(ctx, tx, EventRequested, linked.Appointment, linked.Entry)
}

// SeedTx inserts (or replaces) a sample appointment with its timeline and no
// events. Development seed only.
func SeedTx(ctx context.Context, tx pgx.Tx, a Appointment, entries ...TimelineEntry) error {
	if _, err := tx.Exec(ctx, `DELETE FROM appointments WHERE id = $1`, a.ID); err != nil {
		return err
	}
	if _, err := insert(ctx, tx, a); err != nil {
		return err
	}
	for _, e := range entries {
		if err := insertEntry(ctx, tx, e); err != nil {
			return err
		}
	}
	return nil
}

// FindByIdempotencyKey implements Store.
func (s *PostgresStore) FindByIdempotencyKey(ctx context.Context, key string) (Appointment, error) {
	a, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM appointments a WHERE a.idempotency_key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Appointment{}, ErrNotFound
	}
	return a, err
}

// Create implements Store: the appointment, its first timeline entry and the
// outbox event are written in ONE transaction.
func (s *PostgresStore) Create(ctx context.Context, a Appointment, entry TimelineEntry) (Appointment, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Appointment{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	inserted, err := insert(ctx, tx, a)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == referenceConstraint {
			return Appointment{}, false, ErrReferenceCollision
		}
		return Appointment{}, false, fmt.Errorf("insert appointment: %w", err)
	}
	if !inserted {
		// Idempotency key already committed by another request.
		_ = tx.Rollback(context.WithoutCancel(ctx))
		existing, err := s.FindByIdempotencyKey(ctx, a.IdempotencyKey)
		if err != nil {
			return Appointment{}, false, fmt.Errorf("load existing appointment: %w", err)
		}
		return existing, false, nil
	}
	if err := insertEntry(ctx, tx, entry); err != nil {
		return Appointment{}, false, fmt.Errorf("insert appointment timeline: %w", err)
	}
	if err := insertEvent(ctx, tx, EventRequested, a, entry); err != nil {
		return Appointment{}, false, fmt.Errorf("insert outbox event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Appointment{}, false, fmt.Errorf("commit: %w", err)
	}
	return a, true, nil
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Appointment, error) {
	a, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM appointments a WHERE a.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Appointment{}, ErrNotFound
	}
	return a, err
}

// Timeline implements Store.
func (s *PostgresStore) Timeline(ctx context.Context, id uuid.UUID) ([]TimelineEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, appointment_id, event, actor_type, actor_user_id, COALESCE(from_status, ''), to_status, COALESCE(note, ''), start_at, created_at
		FROM appointment_timeline WHERE appointment_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TimelineEntry, error) {
		var e TimelineEntry
		err := row.Scan(&e.ID, &e.AppointmentID, &e.Event, &e.ActorType, &e.ActorUserID, &e.FromStatus, &e.ToStatus, &e.Note, &e.StartUTC, &e.At)
		e.At = e.At.UTC()
		return e, err
	})
}

// Update implements Store. The row is read FOR UPDATE, so two changes of the
// same appointment run one after the other.
//
// The overlap rule is a LOCKED CHECK inside this transaction (not an
// exclusion constraint): before a state that is CONFIRMED is written, the
// transaction takes pg_advisory_xact_lock on the professional's key, so two
// confirmations for the same professional are serialised, and then looks for
// another CONFIRMED appointment of that professional whose time overlaps. The
// lock is released at commit or rollback and works behind a transaction pooler.
func (s *PostgresStore) Update(ctx context.Context, id uuid.UUID, decide func(Appointment) (Mutation, error)) (Appointment, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Appointment{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	current, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM appointments a WHERE a.id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Appointment{}, ErrNotFound
	}
	if err != nil {
		return Appointment{}, fmt.Errorf("load appointment: %w", err)
	}
	m, err := decide(current)
	if err != nil {
		return Appointment{}, err
	}
	next := m.Next

	if next.Status == StatusConfirmed {
		if key, ok := next.BusyKey(); ok {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, key.String()); err != nil {
				return Appointment{}, fmt.Errorf("lock professional: %w", err)
			}
			var taken bool
			err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM appointments o
					WHERE o.status = 'CONFIRMED' AND o.id <> $1
					  AND COALESCE(o.recipient_user_id, o.recipient_organisation_id) = $2
					  AND o.start_at < $4 AND o.end_at > $3)`,
				id, key, next.StartUTC, next.EndUTC()).Scan(&taken)
			if err != nil {
				return Appointment{}, fmt.Errorf("check overlap: %w", err)
			}
			if taken {
				return Appointment{}, ErrSlotTaken
			}
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE appointments SET
			status = $2, recipient_type = $3, recipient_user_id = $4,
			start_at = $5, duration_minutes = $6, end_at = $7, location_note = $8,
			proposed_start_at = $9, proposed_by = $10, cancelled_by = $11, cancel_reason = $12, updated_at = $13
		WHERE id = $1`,
		id, next.Status, next.RecipientType, next.AssignedUserID,
		next.StartUTC, next.DurationMinutes, next.EndUTC(), null(next.LocationNote),
		next.ProposedStartUTC, null(next.ProposedBy), null(next.CancelledBy), null(next.CancelReason), next.UpdatedAt,
	); err != nil {
		return Appointment{}, fmt.Errorf("update appointment: %w", err)
	}
	if err := insertEntry(ctx, tx, m.Entry); err != nil {
		return Appointment{}, fmt.Errorf("insert appointment timeline: %w", err)
	}
	if m.Event != "" {
		if err := insertEvent(ctx, tx, m.Event, next, m.Entry); err != nil {
			return Appointment{}, fmt.Errorf("insert outbox event: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Appointment{}, fmt.Errorf("commit: %w", err)
	}
	return next, nil
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// conditions builds a WHERE clause with numbered arguments.
type conditions struct {
	parts []string
	args  []any
}

// arg registers a value and returns its placeholder.
func (c *conditions) arg(v any) string {
	c.args = append(c.args, v)
	return fmt.Sprintf("$%d", len(c.args))
}

func (c *conditions) add(sql string) { c.parts = append(c.parts, sql) }

// scope adds the visibility rule documented on Scope (and implemented for
// the memory store by Scope.Sees).
func (c *conditions) scope(s Scope) {
	managed, member, user, direct := c.arg(uuidStrings(s.ManagedOrganisationIDs)), c.arg(uuidStrings(s.MemberOrganisationIDs)), c.arg(s.UserID), c.arg(s.DirectOwner)
	c.add(`(a.recipient_organisation_id IN (SELECT unnest(` + managed + `::text[])::uuid)
		OR (a.recipient_organisation_id IN (SELECT unnest(` + member + `::text[])::uuid)
		    AND (a.recipient_user_id = ` + user + ` OR (a.recipient_user_id IS NULL AND a.type = 'VIEWING')))
		OR (` + direct + `::boolean AND a.recipient_type = 'SELLER' AND a.recipient_user_id = ` + user + `))`)
}

const upcomingSQL = `(a.status IN ('REQUESTED', 'CONFIRMED', 'RESCHEDULE_REQUESTED') AND a.end_at > %s)`

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, f ListFilter) ([]Appointment, int, error) {
	var c conditions
	switch {
	case f.CustomerUserID != nil:
		c.add(`a.customer_user_id = ` + c.arg(*f.CustomerUserID))
	case f.Scope != nil:
		c.scope(*f.Scope)
	default:
		return []Appointment{}, 0, nil
	}
	switch f.When {
	case WhenUpcoming:
		c.add(fmt.Sprintf(upcomingSQL, c.arg(f.Now)))
	case WhenPast:
		c.add(`NOT ` + fmt.Sprintf(upcomingSQL, c.arg(f.Now)))
	}
	if f.From != nil {
		c.add(`a.start_at >= ` + c.arg(*f.From))
	}
	if f.To != nil {
		c.add(`a.start_at < ` + c.arg(*f.To))
	}
	if len(f.Statuses) > 0 {
		c.add(`a.status = ANY(` + c.arg(f.Statuses) + `::text[])`)
	}
	if f.Type != "" {
		c.add(`a.type = ` + c.arg(f.Type))
	}
	if f.ListingID != nil {
		c.add(`a.listing_id = ` + c.arg(*f.ListingID))
	}
	where := strings.Join(c.parts, " AND ")

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM appointments a WHERE `+where, c.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count appointments: %w", err)
	}
	order := `a.start_at DESC, a.id`
	if f.Ascending {
		order = `a.start_at ASC, a.id`
	}
	limit, offset := c.arg(f.PageSize), c.arg((f.Page-1)*f.PageSize)
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM appointments a WHERE `+where+` ORDER BY `+order+` LIMIT `+limit+` OFFSET `+offset, c.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list appointments: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Appointment, error) { return scan(row) })
	if err != nil {
		return nil, 0, fmt.Errorf("list appointments: %w", err)
	}
	return list, total, nil
}

// Count implements Store.
func (s *PostgresStore) Count(ctx context.Context, scope Scope, now time.Time) (Counts, error) {
	var c conditions
	c.scope(scope)
	at := c.arg(now)
	var out Counts
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE a.status = 'REQUESTED'),
		       count(*) FILTER (WHERE a.status = 'RESCHEDULE_REQUESTED'),
		       count(*) FILTER (WHERE a.status = 'CONFIRMED' AND a.end_at > `+at+`)
		FROM appointments a WHERE `+c.parts[0], c.args...).Scan(&out.Requested, &out.RescheduleRequested, &out.UpcomingConfirmed)
	if err != nil {
		return Counts{}, fmt.Errorf("count appointments: %w", err)
	}
	return out, nil
}
