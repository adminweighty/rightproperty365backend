// Package propertyrequests implements the mandatory MVP enquiry flow: every
// customer enquiry enters through POST /v1/property-requests and is persisted
// atomically together with its activity, timeline and outbox records.
package propertyrequests

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Request types accepted by the API.
const (
	TypeEnquiry             = "ENQUIRY"
	TypePropertyInformation = "PROPERTY_INFORMATION"
	TypeViewingRequest      = "VIEWING_REQUEST"
	TypeConsultationRequest = "CONSULTATION_REQUEST"
	// TypeStayRequest asks to stay at a short-stay listing from check_in to
	// check_out with a number of guests. It is still an enquiry: it does not
	// block the dates and creates no appointment or booking.
	TypeStayRequest = "STAY_REQUEST"
)

// Preferred contact methods.
const (
	ContactEmail    = "EMAIL"
	ContactPhone    = "PHONE"
	ContactWhatsApp = "WHATSAPP"
)

// Request statuses. Customers only ever see StatusReceived; a request that
// could not be routed is stored as StatusRoutingException for operations to
// resolve. Nothing in this module may claim a request was "delivered".
const (
	StatusReceived         = "RECEIVED"
	StatusRoutingException = "ROUTING_EXCEPTION"
)

// Recipient types, in routing priority order.
const (
	RecipientAgent        = "AGENT"
	RecipientOrganisation = "ORGANISATION"
	RecipientSeller       = "SELLER"
)

// ListingStatusPublished is the only listing status that accepts requests.
const ListingStatusPublished = "PUBLISHED"

// Domain errors mapped to HTTP statuses by the handler.
var (
	ErrListingNotFound     = errors.New("listing not found")
	ErrListingUnavailable  = errors.New("listing is not accepting requests")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different content")
	// ErrNotFound is returned by Store.FindByIdempotencyKey when no request exists.
	ErrNotFound = errors.New("property request not found")
	// ErrReferenceCollision is returned by Store.Create when the generated
	// human-friendly reference already exists; the service retries with a new one.
	ErrReferenceCollision = errors.New("reference already in use")
)

// StayDetails are the dates and guests of a request to stay.
type StayDetails struct {
	CheckIn  string `json:"check_in"`  // YYYY-MM-DD: the first night
	CheckOut string `json:"check_out"` // YYYY-MM-DD: the morning of leaving
	Nights   int    `json:"nights"`
	Guests   int    `json:"guests"`
}

// StayChecker decides whether a stay can be asked for at a listing: the
// listing is a short stay, the dates are inside its booking window, the stay
// is long enough, the guests fit and every night is available right now. It
// returns the number of nights, a *ValidationError for a broken rule, or a
// *DatesUnavailableError. It never changes availability.
type StayChecker interface {
	CheckStay(ctx context.Context, listingID uuid.UUID, checkIn, checkOut string, guests int) (nights int, err error)
}

// DatesUnavailableError: at least one night of the stay is taken. Date is
// the first such night (YYYY-MM-DD).
type DatesUnavailableError struct{ Date string }

func (e *DatesUnavailableError) Error() string { return "dates unavailable from " + e.Date }

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string { return "validation failed" }

// Input is the JSON body of POST /v1/property-requests.
type Input struct {
	ListingID              string        `json:"listing_id"`
	RequestType            string        `json:"request_type"`
	Message                string        `json:"message"`
	PreferredContactMethod string        `json:"preferred_contact_method"`
	PreferredSlotUTC       *string       `json:"preferred_slot_utc"`
	IdempotencyKey         string        `json:"idempotency_key"`
	GuestContact           *GuestContact `json:"guest_contact"`
	Consent                *Consent      `json:"consent"`
	// CheckIn, CheckOut (YYYY-MM-DD, in the listing's time zone) and Guests
	// are required for STAY_REQUEST and not allowed for any other type.
	CheckIn  *string `json:"check_in"`
	CheckOut *string `json:"check_out"`
	Guests   *int    `json:"guests"`
	// ServiceID is a consultation service of the professional, for a
	// CONSULTATION_REQUEST with a preferred slot (API 0.51.0). The
	// appointment planner checks it.
	ServiceID *string `json:"service_id"`

	// Customer is the authenticated caller, set by the handler from the
	// session and never read from the body. Nil for a guest request.
	Customer *Customer `json:"-"`
}

