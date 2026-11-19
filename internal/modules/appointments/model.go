// Package appointments implements appointments between a customer and a
// professional: property VIEWINGS (tied to a listing) and CONSULTATIONS (tied
// to a listing's advertiser or to a directory profile).
//
// An appointment is created either together with a property request that
// carries a preferred slot (same transaction) or explicitly by a signed-in
// customer. It is routed with the SAME rules as property requests and business
// enquiries, and every change of status is appended to an immutable timeline
// and written to the outbox in the same transaction.
//
// Not here (see the README): venue and short-stay bookings, availability
// calendars, calendar sync, reminders and push notifications. Calendar
// invitations (ICS) are in calendar.go (API 0.39.0).
package appointments

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Appointment types.
const (
	TypeViewing      = "VIEWING"
	TypeConsultation = "CONSULTATION"
)

// Statuses. See transitions in service.go.
const (
	StatusRequested           = "REQUESTED"
	StatusConfirmed           = "CONFIRMED"
	StatusRescheduleRequested = "RESCHEDULE_REQUESTED"
	StatusCompleted           = "COMPLETED"
	StatusNoShow              = "NO_SHOW"
	StatusCancelled           = "CANCELLED"
)

// Statuses returns every status, in lifecycle order.
func Statuses() []string {
	return []string{StatusRequested, StatusConfirmed, StatusRescheduleRequested, StatusCompleted, StatusNoShow, StatusCancelled}
}

// Types returns every appointment type.
func Types() []string { return []string{TypeViewing, TypeConsultation} }

// openStatuses are the statuses an appointment can still change from.
var openStatuses = []string{StatusRequested, StatusConfirmed, StatusRescheduleRequested}

// StatusLabel is how a status is shown, in title case as the mobile app writes it.
func StatusLabel(status string) string {
	switch status {
	case StatusRequested:
		return "Requested"
	case StatusConfirmed:
		return "Confirmed"
	case StatusRescheduleRequested:
		return "Reschedule Requested"
	case StatusCompleted:
		return "Completed"
	case StatusNoShow:
		return "No Show"
	case StatusCancelled:
		return "Cancelled"
	}
	return status
}

// Parties: who proposed a time, cancelled, or made a timeline entry.
const (
	PartyCustomer     = "CUSTOMER"
	PartyProfessional = "PROFESSIONAL"
	PartySystem       = "SYSTEM"
)

// Recipient types, the same values property requests use.
const (
	RecipientAgent        = "AGENT"
	RecipientOrganisation = "ORGANISATION"
	RecipientSeller       = "SELLER"
)

// Duration limits, in minutes.
const (
	DefaultDurationMinutes = 30
	MinDurationMinutes     = 15
	MaxDurationMinutes     = 240
)

// Paging limits.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// MaxAdvance is how far ahead an appointment may start.
const MaxAdvance = 366 * 24 * time.Hour

// Which appointments a customer list returns.
const (
	WhenUpcoming = "upcoming"
	WhenPast     = "past"
	WhenAll      = "all"
)

