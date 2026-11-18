// Package venues implements hourly rental of showrooms, venues and other
// eligible commercial listings (API 0.33.0, BR-007, FR-022 to FR-025).
//
// A lister configures a listing's hourly settings (rate, ISO currency,
// increment, opening hours and exceptions, minimum and maximum duration,
// cleaning buffer, capacity, deposit, fees with effective dates and the
// cancellation policy). The public reads the availability in the venue's
// local time and gets a priced, stored quote; a signed-in customer turns a
// quote into a booking REQUEST. The host confirms it: the slot is locked
// then, in one critical section per listing, so two overlapping requests
// can never both be confirmed.
//
// Enquiry mode only: no payment is ever taken or implied (decision D-04).
// Bookings are their own table and service type (VENUE_HOURLY), so reports
// keep them apart from viewing and consultation appointments (FR-022).
package venues

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// ServiceType is the service type of every booking here, distinct from the
// appointments' VIEWING and CONSULTATION.
const ServiceType = "VENUE_HOURLY"

// ModeEnquiry is the only booking mode: the host confirms and arranges
// payment directly with the customer.
const ModeEnquiry = "ENQUIRY"

// Notice is shown with every quote and booking, word for word.
const Notice = "This is an estimate. No payment is taken on RightProperty365; the host confirms and arranges payment with you."

// DepositNote says how the deposit is meant.
const DepositNote = "payable to the host if confirmed"

// QuoteLifetime is how long a quote can be turned into a request.
const QuoteLifetime = 30 * time.Minute

// MaxAvailabilityDays is the longest window GET .../availability answers.
const MaxAvailabilityDays = 62

// Fee kinds.
const (
	KindRate    = "RATE"
	KindPercent = "PERCENT"
	KindFixed   = "FIXED"
	KindPerHour = "PER_HOUR"
)

// FeeKinds lists the kinds a fee may have.
func FeeKinds() []string { return []string{KindPercent, KindFixed, KindPerHour} }

// Booking statuses.
const (
	StatusRequested           = "REQUESTED"
	StatusConfirmed           = "CONFIRMED"
	StatusRescheduleRequested = "RESCHEDULE_REQUESTED"
	StatusDeclined            = "DECLINED"
	StatusCancelled           = "CANCELLED"
	StatusCompleted           = "COMPLETED"
	StatusNoShow              = "NO_SHOW"
)

// Statuses returns every status.
func Statuses() []string {
	return []string{StatusRequested, StatusConfirmed, StatusRescheduleRequested, StatusDeclined, StatusCancelled, StatusCompleted, StatusNoShow}
}

// StatusLabel is how a status is shown.
func StatusLabel(s string) string {
	switch s {
	case StatusRequested:
		return "Requested"
	case StatusConfirmed:
		return "Confirmed"
	case StatusRescheduleRequested:
		return "New time requested"
	case StatusDeclined:
		return "Declined"
	case StatusCancelled:
		return "Cancelled"
	case StatusCompleted:
		return "Completed"
	case StatusNoShow:
		return "No-show"
	}
	return s
}

// Parties.
const (
	PartyCustomer = "CUSTOMER"
	PartyHost     = "HOST"
	PartySystem   = "SYSTEM"
)

// Event actions (venue_booking_events.action).
const (
	ActionRequested           = "REQUESTED"
	ActionConfirmed           = "CONFIRMED"
	ActionDeclined            = "DECLINED"
	ActionCancelled           = "CANCELLED"
	ActionRescheduleRequested = "RESCHEDULE_REQUESTED"
	ActionProposed            = "PROPOSED"
	ActionAccepted            = "ACCEPTED"
	ActionCompleted           = "COMPLETED"
	ActionNoShow              = "NO_SHOW"
)

// Reasons a slot is unavailable (409 SLOT_UNAVAILABLE details.reason).
const (
	ReasonClosed         = "CLOSED"
	ReasonTaken          = "TAKEN"
	ReasonBuffer         = "BUFFER"
	ReasonLeadTime       = "LEAD_TIME"
	ReasonOutsideHorizon = "OUTSIDE_HORIZON"
)

// Paging.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Errors mapped to HTTP answers by the handler.
var (
	ErrUnauthenticated     = errors.New("sign in first")
	ErrForbidden           = errors.New("not allowed")
	ErrListingNotFound     = errors.New("listing not found")
	ErrListingNotEligible  = errors.New("this property type cannot be booked by the hour")
	ErrVenueNotFound       = errors.New("venue not found or not bookable by the hour")
	ErrQuoteNotFound       = errors.New("quote not found")
	ErrQuoteUsed           = errors.New("quote already used")
	ErrNotFound            = errors.New("venue booking not found")
	ErrOrganisationUnknown = errors.New("organisation not found")
	ErrSlotTaken           = errors.New("the slot is already taken by a confirmed booking")
	ErrMixedCurrency       = errors.New("a fee is in another currency than the rate")
	ErrReferenceCollision  = errors.New("reference already in use")
)

