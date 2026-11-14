package propertyrequests

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

// Constraint names from migrations/0001_init.up.sql.
const (
	uniqueViolation     = "23505"
	referenceConstraint = "property_requests_reference_key"
)

// PostgresStore is the production Store. Statements use no session state and
// one explicit short transaction, so they work on a plain PostgreSQL
// connection and equally behind a transaction pooler (DB_POOL_MODE=transaction).
type PostgresStore struct {
	pool *pgxpool.Pool
	// appointments inserts the appointment that belongs to a request inside
	// the request's transaction (appointments.InsertLinkedTx).
	appointments func(ctx context.Context, tx pgx.Tx, req NewRequest) error
}

// SetAppointmentWriter registers the function that inserts a request's
// appointment in the same transaction.
func (s *PostgresStore) SetAppointmentWriter(write func(ctx context.Context, tx pgx.Tx, req NewRequest) error) {
	s.appointments = write
}

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// appointmentReferenceConstraint is from migrations/0011_appointments.up.sql.
const appointmentReferenceConstraint = "appointments_reference_key"

// GetListing implements Store.
func (s *PostgresStore) GetListing(ctx context.Context, id uuid.UUID) (Listing, error) {
	var l Listing
	err := s.pool.QueryRow(ctx, `
		SELECT l.id, l.market_id, l.status::text, l.expires_at,
		       l.agent_user_id,
		       COALESCE(au.status = 'ACTIVE' AND (
		           l.organisation_id IS NULL OR EXISTS (
		               SELECT 1 FROM memberships m
		               WHERE m.organisation_id = l.organisation_id
		                 AND m.user_id = l.agent_user_id
		                 AND m.status = 'ACTIVE')), false),
		       l.organisation_id,
		       COALESCE(o.status = 'ACTIVE' AND o.enquiry_email IS NOT NULL, false),
		       l.seller_user_id,
		       COALESCE(su.status = 'ACTIVE', false)
		FROM listings l
		LEFT JOIN users au ON au.id = l.agent_user_id
		LEFT JOIN organisations o ON o.id = l.organisation_id
		LEFT JOIN users su ON su.id = l.seller_user_id
		WHERE l.id = $1`, id).Scan(
		&l.ID, &l.MarketID, &l.Status, &l.ExpiresAt,
		&l.AgentUserID, &l.AgentEligible,
		&l.OrganisationID, &l.OrganisationEligible,
		&l.SellerUserID, &l.SellerEligible,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Listing{}, ErrListingNotFound
	}
	if err != nil {
		return Listing{}, err
	}
	if l.ExpiresAt != nil {
		utc := l.ExpiresAt.UTC()
		l.ExpiresAt = &utc
	}
	return l, nil
}