// Domain errors mapped to HTTP statuses by the handler.
var (
	ErrNotFound             = errors.New("appointment not found")
	ErrInvalidState         = errors.New("appointment is not in a state that allows this")
	ErrSlotTaken            = errors.New("the professional already has a confirmed appointment at that time")
	ErrIdempotencyConflict  = errors.New("idempotency key reused with different content")
	ErrReferenceCollision   = errors.New("reference already in use")
	ErrListingNotFound      = errors.New("listing not found")
	ErrListingUnavailable   = errors.New("listing is not accepting appointments")
	ErrRecipientNotFound    = errors.New("profile not found")
	ErrRecipientUnavailable = errors.New("profile is not accepting appointments")
	ErrForbidden            = errors.New("not allowed")
	ErrNotAMember           = errors.New("the user is not a member of the organisation")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// Appointment is one appointment as stored. The customer's contact details
// are private: they leave the store only towards the authorised professional.
type Appointment struct {
	ID        uuid.UUID
	Reference string
	Type      string
	Status    string
	MarketID  uuid.UUID
	// Timezone is the market's IANA zone, in which times are shown to people.
	Timezone string

	// What the appointment is about: a listing, or a directory profile. The
	// names are a snapshot taken at creation, so the history stays readable
	// after a listing expires.
	ListingID        *uuid.UUID
	ListingSlug      string
	ListingTitle     string
	ListingReference string
	// Profile is the directory profile the appointment was addressed to
	// (explicit creation with `recipient`); empty otherwise.
	ProfileKind string
	ProfileSlug string
	// With is the PUBLIC profile of the professional or business the customer
	// meets: the addressed profile, or the listing's agent or agency. Empty
	// for a private seller.
	WithKind string
	WithSlug string
	WithName string

	// What created it.
	PropertyRequestID *uuid.UUID
	BusinessEnquiryID *uuid.UUID
	// LinkedReference is the reference of that request or enquiry.
	LinkedReference string

	// The customer: an account (nil for a guest) and a private contact snapshot.
	CustomerUserID         *uuid.UUID
	CustomerName           string
	CustomerEmail          string
	CustomerPhone          string
	PreferredContactMethod string
	CustomerNote           string

	// The professional side, resolved with the request / enquiry routing rules.
	RecipientType  string
	OrganisationID *uuid.UUID
	AssignedUserID *uuid.UUID

	StartUTC        time.Time
	DurationMinutes int
	LocationNote    string

	ProposedStartUTC *time.Time
	ProposedBy       string

	CancelledBy  string
	CancelReason string

	// Explicit creation only.
	IdempotencyKey    string
	RequestHash       string
	PrivacyVersion    string
	ConsentAcceptedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// EndUTC is when the appointment ends.
func (a Appointment) EndUTC() time.Time {
	return a.StartUTC.Add(time.Duration(a.DurationMinutes) * time.Minute)
}

// Open reports whether the appointment can still change.
func (a Appointment) Open() bool {
	return a.Status == StatusRequested || a.Status == StatusConfirmed || a.Status == StatusRescheduleRequested
}

// BusyKey identifies the professional whose time the appointment takes: the
// assigned user, or the organisation when nobody is assigned.
func (a Appointment) BusyKey() (uuid.UUID, bool) {
	switch {
	case a.AssignedUserID != nil:
		return *a.AssignedUserID, true
	case a.OrganisationID != nil:
		return *a.OrganisationID, true
	}
	return uuid.Nil, false
}

// Overlaps reports whether two appointments share any time. Back-to-back
// appointments (one ends when the other starts) do not overlap.
func (a Appointment) Overlaps(b Appointment) bool {
	return a.StartUTC.Before(b.EndUTC()) && b.StartUTC.Before(a.EndUTC())
}

// Timeline events.
const (
	TimelineRequested           = "REQUESTED"
	TimelineConfirmed           = "CONFIRMED"
	TimelineRescheduleRequested = "RESCHEDULE_REQUESTED"
	TimelineCancelled           = "CANCELLED"
	TimelineCompleted           = "COMPLETED"
	TimelineNoShow              = "NO_SHOW"
	TimelineAssigned            = "ASSIGNED"
	// TimelineUnassigned: the assigned member left the team (removed or
	// suspended) and the appointment went back to the organisation.
	TimelineUnassigned = "UNASSIGNED"
)

// TimelineEntry is one immutable row of appointment_timeline.
type TimelineEntry struct {
	ID            uuid.UUID
	AppointmentID uuid.UUID
	Event         string
	// ActorType is CUSTOMER, PROFESSIONAL or SYSTEM; ActorUserID is the
	// account that acted (nil for a guest or the system).
	ActorType   string
	ActorUserID *uuid.UUID
	FromStatus  string // "" for the first entry
	ToStatus    string
	Note        string
	// StartUTC is the time the entry is about: the requested, confirmed or
	// proposed start. Nil when the entry is not about a time.
	StartUTC *time.Time
	At       time.Time
}

// Scope says which appointments a professional may see and act on. It is
// built on the server from the caller's identity, memberships and
// capabilities (auth.MayManageAppointments, auth.IsDirectOwner) and is the
// ONLY thing that selects rows. An appointment is visible when ANY holds:
//   - its organisation is in ManagedOrganisationIDs (the caller is OWNER or
//     MANAGER there);
//   - its organisation is in MemberOrganisationIDs (the caller is AGENT or
//     STAFF there) and it is assigned to the caller, or it is an unassigned
//     property VIEWING. A consultation addressed to the caller's own agent
//     profile is assigned to the caller by routing;
//   - DirectOwner is set and it was routed to the caller as the direct owner
//     (landlord or property seller) of the listing.
type Scope struct {
	UserID                 uuid.UUID
	ManagedOrganisationIDs []uuid.UUID
	MemberOrganisationIDs  []uuid.UUID
	DirectOwner            bool
}

// Empty reports whether the scope can match nothing.
func (s Scope) Empty() bool {
	return len(s.ManagedOrganisationIDs) == 0 && len(s.MemberOrganisationIDs) == 0 && !s.DirectOwner
}

func containsID(ids []uuid.UUID, id uuid.UUID) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// Sees reports whether the appointment is visible under the scope. The
// Postgres store expresses the same rule in SQL.
func (s Scope) Sees(a Appointment) bool {
	mine := a.AssignedUserID != nil && *a.AssignedUserID == s.UserID
	if a.OrganisationID != nil {
		org := *a.OrganisationID
		if containsID(s.ManagedOrganisationIDs, org) {
			return true
		}
		if containsID(s.MemberOrganisationIDs, org) && (mine || (a.AssignedUserID == nil && a.Type == TypeViewing)) {
			return true
		}
	}
	return s.DirectOwner && a.RecipientType == RecipientSeller && mine
}

// Manages reports whether the scope's caller is OWNER or MANAGER of the
// appointment's organisation.
func (s Scope) Manages(a Appointment) bool {
	return a.OrganisationID != nil && containsID(s.ManagedOrganisationIDs, *a.OrganisationID)
}

// ListFilter selects one page of appointments. Exactly one of CustomerUserID
// and Scope is set: the first lists a customer's own appointments, the second
// what a professional may see.
type ListFilter struct {
	CustomerUserID *uuid.UUID
	Scope          *Scope

	// When and Now: upcoming (still open and not yet over), past (the rest) or all.
	When string
	Now  time.Time
	// From (inclusive) and To (exclusive) bound start_utc.
	From      *time.Time
	To        *time.Time
	Statuses  []string
	Type      string
	ListingID *uuid.UUID
	// Ascending orders by start time, soonest first; otherwise latest first.
	Ascending bool
	Page      int
	PageSize  int
}

// upcoming reports whether the appointment is still ahead: open and not over.
func upcoming(a Appointment, now time.Time) bool { return a.Open() && a.EndUTC().After(now) }

// matches reports whether the appointment passes the filter (memory store;
// the Postgres store expresses the same in SQL).
func (f ListFilter) matches(a Appointment) bool {
	switch {
	case f.CustomerUserID != nil:
		if a.CustomerUserID == nil || *a.CustomerUserID != *f.CustomerUserID {
			return false
		}
	case f.Scope != nil:
		if !f.Scope.Sees(a) {
			return false
		}
	default:
		return false
	}
	switch f.When {
	case WhenUpcoming:
		if !upcoming(a, f.Now) {
			return false
		}
	case WhenPast:
		if upcoming(a, f.Now) {
			return false
		}
	}
	if f.From != nil && a.StartUTC.Before(*f.From) {
		return false
	}
	if f.To != nil && !a.StartUTC.Before(*f.To) {
		return false
	}
	if len(f.Statuses) > 0 {
		found := false
		for _, s := range f.Statuses {
			found = found || s == a.Status
		}
		if !found {
			return false
		}
	}
	if f.Type != "" && a.Type != f.Type {
		return false
	}
	if f.ListingID != nil && (a.ListingID == nil || *a.ListingID != *f.ListingID) {
		return false
	}
	return true
}

// Counts are the numbers behind the app's appointment badges.
type Counts struct {
	Requested           int `json:"requested"`
	RescheduleRequested int `json:"reschedule_requested"`
	UpcomingConfirmed   int `json:"upcoming_confirmed"`
}

// Mutation is what an update writes: the new state, the timeline entry that
// records it and the outbox event that announces it.
type Mutation struct {
	Next  Appointment
	Entry TimelineEntry
	Event string
}

// ListingCard is the public name of a listing and of who advertises it.
type ListingCard struct {
	Slug      string
	Title     string
	Reference string
	// With is the listing's agent profile, else its agency profile; all empty
	// for a private seller.
	WithKind string
	WithSlug string
	WithName string
}