// ValidationError carries per-field messages (400).
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// UnavailableError is a slot that cannot be booked (409 SLOT_UNAVAILABLE).
type UnavailableError struct{ Reason string }

func (e *UnavailableError) Error() string { return "slot unavailable: " + e.Reason }

// QuoteExpiredError is a quote past its expiry (409 QUOTE_EXPIRED).
type QuoteExpiredError struct{ ExpiredAt time.Time }

func (e *QuoteExpiredError) Error() string { return "quote expired" }

// TransitionError is an action the booking's status does not allow (409).
type TransitionError struct {
	From    string
	Action  string
	Allowed []string
	// Why explains a time rule ("" for a plain status rule).
	Why string
}

func (e *TransitionError) Error() string { return "invalid booking transition" }

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// OpeningRange is one opening range of a weekday, in venue-local wall time.
type OpeningRange struct {
	Weekday int    `json:"weekday"` // 1 = Monday ... 7 = Sunday
	Opens   string `json:"opens"`   // "HH:MM"
	Closes  string `json:"closes"`  // "HH:MM", up to "24:00"
}

// Exception changes one local date: closed, or open with its own hours.
type Exception struct {
	Date   string  `json:"date"`
	Closed bool    `json:"closed"`
	Opens  *string `json:"opens"`
	Closes *string `json:"closes"`
	Note   *string `json:"note"`
}

// Fee is a tax or fee rule. PERCENT amounts are percentages; FIXED and
// PER_HOUR amounts are money in the settings' currency.
type Fee struct {
	Code          string  `json:"code"`
	Label         string  `json:"label"`
	Kind          string  `json:"kind"`
	Amount        string  `json:"amount"`
	Currency      string  `json:"currency,omitempty"`
	EffectiveFrom string  `json:"effective_from"`
	EffectiveTo   *string `json:"effective_to"`
}

// Settings are a listing's hourly settings.
type Settings struct {
	ListingID             uuid.UUID
	Enabled               bool
	Currency              string
	RatePerHour           string
	IncrementMinutes      int
	MinMinutes            int
	MaxMinutes            int
	CleaningBufferMinutes int
	Capacity              int
	Deposit               *string
	OpeningHours          []OpeningRange
	Exceptions            []Exception
	Fees                  []Fee
	CancellationPolicy    string
	FreeCancellationHours int
	LeadTimeHours         int
	HorizonDays           int
	UpdatedBy             *uuid.UUID
	UpdatedAt             time.Time
}

// DefaultSettings are what an unconfigured listing shows.
func DefaultSettings(listingID uuid.UUID, currency string) Settings {
	return Settings{ListingID: listingID, Currency: currency, IncrementMinutes: 60, MinMinutes: 60, MaxMinutes: 480, Capacity: 1,
		OpeningHours: []OpeningRange{}, Exceptions: []Exception{}, Fees: []Fee{}, FreeCancellationHours: 24, LeadTimeHours: 24, HorizonDays: 90}
}

// ---------------------------------------------------------------------------
// Quotes and pricing
// ---------------------------------------------------------------------------

// Line is one itemised line of a quote.
type Line struct {
	Code   string `json:"code"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Amount string `json:"amount"`
}

// Pricing is what a quote costs, in one currency.
type Pricing struct {
	Currency              string  `json:"currency"`
	Lines                 []Line  `json:"lines"`
	Subtotal              string  `json:"subtotal"`
	Deposit               *string `json:"deposit"`
	TotalExcludingDeposit string  `json:"total_excluding_deposit"`
}

// Quote is a stored quote.
type Quote struct {
	ID            uuid.UUID
	ListingID     uuid.UUID
	Start, End    time.Time
	BufferMinutes int
	TimeZone      string
	Pricing       Pricing
	CreatedAt     time.Time
	ExpiresAt     time.Time
	// UsedBy is the booking that used the quote (request, reschedule or proposal).
	UsedBy *uuid.UUID
}

// DurationMinutes is the real elapsed length of the slot.
func (q Quote) DurationMinutes() int { return int(q.End.Sub(q.Start) / time.Minute) }

// ---------------------------------------------------------------------------
// Bookings
// ---------------------------------------------------------------------------

// Proposal is a new time on the table: asked for by the customer
// (reschedule) or proposed by the host.
type Proposal struct {
	By            string    `json:"by"`
	QuoteID       uuid.UUID `json:"quote_id"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	BufferMinutes int       `json:"buffer_minutes"`
	Pricing       Pricing   `json:"pricing"`
}

