// Package businessenquiries implements enquiries to directory profiles
// (agencies, agents, developers, property managers, hosts and service
// providers).
//
// Profiles never expose a phone number, an email address or a street address:
// an enquiry sent through POST /v1/business-enquiries is the ONLY way to
// contact one. The customer's contact details are stored with the enquiry and
// returned solely by the recipient inbox to the authorised recipient — never
// by a public endpoint, never in the outbox payload and never in logs.
//
// Like a property request, an enquiry is persisted atomically together with
// its timeline, customer-activity and outbox records, and the API only ever
// says RECEIVED. Nothing here may claim an enquiry was "delivered".
package businessenquiries

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Directory kinds, exactly as they appear in profile URLs and in the API.
const (
	KindAgencies         = "agencies"
	KindAgents           = "agents"
	KindDevelopers       = "developers"
	KindPropertyManagers = "property-managers"
	KindSolicitors       = "solicitors"
	KindHosts            = "hosts"
	KindServiceProviders = "service-providers"
)

// Kinds returns every directory kind.
func Kinds() []string {
	return []string{KindAgencies, KindAgents, KindDevelopers, KindPropertyManagers, KindHosts, KindServiceProviders}
}

// Preferred contact methods.
const (
	ContactEmail    = "EMAIL"
	ContactPhone    = "PHONE"
	ContactWhatsApp = "WHATSAPP"
)

// Enquiry statuses. Customers only ever see StatusReceived; an enquiry nobody
// can receive is stored as StatusRoutingException for operations to resolve.
const (
	StatusReceived         = "RECEIVED"
	StatusRoutingException = "ROUTING_EXCEPTION"
)

// Profile statuses. Only an ACTIVE profile accepts enquiries.
const (
	ProfileActive              = "ACTIVE"
	ProfilePendingVerification = "PENDING_VERIFICATION"
	ProfileSuspended           = "SUSPENDED"
)

// Domain errors mapped to HTTP statuses by the handler.
var (
	ErrRecipientNotFound    = errors.New("recipient profile not found")
	ErrRecipientUnavailable = errors.New("recipient profile is not accepting enquiries")
	ErrIdempotencyConflict  = errors.New("idempotency key reused with different content")
	// ErrNotFound is returned by Store.FindByIdempotencyKey when no enquiry exists.
	ErrNotFound = errors.New("business enquiry not found")
	// ErrReferenceCollision is returned by Store.Create when the generated
	// reference already exists; the service retries with a new one.
	ErrReferenceCollision = errors.New("reference already in use")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string { return "validation failed" }

// Input is the JSON body of POST /v1/business-enquiries.
type Input struct {
	Recipient              *RecipientRef `json:"recipient"`
	Service                *string       `json:"service"`
	Message                string        `json:"message"`
	PreferredContactMethod string        `json:"preferred_contact_method"`
	IdempotencyKey         string        `json:"idempotency_key"`
	GuestContact           *GuestContact `json:"guest_contact"`
	Consent                *Consent      `json:"consent"`

	// Customer is the authenticated caller, set by the handler from the
	// session and never read from the body. Nil for a guest.
	Customer *Customer `json:"-"`
}

// RecipientRef names a directory profile by its public kind and slug.
type RecipientRef struct {
	Kind string `json:"kind"`
	Slug string `json:"slug"`
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

// Customer is the signed-in user sending an enquiry. Name and email come from
// the account, not from the request body.
type Customer struct {
	UserID uuid.UUID
	Name   string
	Email  string
}

// Result is the response body for both 201 (created) and 200 (replay).
type Result struct {
	EnquiryID uuid.UUID `json:"enquiry_id"`
	Reference string    `json:"reference"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	// Replayed is true when an existing enquiry was returned for a repeated
	// idempotency key; the handler maps it to 200 instead of 201.
	Replayed bool `json:"-"`
}

// Profile is the slice of a directory profile needed to accept and route an
// enquiry. It deliberately holds no contact details.
type Profile struct {
	ID       uuid.UUID
	Kind     string
	Slug     string
	Name     string
	MarketID uuid.UUID
	Status   string

	// OrganisationID owns the profile. For kind "agents" it is the agent's agency.
	OrganisationID uuid.UUID
	// OrganisationEligible: the organisation is active and has at least one
	// active OWNER or MANAGER to read its inbox.
	OrganisationEligible bool

	// AgentUserID is set for kind "agents" only: the person the profile is about.
	AgentUserID *uuid.UUID
	// AgentEligible: that person is an active user and an active member of
	// the (active) organisation.
	AgentEligible bool
}

// Recipient is who an enquiry is routed to: always an organisation, and for
// an agent profile also the named agent.
type Recipient struct {
	OrganisationID uuid.UUID
	UserID         *uuid.UUID
}

// NewEnquiry is everything a Store needs to persist an enquiry atomically.
type NewEnquiry struct {
	ID                     uuid.UUID
	Reference              string
	MarketID               uuid.UUID
	ProfileID              uuid.UUID
	ProfileKind            string
	ProfileSlug            string
	ProfileName            string
	Status                 string // internal status: RECEIVED or ROUTING_EXCEPTION
	Service                string // "" when the customer picked none
	Message                string
	PreferredContactMethod string
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
}

// Record is the persisted identity of an enquiry, used for idempotent replay.
type Record struct {
	ID          uuid.UUID
	Reference   string
	Status      string
	RequestHash string
	CreatedAt   time.Time
}

// MaxInboxItems caps GET /v1/inbox/business-enquiries until it is paginated.
const MaxInboxItems = 100

// InboxScope says which enquiries a member may see. It is built on the server
// from the caller's memberships and is the ONLY thing that selects rows:
//   - every enquiry routed to an organisation in ManagedOrganisationIDs (the
//     caller is OWNER or MANAGER there), including those addressed to the
//     profiles of that organisation's agents;
//   - enquiries routed to an organisation in MemberOrganisationIDs (the caller
//     is AGENT or STAFF there) that are addressed to the caller's OWN agent
//     profile.
//
// Routing exceptions have no recipient and are never matched.
type InboxScope struct {
	UserID                 uuid.UUID
	ManagedOrganisationIDs []uuid.UUID
	MemberOrganisationIDs  []uuid.UUID
	Limit                  int
}

// InboxItem is one enquiry as shown to its authorised recipient. This is the
// only place the customer's contact details leave the store.
type InboxItem struct {
	EnquiryID              uuid.UUID      `json:"enquiry_id"`
	Reference              string         `json:"reference"`
	Recipient              InboxRecipient `json:"recipient"`
	Service                *string        `json:"service"`
	Message                string         `json:"message"`
	PreferredContactMethod string         `json:"preferred_contact_method"`
	Contact                InboxContact   `json:"contact"`
	Status                 string         `json:"status"`
	CreatedAt              time.Time      `json:"created_at"`
}

// InboxRecipient is the profile the enquiry was addressed to.
type InboxRecipient struct {
	Kind string `json:"kind"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// InboxContact is how to reach the customer. Phone is null when not given.
type InboxContact struct {
	Name  string  `json:"name"`
	Email string  `json:"email"`
	Phone *string `json:"phone"`
}