// FindByIdempotencyKey implements Store.
func (s *PostgresStore) FindByIdempotencyKey(ctx context.Context, key string) (Record, error) {
	var rec Record
	var appointmentID *uuid.UUID
	var appointmentRef *string
	err := s.pool.QueryRow(ctx, `
		SELECT pr.id, pr.reference, pr.status, pr.request_hash, pr.created_at, a.id, a.reference
		FROM property_requests pr
		LEFT JOIN appointments a ON a.property_request_id = pr.id
		WHERE pr.idempotency_key = $1`, key).Scan(&rec.ID, &rec.Reference, &rec.Status, &rec.RequestHash, &rec.CreatedAt, &appointmentID, &appointmentRef)
	if err == nil && appointmentID != nil && appointmentRef != nil {
		rec.Appointment = &AppointmentRef{ID: *appointmentID, Reference: *appointmentRef}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	return rec, nil
}

// Create implements Store: the request, its timeline, the customer activity
// and the outbox event are written in ONE transaction and the method returns
// only after COMMIT succeeds.
//
// Concurrent requests with the same idempotency key are made safe by the
// unique constraint: the loser's INSERT ... ON CONFLICT DO NOTHING waits for
// the winner to commit, inserts nothing, and the committed row is returned.
func (s *PostgresStore) Create(ctx context.Context, req NewRequest) (Record, bool, error) {
	payload, err := outboxPayload(req)
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
		recipientType   *string
		recipientUserID *uuid.UUID
		recipientOrgID  *uuid.UUID
	)
	if req.Recipient != nil {
		recipientType = &req.Recipient.Type
		recipientUserID = req.Recipient.UserID
		recipientOrgID = req.Recipient.OrganisationID
	}
	var guestPhone *string
	if req.GuestPhone != "" {
		guestPhone = &req.GuestPhone
	}
	// A request to stay stores its dates and guests (NULL for every other type).
	var stayCheckIn, stayCheckOut *string
	var stayGuests *int
	if req.Stay != nil {
		stayCheckIn, stayCheckOut, stayGuests = &req.Stay.CheckIn, &req.Stay.CheckOut, &req.Stay.Guests
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO property_requests (
			id, reference, market_id, listing_id, request_type, status, message,
			preferred_contact_method, preferred_slot_at, idempotency_key, request_hash,
			guest_name, guest_email, guest_phone,
			consent_privacy_version, consent_accepted_at,
			recipient_type, recipient_user_id, recipient_organisation_id,
			created_at, updated_at, customer_user_id, check_in, check_out, guests)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $20, $21,
		        $22::text::date, $23::text::date, $24)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		req.ID, req.Reference, req.MarketID, req.ListingID, req.RequestType, req.Status, req.Message,
		req.PreferredContactMethod, req.PreferredSlot, req.IdempotencyKey, req.RequestHash,
		req.GuestName, req.GuestEmail, guestPhone,
		req.PrivacyVersion, req.ConsentAcceptedAt,
		recipientType, recipientUserID, recipientOrgID,
		req.CreatedAt, req.CustomerUserID, stayCheckIn, stayCheckOut, stayGuests,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == referenceConstraint {
			return Record{}, false, ErrReferenceCollision
		}
		return Record{}, false, fmt.Errorf("insert property request: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Idempotency key already committed by another request.
		_ = tx.Rollback(context.WithoutCancel(ctx))
		rec, err := s.FindByIdempotencyKey(ctx, req.IdempotencyKey)
		if err != nil {
			return Record{}, false, fmt.Errorf("load existing request: %w", err)
		}
		return rec, false, nil
	}

	for _, ev := range timelineFor(req) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO request_timeline (id, request_id, event_type, actor_type, created_at)
			VALUES ($1, $2, $3, $4, $5)`,
			uuid.New(), req.ID, ev.EventType, ev.ActorType, req.CreatedAt,
		); err != nil {
			return Record{}, false, fmt.Errorf("insert request timeline: %w", err)
		}
	}

	// Activity belongs to the account for an authenticated customer and to the
	// guest's email otherwise.
	var activityGuestEmail *string
	if req.CustomerUserID == nil {
		activityGuestEmail = &req.GuestEmail
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO customer_activity (id, user_id, guest_email, activity_type, market_id, listing_id, request_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		uuid.New(), req.CustomerUserID, activityGuestEmail, ActivityRequestSubmitted, req.MarketID, req.ListingID, req.ID, req.CreatedAt,
	); err != nil {
		return Record{}, false, fmt.Errorf("insert customer activity: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
		uuid.New(), EventPropertyRequestCreated, AggregatePropertyRequest, req.ID, string(payload), req.CreatedAt,
	); err != nil {
		return Record{}, false, fmt.Errorf("insert outbox event: %w", err)
	}

	rec := Record{ID: req.ID, Reference: req.Reference, Status: req.Status, RequestHash: req.RequestHash, CreatedAt: req.CreatedAt}
	if req.Appointment != nil {
		if s.appointments == nil {
			return Record{}, false, errors.New("no appointment writer is configured")
		}
		// Same transaction: the request and its appointment commit together.
		// A taken appointment reference fails the whole transaction and is
		// reported as ErrReferenceCollision; the service retries with new ones.
		if err := s.appointments(ctx, tx, req); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == appointmentReferenceConstraint {
				return Record{}, false, ErrReferenceCollision
			}
			return Record{}, false, fmt.Errorf("insert appointment: %w", err)
		}
		rec.Appointment = &AppointmentRef{ID: req.Appointment.ID, Reference: req.Appointment.Reference}
	}

	if err := tx.Commit(ctx); err != nil {
		return Record{}, false, fmt.Errorf("commit: %w", err)
	}
	return rec, true, nil
}

// StayOf builds the stay of a stored request from its nullable columns
// (dates as YYYY-MM-DD); nil unless all three are set.
func StayOf(checkIn, checkOut *string, guests *int) *StayDetails {
	if checkIn == nil || checkOut == nil || guests == nil {
		return nil
	}
	stay := &StayDetails{CheckIn: *checkIn, CheckOut: *checkOut, Guests: *guests}
	from, errFrom := time.Parse("2006-01-02", *checkIn)
	to, errTo := time.Parse("2006-01-02", *checkOut)
	if errFrom == nil && errTo == nil {
		stay.Nights = int(to.Sub(from).Hours() / 24)
	}
	return stay
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// IsDirectSeller implements Store.
//
// NOTE: compiled but not yet exercised against a real database.
func (s *PostgresStore) IsDirectSeller(ctx context.Context, userID uuid.UUID) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM listings WHERE seller_user_id = $1)`, userID).Scan(&ok)
	return ok, err
}