// Booking is one hourly booking (request).
type Booking struct {
	ID               uuid.UUID
	Reference        string
	ListingID        uuid.UUID
	ListingSlug      string
	ListingTitle     string
	ListingReference string
	// Who hosts it: the listing's organisation, or its direct owner.
	OrganisationID *uuid.UUID
	SellerUserID   *uuid.UUID

	CustomerUserID uuid.UUID
	CustomerName   string
	CustomerEmail  string

	Status        string
	Start, End    time.Time
	BufferMinutes int
	TimeZone      string
	Attendees     *int
	Message       string
	QuoteID       uuid.UUID
	Pricing       Pricing

	PolicyText            string
	FreeCancellationHours int

	// Held: the booking's current slot counts against the capacity
	// (CONFIRMED, or RESCHEDULE_REQUESTED while the confirmed slot stays).
	Held     bool
	Proposal *Proposal

	CancelledBy   string
	CancelReason  string
	CancelledAt   *time.Time
	CancelFree    *bool
	DeclineReason string
	// Confirmations counts how many times the booking was confirmed (the
	// first one is the analytics booking_confirm).
	Confirmations int

	PrivacyVersion    string
	ConsentAcceptedAt time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// DurationMinutes is the real elapsed length of the booking.
func (b Booking) DurationMinutes() int { return int(b.End.Sub(b.Start) / time.Minute) }

// Occupies reports whether the booking's current slot takes capacity.
func (b Booking) Occupies() bool {
	return b.Status == StatusConfirmed || (b.Status == StatusRescheduleRequested && b.Held)
}

// Block is the time a booking takes, its cleaning buffer included.
func (b Booking) Block() Interval {
	return Interval{Start: b.Start, End: b.End.Add(time.Duration(b.BufferMinutes) * time.Minute)}
}

// FreeUntil is the last moment the customer may cancel free of charge.
func (b Booking) FreeUntil() time.Time {
	return b.Start.Add(-time.Duration(b.FreeCancellationHours) * time.Hour)
}

// Event is one immutable entry of a booking's history.
type Event struct {
	ID          uuid.UUID
	BookingID   uuid.UUID
	Action      string
	FromStatus  string
	ToStatus    string
	ActorType   string
	ActorUserID *uuid.UUID
	ActorName   string
	Reason      string
	Start, End  *time.Time
	At          time.Time
}

// Filter selects bookings. Exactly one of CustomerUserID, OrganisationID
// and SellerUserID is set; ListingIDs narrows an organisation's bookings to
// an agent's listings (nil: all of them).
type Filter struct {
	CustomerUserID *uuid.UUID
	OrganisationID *uuid.UUID
	SellerUserID   *uuid.UUID
	ListingIDs     []uuid.UUID
	ListingID      *uuid.UUID
	Statuses       []string
	// FromDay and ToDay bound the venue-local start date (inclusive, "" = open).
	FromDay, ToDay string
	Page, PageSize int
}

// Matches reports whether b passes the filter (the memory store; the
// Postgres store says the same in SQL).
func (f Filter) Matches(b Booking) bool {
	switch {
	case f.CustomerUserID != nil:
		if b.CustomerUserID != *f.CustomerUserID {
			return false
		}
	case f.OrganisationID != nil:
		if b.OrganisationID == nil || *b.OrganisationID != *f.OrganisationID {
			return false
		}
	case f.SellerUserID != nil:
		if b.OrganisationID != nil || b.SellerUserID == nil || *b.SellerUserID != *f.SellerUserID {
			return false
		}
	default:
		return false
	}
	if f.ListingIDs != nil && !containsID(f.ListingIDs, b.ListingID) {
		return false
	}
	if f.ListingID != nil && b.ListingID != *f.ListingID {
		return false
	}
	if len(f.Statuses) > 0 {
		found := false
		for _, s := range f.Statuses {
			found = found || s == b.Status
		}
		if !found {
			return false
		}
	}
	if f.FromDay != "" || f.ToDay != "" {
		day := LocalDay(b.Start, b.TimeZone)
		if (f.FromDay != "" && day < f.FromDay) || (f.ToDay != "" && day > f.ToDay) {
			return false
		}
	}
	return true
}

func containsID(ids []uuid.UUID, id uuid.UUID) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// LocalDay is the date of t in the zone ("2006-01-02").
func LocalDay(t time.Time, zone string) string { return t.In(location(zone)).Format(dateLayout) }

const dateLayout = "2006-01-02"

// location loads an IANA zone (UTC when unknown).
func location(zone string) *time.Location {
	if loc, err := time.LoadLocation(zone); err == nil && zone != "" {
		return loc
	}
	return time.UTC
}
