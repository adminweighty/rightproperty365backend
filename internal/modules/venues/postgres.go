package venues

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

// PostgresStore is the production Store (migration 0032). Every write is one
// short explicit transaction without session state, so it works behind a
// transaction pooler.
//
// The confirmation lock: Update takes the listing's venue_settings row FOR
// UPDATE before it locks the booking, so every change of a listing's
// bookings is serialised per listing; inside that, a state that holds
// capacity is checked against the other CONFIRMED (and held) rows, read
// with the tstzrange overlap index, before it is written. Two concurrent
// confirmations of overlapping requests therefore run one after the other,
// and the second sees the first and gets ErrSlotTaken.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func readSettings(ctx context.Context, q querier, listingID uuid.UUID, lock bool) (Settings, bool, error) {
	sql := `SELECT listing_id, enabled, currency, rate_per_hour::text, increment_minutes, min_minutes, max_minutes,
		       cleaning_buffer_minutes, capacity, deposit::text, opening_hours, exceptions, cancellation_policy,
		       free_cancellation_hours, lead_time_hours, horizon_days, updated_by, updated_at
		FROM venue_settings WHERE listing_id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var s Settings
	var hours, exceptions []byte
	err := q.QueryRow(ctx, sql, listingID).Scan(&s.ListingID, &s.Enabled, &s.Currency, &s.RatePerHour, &s.IncrementMinutes, &s.MinMinutes,
		&s.MaxMinutes, &s.CleaningBufferMinutes, &s.Capacity, &s.Deposit, &hours, &exceptions, &s.CancellationPolicy,
		&s.FreeCancellationHours, &s.LeadTimeHours, &s.HorizonDays, &s.UpdatedBy, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, fmt.Errorf("load venue settings: %w", err)
	}
	s.RatePerHour = NormaliseAmount(s.RatePerHour, s.Currency)
	if s.Deposit != nil {
		d := NormaliseAmount(*s.Deposit, s.Currency)
		s.Deposit = &d
	}
	s.UpdatedAt = s.UpdatedAt.UTC()
	if err := json.Unmarshal(hours, &s.OpeningHours); err != nil {
		return Settings{}, false, err
	}
	if err := json.Unmarshal(exceptions, &s.Exceptions); err != nil {
		return Settings{}, false, err
	}
	rows, err := q.Query(ctx, `
		SELECT code, label, kind, amount::text, to_char(effective_from, 'YYYY-MM-DD'), to_char(effective_to, 'YYYY-MM-DD')
		FROM venue_fees WHERE listing_id = $1 ORDER BY position`, listingID)
	if err != nil {
		return Settings{}, false, fmt.Errorf("load venue fees: %w", err)
	}
	s.Fees, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Fee, error) {
		var f Fee
		err := row.Scan(&f.Code, &f.Label, &f.Kind, &f.Amount, &f.EffectiveFrom, &f.EffectiveTo)
		if f.Kind == KindPercent {
			f.Amount = trimDecimal(mustRat(f.Amount))
		} else {
			f.Amount = NormaliseAmount(f.Amount, s.Currency)
		}
		return f, err
	})
	if err != nil {
		return Settings{}, false, fmt.Errorf("load venue fees: %w", err)
	}
	if s.OpeningHours == nil {
		s.OpeningHours = []OpeningRange{}
	}
	if s.Exceptions == nil {
		s.Exceptions = []Exception{}
	}
	if s.Fees == nil {
		s.Fees = []Fee{}
	}
	return s, true, nil
}

// Settings implements Store.
func (s *PostgresStore) Settings(ctx context.Context, listingID uuid.UUID) (Settings, bool, error) {
	return readSettings(ctx, s.pool, listingID, false)
}

// SettingsOf implements Store.
func (s *PostgresStore) SettingsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Settings, error) {
	out := map[uuid.UUID]Settings{}
	if len(ids) == 0 {
		return out, nil
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	rows, err := s.pool.Query(ctx, `SELECT listing_id, enabled, currency, rate_per_hour::text, increment_minutes
		FROM venue_settings WHERE listing_id = ANY($1::uuid[])`, strs)
	if err != nil {
		return nil, fmt.Errorf("load venue settings: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Settings, error) {
		var v Settings
		err := row.Scan(&v.ListingID, &v.Enabled, &v.Currency, &v.RatePerHour, &v.IncrementMinutes)
		v.RatePerHour = NormaliseAmount(v.RatePerHour, v.Currency)
		return v, err
	})
	if err != nil {
		return nil, fmt.Errorf("load venue settings: %w", err)
	}
	for _, v := range list {
		out[v.ListingID] = v
	}
	return out, nil
}

// PutSettings implements Store.
func (s *PostgresStore) PutSettings(ctx context.Context, v Settings) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := PutSettingsTx(ctx, tx, v); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PutSettingsTx writes a listing's settings and fees inside tx (also the
// development seed).
func PutSettingsTx(ctx context.Context, tx pgx.Tx, v Settings) error {
	hours, err := json.Marshal(v.OpeningHours)
	if err != nil {
		return err
	}
	exceptions, err := json.Marshal(v.Exceptions)
	if err != nil {
		return err
	}
	at := v.UpdatedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO venue_settings (listing_id, enabled, currency, rate_per_hour, increment_minutes, min_minutes, max_minutes,
			cleaning_buffer_minutes, capacity, deposit, opening_hours, exceptions, cancellation_policy, free_cancellation_hours,
			lead_time_hours, horizon_days, updated_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4::numeric, $5, $6, $7, $8, $9, $10::numeric, $11::jsonb, $12::jsonb, $13, $14, $15, $16, $17, $18, $18)
		ON CONFLICT (listing_id) DO UPDATE SET
			enabled = EXCLUDED.enabled, currency = EXCLUDED.currency, rate_per_hour = EXCLUDED.rate_per_hour,
			increment_minutes = EXCLUDED.increment_minutes, min_minutes = EXCLUDED.min_minutes, max_minutes = EXCLUDED.max_minutes,
			cleaning_buffer_minutes = EXCLUDED.cleaning_buffer_minutes, capacity = EXCLUDED.capacity, deposit = EXCLUDED.deposit,
			opening_hours = EXCLUDED.opening_hours, exceptions = EXCLUDED.exceptions, cancellation_policy = EXCLUDED.cancellation_policy,
			free_cancellation_hours = EXCLUDED.free_cancellation_hours, lead_time_hours = EXCLUDED.lead_time_hours,
			horizon_days = EXCLUDED.horizon_days, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
		v.ListingID, v.Enabled, v.Currency, v.RatePerHour, v.IncrementMinutes, v.MinMinutes, v.MaxMinutes,
		v.CleaningBufferMinutes, v.Capacity, v.Deposit, string(hours), string(exceptions), v.CancellationPolicy, v.FreeCancellationHours,
		v.LeadTimeHours, v.HorizonDays, v.UpdatedBy, at); err != nil {
		return fmt.Errorf("save venue settings: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM venue_fees WHERE listing_id = $1`, v.ListingID); err != nil {
		return fmt.Errorf("save venue fees: %w", err)
	}
	for i, f := range v.Fees {
		if _, err := tx.Exec(ctx, `
			INSERT INTO venue_fees (id, listing_id, position, code, label, kind, amount, effective_from, effective_to)
			VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8::date, $9::date)`,
			newID(), v.ListingID, i, f.Code, f.Label, f.Kind, f.Amount, f.EffectiveFrom, f.EffectiveTo); err != nil {
			return fmt.Errorf("save venue fee %s: %w", f.Code, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Quotes
// ---------------------------------------------------------------------------

// InsertQuote implements Store.
func (s *PostgresStore) InsertQuote(ctx context.Context, q Quote) error {
	return insertQuote(ctx, s.pool, q)
}

func insertQuote(ctx context.Context, x querier, q Quote) error {
	pricing, err := json.Marshal(q.Pricing)
	if err != nil {
		return err
	}
	_, err = x.Exec(ctx, `
		INSERT INTO venue_quotes (id, listing_id, start_at, end_at, buffer_minutes, time_zone, currency, pricing, used_by, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, NULL, $9, $10)`,
		q.ID, q.ListingID, q.Start, q.End, q.BufferMinutes, q.TimeZone, q.Pricing.Currency, string(pricing), q.CreatedAt, q.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert venue quote: %w", err)
	}
	return nil
}

// Quote implements Store.
func (s *PostgresStore) Quote(ctx context.Context, id uuid.UUID) (Quote, error) {
	var q Quote
	var pricing []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, listing_id, start_at, end_at, buffer_minutes, time_zone, pricing, used_by, created_at, expires_at
		FROM venue_quotes WHERE id = $1`, id).Scan(&q.ID, &q.ListingID, &q.Start, &q.End, &q.BufferMinutes, &q.TimeZone, &pricing,
		&q.UsedBy, &q.CreatedAt, &q.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, ErrQuoteNotFound
	}
	if err != nil {
		return Quote{}, fmt.Errorf("load venue quote: %w", err)
	}
	q.Start, q.End, q.CreatedAt, q.ExpiresAt = q.Start.UTC(), q.End.UTC(), q.CreatedAt.UTC(), q.ExpiresAt.UTC()
	return q, json.Unmarshal(pricing, &q.Pricing)
}

// ---------------------------------------------------------------------------
// Bookings
// ---------------------------------------------------------------------------

const bookingColumns = `b.id, b.reference, b.listing_id, b.listing_slug, b.listing_title, b.listing_reference, b.organisation_id, b.seller_user_id,
	b.customer_user_id, b.customer_name, b.customer_email, b.status, b.start_at, b.end_at, b.buffer_minutes, b.time_zone,
	b.attendees, COALESCE(b.message, ''), b.quote_id, b.pricing, b.policy_text, b.free_cancellation_hours, b.held, b.proposal,
	COALESCE(b.cancelled_by, ''), COALESCE(b.cancel_reason, ''), b.cancelled_at, b.cancel_free, COALESCE(b.decline_reason, ''),
	b.confirmations, b.consent_privacy_version, b.consent_accepted_at, b.created_at, b.updated_at`

func scanBooking(row pgx.Row) (Booking, error) {
	var b Booking
	var pricing, proposal []byte
	err := row.Scan(&b.ID, &b.Reference, &b.ListingID, &b.ListingSlug, &b.ListingTitle, &b.ListingReference, &b.OrganisationID, &b.SellerUserID,
		&b.CustomerUserID, &b.CustomerName, &b.CustomerEmail, &b.Status, &b.Start, &b.End, &b.BufferMinutes, &b.TimeZone,
		&b.Attendees, &b.Message, &b.QuoteID, &pricing, &b.PolicyText, &b.FreeCancellationHours, &b.Held, &proposal,
		&b.CancelledBy, &b.CancelReason, &b.CancelledAt, &b.CancelFree, &b.DeclineReason,
		&b.Confirmations, &b.PrivacyVersion, &b.ConsentAcceptedAt, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return Booking{}, err
	}
	b.Start, b.End, b.CreatedAt, b.UpdatedAt, b.ConsentAcceptedAt = b.Start.UTC(), b.End.UTC(), b.CreatedAt.UTC(), b.UpdatedAt.UTC(), b.ConsentAcceptedAt.UTC()
	if b.CancelledAt != nil {
		t := b.CancelledAt.UTC()
		b.CancelledAt = &t
	}
	if err := json.Unmarshal(pricing, &b.Pricing); err != nil {
		return Booking{}, err
	}
	if len(proposal) > 0 && string(proposal) != "null" {
		var p Proposal
		if err := json.Unmarshal(proposal, &p); err != nil {
			return Booking{}, err
		}
		p.Start, p.End = p.Start.UTC(), p.End.UTC()
		b.Proposal = &p
	}
	return b, nil
}

func encodeProposal(p *Proposal) (*string, error) {
	if p == nil {
		return nil, nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	s := string(raw)
	return &s, nil
}

func insertBooking(ctx context.Context, q querier, b Booking) error {
	pricing, err := json.Marshal(b.Pricing)
	if err != nil {
		return err
	}
	proposal, err := encodeProposal(b.Proposal)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO venue_bookings (id, reference, listing_id, listing_slug, listing_title, listing_reference, organisation_id, seller_user_id,
			customer_user_id, customer_name, customer_email, status, start_at, end_at, buffer_minutes, block_end_at, time_zone,
			attendees, message, quote_id, pricing, policy_text, free_cancellation_hours, held, proposal,
			cancelled_by, cancel_reason, cancelled_at, cancel_free, decline_reason, confirmations,
			consent_privacy_version, consent_accepted_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21::jsonb, $22, $23, $24,
		        $25::jsonb, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35)`,
		b.ID, b.Reference, b.ListingID, b.ListingSlug, b.ListingTitle, b.ListingReference, b.OrganisationID, b.SellerUserID,
		b.CustomerUserID, b.CustomerName, b.CustomerEmail, b.Status, b.Start, b.End, b.BufferMinutes, b.Block().End, b.TimeZone,
		b.Attendees, nullText(b.Message), b.QuoteID, string(pricing), b.PolicyText, b.FreeCancellationHours, b.Held, proposal,
		nullText(b.CancelledBy), nullText(b.CancelReason), b.CancelledAt, b.CancelFree, nullText(b.DeclineReason), b.Confirmations,
		b.PrivacyVersion, b.ConsentAcceptedAt, b.CreatedAt, b.UpdatedAt)
	return err
}

func insertEvent(ctx context.Context, q querier, e Event) error {
	_, err := q.Exec(ctx, `
		INSERT INTO venue_booking_events (id, booking_id, action, from_status, to_status, actor_type, actor_user_id, actor_name, reason,
			start_at, end_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		e.ID, e.BookingID, e.Action, nullText(e.FromStatus), e.ToStatus, e.ActorType, e.ActorUserID, e.ActorName, nullText(e.Reason),
		e.Start, e.End, e.At)
	if err != nil {
		return fmt.Errorf("insert venue booking event: %w", err)
	}
	return nil
}

func insertOutbox(ctx context.Context, q querier, eventType string, b Booking, e Event) error {
	payload, err := eventPayload(b, e)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
		uuid.New(), eventType, AggregateVenueBooking, b.ID, string(payload), e.At)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// useQuote marks a quote used by the booking (ErrQuoteUsed when another
// booking used it).
func useQuote(ctx context.Context, q querier, quoteID, bookingID uuid.UUID) error {
	tag, err := q.Exec(ctx, `UPDATE venue_quotes SET used_by = $2 WHERE id = $1 AND (used_by IS NULL OR used_by = $2)`, quoteID, bookingID)
	if err != nil {
		return fmt.Errorf("use venue quote: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM venue_quotes WHERE id = $1)`, quoteID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrQuoteNotFound
		}
		return ErrQuoteUsed
	}
	return nil
}

// BookingByQuote implements Store.
func (s *PostgresStore) BookingByQuote(ctx context.Context, quoteID uuid.UUID) (Booking, error) {
	b, err := scanBooking(s.pool.QueryRow(ctx, `SELECT `+bookingColumns+` FROM venue_bookings b WHERE b.quote_id = $1`, quoteID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, ErrNotFound
	}
	return b, err
}

// CreateBooking implements Store.
func (s *PostgresStore) CreateBooking(ctx context.Context, b Booking, e Event) (Booking, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Booking{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := insertBooking(ctx, tx, b); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			switch pgErr.ConstraintName {
			case "venue_bookings_quote_id_key":
				_ = tx.Rollback(context.WithoutCancel(ctx))
				existing, err := s.BookingByQuote(ctx, b.QuoteID)
				if err != nil {
					return Booking{}, false, err
				}
				return existing, false, nil
			case "venue_bookings_reference_key":
				return Booking{}, false, ErrReferenceCollision
			}
		}
		return Booking{}, false, fmt.Errorf("insert venue booking: %w", err)
	}
	if err := useQuote(ctx, tx, b.QuoteID, b.ID); err != nil {
		return Booking{}, false, err
	}
	if err := insertEvent(ctx, tx, e); err != nil {
		return Booking{}, false, err
	}
	if err := insertOutbox(ctx, tx, EventRequested, b, e); err != nil {
		return Booking{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Booking{}, false, err
	}
	return b, true, nil
}

// SeedTx replaces a sample booking with its quote and history, without
// outbox events (development seed only).
func SeedTx(ctx context.Context, tx pgx.Tx, q Quote, b Booking, events ...Event) error {
	if _, err := tx.Exec(ctx, `DELETE FROM venue_bookings WHERE id = $1`, b.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM venue_quotes WHERE id = $1`, q.ID); err != nil {
		return err
	}
	if err := insertQuote(ctx, tx, q); err != nil {
		return err
	}
	if err := insertBooking(ctx, tx, b); err != nil {
		return fmt.Errorf("seed venue booking: %w", err)
	}
	if err := useQuote(ctx, tx, q.ID, b.ID); err != nil {
		return err
	}
	for _, e := range events {
		if err := insertEvent(ctx, tx, e); err != nil {
			return err
		}
	}
	return nil
}

// Booking implements Store.
func (s *PostgresStore) Booking(ctx context.Context, id uuid.UUID) (Booking, error) {
	b, err := scanBooking(s.pool.QueryRow(ctx, `SELECT `+bookingColumns+` FROM venue_bookings b WHERE b.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, ErrNotFound
	}
	return b, err
}

// Events implements Store.
func (s *PostgresStore) Events(ctx context.Context, id uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, booking_id, action, COALESCE(from_status, ''), to_status, actor_type, actor_user_id, actor_name, COALESCE(reason, ''),
		       start_at, end_at, created_at
		FROM venue_booking_events WHERE booking_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.ID, &e.BookingID, &e.Action, &e.FromStatus, &e.ToStatus, &e.ActorType, &e.ActorUserID, &e.ActorName, &e.Reason,
			&e.Start, &e.End, &e.At)
		e.At = e.At.UTC()
		for _, t := range []**time.Time{&e.Start, &e.End} {
			if *t != nil {
				u := (*t).UTC()
				*t = &u
			}
		}
		return e, err
	})
	if out == nil {
		out = []Event{}
	}
	return out, err
}