// Customer is the signed-in user submitting a request. Name and email come
// from the account, not from the request body.
type Customer struct {
	UserID uuid.UUID
	Name   string
	Email  string
}

// GuestContact identifies an unauthenticated customer. For an authenticated
// customer the whole object is optional and only Phone is read.
type GuestContact struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone"`
}

// Consent records acceptance of a specific privacy notice version.
type Consent struct {
	PrivacyVersion string `json:"privacy_version"`
	Accepted       bool   `json:"accepted"`
}

// Result is the response body for both 201 (created) and 200 (replay).
type Result struct {
	RequestID uuid.UUID `json:"request_id"`
	Reference string    `json:"reference"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	// Appointment is the appointment created together with a viewing or
	// consultation request that carried a preferred slot; null otherwise.
	Appointment *AppointmentRef `json:"appointment"`
	// Replayed is true when an existing request was returned for a repeated
	// idempotency key; the handler maps it to 200 instead of 201.
	Replayed bool `json:"-"`
}

// AppointmentRef names the appointment that belongs to a request.
type AppointmentRef struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
}

// LinkedAppointment is the appointment a request creates in the same
// transaction. Data is the appointments module's own record; this package
// only carries it to the store.
type LinkedAppointment struct {
	ID        uuid.UUID
	Reference string
	Data      any
}

// AppointmentPlanner decides whether a request gets an appointment (a viewing
// or consultation request with a preferred slot) and builds it. It returns
// nil when the request gets none.
type AppointmentPlanner interface {
	PlanAppointment(ctx context.Context, req NewRequest) (*LinkedAppointment, error)
}

// Listing is the slice of listing state needed to accept and route a request.
type Listing struct {
	ID        uuid.UUID
	MarketID  uuid.UUID
	Status    string
	ExpiresAt *time.Time

	AgentUserID *uuid.UUID
	// AgentEligible: the assigned agent is an active user (and an active
	// member of the listing's organisation when there is one).
	AgentEligible bool

	OrganisationID *uuid.UUID
	// OrganisationEligible: the organisation is active and has an enquiry inbox.
	OrganisationEligible bool

	SellerUserID *uuid.UUID
	// SellerEligible: the direct seller is an active user.
	SellerEligible bool
}

// Recipient is who a request is routed to.
type Recipient struct {
	Type           string
	UserID         *uuid.UUID
	OrganisationID *uuid.UUID
}

// NewRequest is everything a Store needs to persist a request atomically.
type NewRequest struct {
	ID                     uuid.UUID
	Reference              string
	MarketID               uuid.UUID
	ListingID              uuid.UUID
	RequestType            string
	Status                 string // internal status: RECEIVED or ROUTING_EXCEPTION
	Message                string
	PreferredContactMethod string
	PreferredSlot          *time.Time // UTC
	IdempotencyKey         string
	RequestHash            string // hex SHA-256 of the canonicalised payload
	// CustomerUserID is the authenticated customer, nil for a guest.
	CustomerUserID *uuid.UUID
	// Contact details: supplied by a guest, or a snapshot of the account's
	// name and email (plus an optional phone) for an authenticated customer.
	GuestName         string
	GuestEmail        string
	GuestPhone        string // "" when not provided
	PrivacyVersion    string
	ConsentAcceptedAt time.Time  // UTC
	Recipient         *Recipient // nil when routing failed
	CreatedAt         time.Time  // UTC, microsecond precision
	// Stay is set for a STAY_REQUEST only.
	Stay *StayDetails
	// ServiceID is the consultation service asked for ("" for none).
	ServiceID string
	// Appointment is written together with the request, or not at all. Nil
	// when the request gets no appointment.
	Appointment *LinkedAppointment
}

// MaxInboxItems caps Service.Inbox, the unpaged form of the inbox.
const MaxInboxItems = 100

// InboxScope says which requests a caller may see. It is built on the server
// from the caller's identity and memberships and is the ONLY thing that
// selects rows. A request is visible when ANY of these holds (the union):
//   - its recipient organisation is in ManagedOrganisationIDs (the caller is
//     OWNER or MANAGER there);
//   - its recipient organisation is in MemberOrganisationIDs (the caller is
//     AGENT or STAFF there) and it is assigned to the caller personally or to
//     the organisation inbox (no assigned user);
//   - it was routed to the caller as the DIRECT SELLER of the listing
//     (recipient type SELLER and recipient user = UserID).
//
// Routing exceptions (no recipient) are never matched, and neither are other
// people's direct-seller requests. Account roles play no part here.
type InboxScope struct {
	UserID                 uuid.UUID
	ManagedOrganisationIDs []uuid.UUID
	MemberOrganisationIDs  []uuid.UUID
}

// InboxItem is one request as shown to the receiving organisation. Status is
// the internal status.
type InboxItem struct {
	RequestID uuid.UUID `json:"request_id"`
	Reference string    `json:"reference"`
	ListingID uuid.UUID `json:"listing_id"`
	// Listing is the public summary of the listing the request is about, or
	// null when the listing has no public page.
	Listing     *InboxListing `json:"listing"`
	RequestType string        `json:"request_type"`
	Status      string        `json:"status"`
	// Stage is the lead stage and StageLabel its wording; StageChangedAt is
	// null until somebody moves the request.
	Stage                  string       `json:"stage"`
	StageLabel             string       `json:"stage_label"`
	StageChangedAt         *time.Time   `json:"stage_changed_at"`
	Message                string       `json:"message"`
	PreferredContactMethod string       `json:"preferred_contact_method"`
	Contact                InboxContact `json:"contact"`
	CreatedAt              time.Time    `json:"created_at"`
	// Stay is the dates, nights and guests of a STAY_REQUEST; null for every other type.
	Stay *StayDetails `json:"stay"`
	// AssignedTo is the member of the receiving organisation the request is
	// assigned to; null in the organisation inbox (and for a direct seller's
	// own request).
	AssignedTo *AssigneeView `json:"assigned_to"`
	// The conversation with the customer (API 0.46.0): its size, its last
	// message, and AwaitingReply when that last message is the customer's.
	MessageCount  int        `json:"message_count"`
	LastMessageAt *time.Time `json:"last_message_at"`
	AwaitingReply bool       `json:"awaiting_reply"`
}

// AssigneeView names the person a request is assigned to.
type AssigneeView struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"name"`
}

