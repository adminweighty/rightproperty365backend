// Package propertyrequests implements the mandatory MVP enquiry flow: every
// customer enquiry enters through POST /v1/property-requests and is persisted
// atomically together with its activity, timeline and outbox records.
package propertyrequests

import (
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
}

// GuestContact identifies an unauthenticated customer.
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
	// Replayed is true when an existing request was returned for a repeated
	// idempotency key; the handler maps it to 200 instead of 201.
	Replayed bool `json:"-"`
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
	GuestName              string
	GuestEmail             string
	GuestPhone             string // "" when not provided
	PrivacyVersion         string
	ConsentAcceptedAt      time.Time  // UTC
	Recipient              *Recipient // nil when routing failed
	CreatedAt              time.Time  // UTC, microsecond precision
}

// Record is the persisted identity of a request, used for idempotent replay.
type Record struct {
	ID          uuid.UUID
	Reference   string
	Status      string
	RequestHash string
	CreatedAt   time.Time
}