func occupiedRows(ctx context.Context, q querier, listingID uuid.UUID, from, to time.Time, except *uuid.UUID) ([]Booking, error) {
	rows, err := q.Query(ctx, `SELECT `+bookingColumns+` FROM venue_bookings b
		WHERE b.listing_id = $1 AND (b.status = 'CONFIRMED' OR (b.status = 'RESCHEDULE_REQUESTED' AND b.held))
		  AND b.occupied && tstzrange($2, $3, '[)') AND ($4::uuid IS NULL OR b.id <> $4)
		ORDER BY b.start_at`, listingID, from, to, except)
	if err != nil {
		return nil, fmt.Errorf("load occupied slots: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Booking, error) { return scanBooking(row) })
}

// Occupied implements Store.
func (s *PostgresStore) Occupied(ctx context.Context, listingID uuid.UUID, from, to time.Time, except *uuid.UUID) ([]Booking, error) {
	return occupiedRows(ctx, s.pool, listingID, from, to, except)
}

// Update implements Store (see PostgresStore for the lock).
func (s *PostgresStore) Update(ctx context.Context, id uuid.UUID, decide func(Booking, Settings) (Mutation, error)) (Booking, error) {
	var listingID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT listing_id FROM venue_bookings WHERE id = $1`, id).Scan(&listingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, ErrNotFound
	}
	if err != nil {
		return Booking{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Booking{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// 1. The per-listing lock row, always before the booking row.
	settings, ok, err := readSettings(ctx, tx, listingID, true)
	if err != nil {
		return Booking{}, err
	}
	if !ok {
		// Not reachable through the API (a booking needs settings); the
		// listing is still serialised, with a transaction advisory lock.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 32))`, listingID.String()); err != nil {
			return Booking{}, err
		}
		settings = DefaultSettings(listingID, "")
	}
	// 2. The booking.
	cur, err := scanBooking(tx.QueryRow(ctx, `SELECT `+bookingColumns+` FROM venue_bookings b WHERE b.id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, ErrNotFound
	}
	if err != nil {
		return Booking{}, err
	}
	m, err := decide(cur, settings)
	if err != nil {
		return Booking{}, err
	}
	next := m.Next
	if m.Lock {
		block := next.Block()
		others, err := occupiedRows(ctx, tx, listingID, block.Start, block.End, &next.ID)
		if err != nil {
			return Booking{}, err
		}
		if err := lockCheck(next, settings, others); err != nil {
			return Booking{}, err
		}
	}
	if m.UseQuote != nil {
		if err := useQuote(ctx, tx, *m.UseQuote, next.ID); err != nil {
			return Booking{}, err
		}
	}
	pricing, err := json.Marshal(next.Pricing)
	if err != nil {
		return Booking{}, err
	}
	proposal, err := encodeProposal(next.Proposal)
	if err != nil {
		return Booking{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE venue_bookings SET status = $2, start_at = $3, end_at = $4, buffer_minutes = $5, block_end_at = $6, quote_id = $7,
			pricing = $8::jsonb, held = $9, proposal = $10::jsonb, cancelled_by = $11, cancel_reason = $12, cancelled_at = $13,
			cancel_free = $14, decline_reason = $15, confirmations = $16, updated_at = $17
		WHERE id = $1`,
		next.ID, next.Status, next.Start, next.End, next.BufferMinutes, next.Block().End, next.QuoteID, string(pricing), next.Held, proposal,
		nullText(next.CancelledBy), nullText(next.CancelReason), next.CancelledAt, next.CancelFree, nullText(next.DeclineReason),
		next.Confirmations, next.UpdatedAt); err != nil {
		return Booking{}, fmt.Errorf("update venue booking: %w", err)
	}
	if err := insertEvent(ctx, tx, m.Event); err != nil {
		return Booking{}, err
	}
	if m.Outbox != "" {
		if err := insertOutbox(ctx, tx, m.Outbox, next, m.Event); err != nil {
			return Booking{}, err
		}
	}
	if m.Audit != nil {
		if err := orgaudit.Insert(ctx, tx, *m.Audit); err != nil {
			return Booking{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Booking{}, err
	}
	return next, nil
}

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, f Filter) ([]Booking, int, error) {
	var parts []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	switch {
	case f.CustomerUserID != nil:
		parts = append(parts, `b.customer_user_id = `+arg(*f.CustomerUserID))
	case f.OrganisationID != nil:
		parts = append(parts, `b.organisation_id = `+arg(*f.OrganisationID))
	case f.SellerUserID != nil:
		parts = append(parts, `b.organisation_id IS NULL AND b.seller_user_id = `+arg(*f.SellerUserID))
	default:
		return []Booking{}, 0, nil
	}
	if f.ListingIDs != nil {
		ids := make([]string, len(f.ListingIDs))
		for i, id := range f.ListingIDs {
			ids[i] = id.String()
		}
		parts = append(parts, `b.listing_id = ANY(`+arg(ids)+`::uuid[])`)
	}
	if f.ListingID != nil {
		parts = append(parts, `b.listing_id = `+arg(*f.ListingID))
	}
	if len(f.Statuses) > 0 {
		parts = append(parts, `b.status = ANY(`+arg(f.Statuses)+`::text[])`)
	}
	if f.FromDay != "" {
		parts = append(parts, `(b.start_at AT TIME ZONE b.time_zone)::date >= `+arg(f.FromDay)+`::date`)
	}
	if f.ToDay != "" {
		parts = append(parts, `(b.start_at AT TIME ZONE b.time_zone)::date <= `+arg(f.ToDay)+`::date`)
	}
	where := strings.Join(parts, " AND ")
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM venue_bookings b WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count venue bookings: %w", err)
	}
	limit, offset := arg(f.PageSize), arg(max((f.Page-1)*f.PageSize, 0))
	rows, err := s.pool.Query(ctx, `SELECT `+bookingColumns+` FROM venue_bookings b WHERE `+where+
		` ORDER BY b.start_at, b.id LIMIT `+limit+` OFFSET `+offset, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list venue bookings: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Booking, error) { return scanBooking(row) })
	if err != nil {
		return nil, 0, err
	}
	if out == nil {
		out = []Booking{}
	}
	return out, total, nil
}