// ItemOf builds the inbox view of a stored request (without its listing).
func ItemOf(r StoredRequest) InboxItem {
	item := InboxItem{
		RequestID: r.ID, Reference: r.Reference, ListingID: r.ListingID,
		RequestType: r.RequestType, Status: r.Status,
		Stage: r.Stage, StageLabel: StageLabel(r.Stage), StageChangedAt: r.StageChangedAt,
		Message: r.Message, PreferredContactMethod: r.PreferredContactMethod,
		Contact:   InboxContact{Name: r.ContactName, Email: r.ContactEmail},
		CreatedAt: r.CreatedAt, Stay: r.Stay,
		MessageCount: r.MessageCount, LastMessageAt: r.LastMessageAt, AwaitingReply: r.AwaitingReply(),
	}
	if r.ContactPhone != "" {
		phone := r.ContactPhone
		item.Contact.Phone = &phone
	}
	return item
}

// InboxListing names the listing a request is about.
type InboxListing struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Reference string `json:"reference"`
}

// ListingNames supplies the public summary of listings by ID.
type ListingNames interface {
	ListingNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]InboxListing, error)
}

// InboxContact is how to reach the customer. Phone is null when not given.
type InboxContact struct {
	Name  string  `json:"name"`
	Email string  `json:"email"`
	Phone *string `json:"phone"`
}

// Record is the persisted identity of a request, used for idempotent replay.
type Record struct {
	ID          uuid.UUID
	Reference   string
	Status      string
	RequestHash string
	CreatedAt   time.Time
	// Appointment is the appointment created with the request, if any.
	Appointment *AppointmentRef
}
