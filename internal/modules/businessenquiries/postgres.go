package businessenquiries

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Constraint names from migrations/0005_business_enquiries.up.sql.
const (
	uniqueViolation     = "23505"
	referenceConstraint = "business_enquiries_reference_key"
)

// PostgresStore is the production Store. Statements use no session state and
// one explicit short transaction, so they work on a plain PostgreSQL
// connection and equally behind a transaction pooler (DB_POOL_MODE=transaction).
//
// NOTE: written against migration 0005 and compiled, but not yet exercised
// against a real database.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// GetProfile implements Store. Eligibility is computed here so that routing
// reflects the state of the organisation and its people at this moment.
func (s *PostgresStore) GetProfile(ctx context.Context, kind, slug string) (Profile, error) {
	var p Profile
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, p.kind, p.slug, p.name, p.market_id, p.status, p.organisation_id,
		       o.status = 'ACTIVE' AND EXISTS (
		           SELECT 1 FROM memberships m
		           JOIN users u ON u.id = m.user_id
		           WHERE m.organisation_id = p.organisation_id
		             AND m.status = 'ACTIVE' AND m.role IN ('OWNER', 'MANAGER')
		             AND u.status = 'ACTIVE'),
		       p.agent_user_id,
		       COALESCE(o.status = 'ACTIVE' AND au.status = 'ACTIVE' AND EXISTS (
		           SELECT 1 FROM memberships m
		           WHERE m.organisation_id = p.organisation_id
		             AND m.user_id = p.agent_user_id
		             AND m.status = 'ACTIVE'), false)
		FROM directory_profiles p
		JOIN organisations o ON o.id = p.organisation_id
		LEFT JOIN users au ON au.id = p.agent_user_id
		WHERE p.kind = $1 AND p.slug = $2`, kind, slug).Scan(
		&p.ID, &p.Kind, &p.Slug, &p.Name, &p.MarketID, &p.Status, &p.OrganisationID,
		&p.OrganisationEligible, &p.AgentUserID, &p.AgentEligible,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrRecipientNotFound
	}
	if err != nil {
		return Profile{}, err
	}
	return p, nil
}

// FindByIdempotencyKey implements Store.
func (s *PostgresStore) FindByIdempotencyKey(ctx context.Context, key string) (Record, error) {
	var rec Record
	err := s.pool.QueryRow(ctx, `
		SELECT id, reference, status, request_hash, created_at
		FROM business_enquiries
		WHERE idempotency_key = $1`, key).Scan(&rec.ID, &rec.Reference, &rec.Status, &rec.RequestHash, &rec.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	return rec, nil
}

// Create implements Store: the enquiry, its timeline, the customer activity
// and the outbox event are written in ONE transaction and the method returns
// only after COMMIT succeeds.
//
// Concurrent requests with the same idempotency key are made safe by the
// unique constraint: the loser's INSERT ... ON CONFLICT DO NOTHING waits for
// the winner to commit, inserts nothing, and the committed row is returned.
func (s *PostgresStore) Create(ctx context.Context, enq NewEnquiry) (Record, bool, error) {
	payload, err := outboxPayload(enq)
	if err != nil {
		return Record{}, false, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Record{}, false, fmt.Errorf("begin: %w", err)
	}
	// No-op after a successful commit. WithoutCancel so a cancelled request
	// still releases the connection cleanly.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var (
		recipientOrgID  *uuid.UUID
		recipientUserID *uuid.UUID
	)
	if enq.Recipient != nil {
		recipientOrgID = &enq.Recipient.OrganisationID
		recipientUserID = enq.Recipient.UserID
	}
	var service, guestPhone *string
	if enq.Service != "" {
		service = &enq.Service
	}
	if enq.GuestPhone != "" {
		guestPhone = &enq.GuestPhone
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO business_enquiries (
			id, reference, market_id, profile_id, status, service, message,
			preferred_contact_method, idempotency_key, request_hash,
			customer_user_id, guest_name, guest_email, guest_phone,
			consent_privacy_version, consent_accepted_at,
			recipient_organisation_id, recipient_user_id,
			created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $19)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		enq.ID, enq.Reference, enq.MarketID, enq.ProfileID, enq.Status, service, enq.Message,
		enq.PreferredContactMethod, enq.IdempotencyKey, enq.RequestHash,
		enq.CustomerUserID, enq.GuestName, enq.GuestEmail, guestPhone,
		enq.PrivacyVersion, enq.ConsentAcceptedAt,
		recipientOrgID, recipientUserID,
		enq.CreatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == referenceConstraint {
			return Record{}, false, ErrReferenceCollision
		}
		return Record{}, false, fmt.Errorf("insert business enquiry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Idempotency key already committed by another request.
		_ = tx.Rollback(context.WithoutCancel(ctx))
		rec, err := s.FindByIdempotencyKey(ctx, enq.IdempotencyKey)
		if err != nil {
			return Record{}, false, fmt.Errorf("load existing enquiry: %w", err)
		}
		return rec, false, nil
	}

	for _, ev := range timelineFor(enq) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO business_enquiry_timeline (id, enquiry_id, event_type, actor_type, created_at)
			VALUES ($1, $2, $3, $4, $5)`,
			uuid.New(), enq.ID, ev.EventType, ev.ActorType, enq.CreatedAt,
		); err != nil {
			return Record{}, false, fmt.Errorf("insert enquiry timeline: %w", err)
		}
	}

	// Activity belongs to the account for an authenticated customer and to the
	// guest's email otherwise.
	var activityGuestEmail *string
	if enq.CustomerUserID == nil {
		activityGuestEmail = &enq.GuestEmail
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO customer_activity (id, user_id, guest_email, activity_type, market_id, business_enquiry_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New(), enq.CustomerUserID, activityGuestEmail, ActivityEnquirySubmitted, enq.MarketID, enq.ID, enq.CreatedAt,
	); err != nil {
		return Record{}, false, fmt.Errorf("insert customer activity: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
		uuid.New(), EventBusinessEnquiryCreated, AggregateBusinessEnquiry, enq.ID, string(payload), enq.CreatedAt,
	); err != nil {
		return Record{}, false, fmt.Errorf("insert outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Record{}, false, fmt.Errorf("commit: %w", err)
	}
	return Record{ID: enq.ID, Reference: enq.Reference, Status: enq.Status, RequestHash: enq.RequestHash, CreatedAt: enq.CreatedAt}, true, nil
}

// inboxColumns / inboxFrom are the inbox's view of an enquiry.
const inboxColumns = `e.id, e.reference, p.kind, p.slug, p.name, e.service, e.message,
		       e.preferred_contact_method,
		       COALESCE(e.guest_name, u.full_name, ''),
		       COALESCE(e.guest_email, u.email, ''),
		       e.guest_phone, e.status, e.created_at, e.replied_at`

const inboxFrom = ` FROM business_enquiries e
		JOIN directory_profiles p ON p.id = e.profile_id
		LEFT JOIN users u ON u.id = e.customer_user_id `

// inboxVisible is the rule of InboxScope ($1 managed organisations, $2
// member organisations, $3 the caller).
const inboxVisible = `(e.recipient_organisation_id IN (SELECT unnest($1::text[])::uuid)
		   OR (e.recipient_organisation_id IN (SELECT unnest($2::text[])::uuid)
		       AND e.recipient_user_id = $3))`

func scanInbox(row pgx.CollectableRow) (InboxItem, error) {
	var it InboxItem
	err := row.Scan(&it.EnquiryID, &it.Reference, &it.Recipient.Kind, &it.Recipient.Slug, &it.Recipient.Name,
		&it.Service, &it.Message, &it.PreferredContactMethod,
		&it.Contact.Name, &it.Contact.Email, &it.Contact.Phone, &it.Status, &it.CreatedAt, &it.RepliedAt)
	it.CreatedAt = it.CreatedAt.UTC()
	if it.RepliedAt != nil {
		at := it.RepliedAt.UTC()
		it.RepliedAt = &at
	}
	return it, err
}

// ListInbox implements Store. Organisation IDs are passed as text arrays and
// cast in SQL; the visibility rule is documented on InboxScope. Routing
// exceptions have no recipient organisation, so no branch can match them.
func (s *PostgresStore) ListInbox(ctx context.Context, scope InboxScope) ([]InboxItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+inboxColumns+inboxFrom+`WHERE `+inboxVisible+`
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT $4`,
		uuidStrings(scope.ManagedOrganisationIDs), uuidStrings(scope.MemberOrganisationIDs), scope.UserID, scope.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanInbox)
}

// MarkReplied implements Store: the first mark sets replied_at and adds a
// REPLIED timeline row, in one transaction; a repeat changes nothing.
func (s *PostgresStore) MarkReplied(ctx context.Context, scope InboxScope, id, by uuid.UUID, at time.Time) (InboxItem, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InboxItem{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	args := []any{uuidStrings(scope.ManagedOrganisationIDs), uuidStrings(scope.MemberOrganisationIDs), scope.UserID, id}
	tag, err := tx.Exec(ctx, `UPDATE business_enquiries e SET replied_at = $5, replied_by = $6, updated_at = $5
		WHERE e.id = $4 AND e.replied_at IS NULL AND `+inboxVisible, append(args, at.UTC(), by)...)
	if err != nil {
		return InboxItem{}, fmt.Errorf("mark enquiry replied: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO business_enquiry_timeline (id, enquiry_id, event_type, actor_type, actor_user_id, created_at)
			VALUES ($1, $2, $3, 'AGENT', $4, $5)`, uuid.New(), id, TimelineReplied, by, at.UTC()); err != nil {
			return InboxItem{}, fmt.Errorf("insert enquiry timeline: %w", err)
		}
	}
	rows, err := tx.Query(ctx, `SELECT `+inboxColumns+inboxFrom+`WHERE e.id = $4 AND `+inboxVisible, args...)
	if err != nil {
		return InboxItem{}, err
	}
	items, err := pgx.CollectRows(rows, scanInbox)
	if err != nil {
		return InboxItem{}, err
	}
	if len(items) == 0 {
		return InboxItem{}, ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return InboxItem{}, err
	}
	return items[0], nil
}

const customerEnquiryColumns = `id, reference, profile_id, customer_user_id, consent_privacy_version, created_at, replied_at`

func scanCustomerEnquiry(row pgx.CollectableRow) (CustomerEnquiry, error) {
	var e CustomerEnquiry
	err := row.Scan(&e.ID, &e.Reference, &e.ProfileID, &e.CustomerUserID, &e.PrivacyVersion, &e.CreatedAt, &e.RepliedAt)
	e.CreatedAt = e.CreatedAt.UTC()
	if e.RepliedAt != nil {
		at := e.RepliedAt.UTC()
		e.RepliedAt = &at
	}
	return e, err
}

// Enquiry implements Store.
func (s *PostgresStore) Enquiry(ctx context.Context, id uuid.UUID) (CustomerEnquiry, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+customerEnquiryColumns+` FROM business_enquiries WHERE id = $1`, id)
	if err != nil {
		return CustomerEnquiry{}, err
	}
	list, err := pgx.CollectRows(rows, scanCustomerEnquiry)
	if err != nil {
		return CustomerEnquiry{}, err
	}
	if len(list) == 0 {
		return CustomerEnquiry{}, ErrNotFound
	}
	return list[0], nil
}

// CustomerEnquiries implements Store.
func (s *PostgresStore) CustomerEnquiries(ctx context.Context, userID, profileID uuid.UUID) ([]CustomerEnquiry, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+customerEnquiryColumns+` FROM business_enquiries
		WHERE customer_user_id = $1 AND profile_id = $2 ORDER BY created_at DESC, id DESC LIMIT 200`, userID, profileID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanCustomerEnquiry)
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
