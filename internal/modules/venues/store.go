package venues

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Outbox events.
const (
	AggregateVenueBooking = "venue_booking"

	EventRequested           = "VENUE_BOOKING_REQUESTED"
	EventConfirmed           = "VENUE_BOOKING_CONFIRMED"
	EventDeclined            = "VENUE_BOOKING_DECLINED"
	EventCancelled           = "VENUE_BOOKING_CANCELLED"
	EventRescheduleRequested = "VENUE_BOOKING_RESCHEDULE_REQUESTED"
	EventProposed            = "VENUE_BOOKING_PROPOSED"
	EventCompleted           = "VENUE_BOOKING_COMPLETED"
	EventNoShow              = "VENUE_BOOKING_NO_SHOW"
)

// EventTypes lists every outbox event this module writes.
func EventTypes() []string {
	return []string{EventRequested, EventConfirmed, EventDeclined, EventCancelled, EventRescheduleRequested, EventProposed, EventCompleted, EventNoShow}
}

// Mutation is what one transition writes, all together.
type Mutation struct {
	Next  Booking
	Event Event
	// Outbox is the event type to write ("" for none).
	Outbox string
	// Lock: the next state takes capacity at its slot. The store checks,
	// inside the per-listing critical section, that the other occupying
	// bookings leave room for its block, and answers ErrSlotTaken if not.
	Lock bool
	// UseQuote marks a quote as used by this booking (ErrQuoteUsed when
	// another booking used it).
	UseQuote *uuid.UUID
	// Audit is the organisation audit record (host actions on an
	// organisation's booking).
	Audit *orgaudit.Event
}

// Store persists settings, quotes and bookings.
type Store interface {
	// Settings returns a listing's settings; ok is false when it has none.
	Settings(ctx context.Context, listingID uuid.UUID) (Settings, bool, error)
	// SettingsOf returns the settings of those listings among ids that have
	// some (without their fees: the cards' summary).
	SettingsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Settings, error)
	// PutSettings stores (replaces) a listing's settings.
	PutSettings(ctx context.Context, s Settings) error

	// InsertQuote stores a quote. Quote returns one, or ErrQuoteNotFound.
	InsertQuote(ctx context.Context, q Quote) error
	Quote(ctx context.Context, id uuid.UUID) (Quote, error)

	// Occupied returns the listing's bookings that take capacity and whose
	// blocks overlap [from, to), leaving out except.
	Occupied(ctx context.Context, listingID uuid.UUID, from, to time.Time, except *uuid.UUID) ([]Booking, error)

	// CreateBooking stores a new REQUESTED booking with its first event and
	// its outbox event, and marks its quote used, all together. When a
	// booking already uses the quote nothing is written and that booking
	// is returned with created=false; ErrQuoteUsed when a reschedule or a
	// proposal of another booking used it. ErrReferenceCollision asks the
	// caller to retry with another reference.
	CreateBooking(ctx context.Context, b Booking, e Event) (stored Booking, created bool, err error)
	// BookingByQuote returns the booking created from a quote, or ErrNotFound.
	BookingByQuote(ctx context.Context, quoteID uuid.UUID) (Booking, error)
	// Booking returns one booking, or ErrNotFound. Events its history, oldest first.
	Booking(ctx context.Context, id uuid.UUID) (Booking, error)
	Events(ctx context.Context, id uuid.UUID) ([]Event, error)
	// Update changes one booking atomically: under the listing's lock (the
	// settings row in Postgres, the mutex in memory) and the booking's, it
	// calls decide with the current booking and the listing's settings and
	// writes the mutation, or returns decide's error and writes nothing.
	Update(ctx context.Context, id uuid.UUID, decide func(cur Booking, s Settings) (Mutation, error)) (Booking, error)
	// List returns one page of bookings, soonest start first, and the total.
	List(ctx context.Context, f Filter) ([]Booking, int, error)
}

// EventPayload is the body of every venue booking outbox event: identifiers,
// statuses and times only, never personal data.
type EventPayload struct {
	BookingID         uuid.UUID  `json:"booking_id"`
	Reference         string     `json:"reference"`
	ListingID         uuid.UUID  `json:"listing_id"`
	ServiceType       string     `json:"service_type"`
	Action            string     `json:"action"`
	From              string     `json:"from"`
	To                string     `json:"to"`
	Actor             string     `json:"actor"`
	Start             time.Time  `json:"start"`
	End               time.Time  `json:"end"`
	ProposedStart     *time.Time `json:"proposed_start,omitempty"`
	ProposedEnd       *time.Time `json:"proposed_end,omitempty"`
	ProposedBy        string     `json:"proposed_by,omitempty"`
	CancelledBy       string     `json:"cancelled_by,omitempty"`
	FirstConfirmation bool       `json:"first_confirmation"`
	OccurredAt        time.Time  `json:"occurred_at"`
}

func eventPayload(b Booking, e Event) ([]byte, error) {
	p := EventPayload{BookingID: b.ID, Reference: b.Reference, ListingID: b.ListingID, ServiceType: ServiceType, Action: e.Action,
		From: e.FromStatus, To: e.ToStatus, Actor: e.ActorType, Start: b.Start, End: b.End, CancelledBy: b.CancelledBy,
		FirstConfirmation: e.ToStatus == StatusConfirmed && b.Confirmations == 1, OccurredAt: e.At}
	if b.Proposal != nil {
		s, en := b.Proposal.Start, b.Proposal.End
		p.ProposedStart, p.ProposedEnd, p.ProposedBy = &s, &en, b.Proposal.By
	}
	return json.Marshal(p)
}

// occupiedOf turns bookings into the slots availability works with.
func occupiedOf(list []Booking) []Occupied {
	out := make([]Occupied, 0, len(list))
	for _, b := range list {
		out = append(out, Occupied{Slot: Interval{Start: b.Start, End: b.End}, Block: b.Block()})
	}
	return out
}

// lockCheck is the capacity rule both stores run inside their critical
// section: next's block must fit beside the other occupying blocks.
func lockCheck(next Booking, s Settings, others []Booking) error {
	blocks := make([]Interval, 0, len(others))
	for _, o := range others {
		if o.ID != next.ID && o.Occupies() {
			blocks = append(blocks, o.Block())
		}
	}
	if !fits(blocks, next.Block(), s.Capacity) {
		return ErrSlotTaken
	}
	return nil
}
