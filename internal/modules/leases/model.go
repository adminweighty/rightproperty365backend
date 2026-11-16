// Package leases is the lease library and lease drafting (BRD BR-011, FSD
// FR-030, FR-031); signing is the agreements module (FR-032).
//
//   - Templates (FR-030): a template is per country (market): ordered
//     sections whose text holds {{variable}} placeholders from a fixed set
//     (variables.go), and optional clauses an advertiser may switch on. A
//     template is DRAFT -> APPROVED ("published") -> RETIRED. Editing an
//     APPROVED template creates the next DRAFT version of the same family;
//     approving a version retires the family's earlier approved version.
//     Agencies and landlords own their templates (OrganisationID): their
//     OWNER and MANAGER design and publish them, with no lawyer or platform
//     review. The platform's content administrators keep a library of
//     SAMPLE templates (no organisation; a legal review may be recorded but
//     is optional) that anyone may use or copy. A lease may use an APPROVED
//     template of its country that is a sample or its organisation's.
//   - Leases (FR-031): whoever may manage listings (auth.ManagesListings)
//     drafts a lease from an approved template: parties, property, dates,
//     rent, deposit and clauses. Each save renders the draft PDF (with a
//     diagonal DRAFT watermark) into private object storage.
//   - Sending: the lease is frozen, its signing copy (no watermark) is
//     rendered with room for every party's initials (each page) and
//     signature block (the end), and an agreement is created from it with
//     those fields placed (internal/modules/agreements). The lease's status
//     follows its agreement.
//
// Nothing in this module may describe a lease as legally executed.
package leases

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Template statuses.
const (
	TemplateDraft    = "DRAFT"
	TemplateApproved = "APPROVED"
	TemplateRetired  = "RETIRED"
)

// Lease kinds.
const (
	KindResidential = "RESIDENTIAL"
	KindCommercial  = "COMMERCIAL"
)

// Lease statuses. After DRAFT the status follows the lease's agreement.
const (
	StatusDraft          = "DRAFT"
	StatusSentForSigning = "SENT_FOR_SIGNING"
	StatusSigned         = "SIGNED"
	StatusDeclined       = "DECLINED"
	StatusCancelled      = "CANCELLED"
	StatusExpired        = "EXPIRED"
)

// Party roles.
const (
	RoleLandlord = "LANDLORD"
	RoleTenant   = "TENANT"
)

// Rent periods.
const (
	PeriodMonthly  = "MONTHLY"
	PeriodWeekly   = "WEEKLY"
	PeriodAnnually = "ANNUALLY"
)

// Document kinds.
const (
	// DocumentDraft is the latest draft PDF (watermarked). Only the latest
	// is kept: each save replaces the object and the row.
	DocumentDraft = "DRAFT"
	// DocumentSigning is the frozen copy sent for signing. Kept forever.
	DocumentSigning = "SIGNING"
	// DocumentFinal is the completed agreement's signed PDF (the same object
	// as the agreement's final document).
	DocumentFinal = "FINAL"
)

// Lease audit actions (lease_events).
const (
	EventCreated      = "CREATED"
	EventEdited       = "EDITED"
	EventPDFGenerated = "PDF_GENERATED"
	EventSent         = "SENT"
	EventSigned       = "SIGNED"
	EventDeclined     = "DECLINED"
	EventCancelled    = "CANCELLED"
	EventExpired      = "EXPIRED"
)

// Template audit actions (lease_template_events).
const (
	TemplateEventCreated    = "CREATED"
	TemplateEventEdited     = "EDITED"
	TemplateEventNewVersion = "NEW_VERSION"
	TemplateEventApproved   = "APPROVED"
	TemplateEventRetired    = "RETIRED"
)

// Actor types of a lease event.
const (
	ActorUser   = "USER"
	ActorSystem = "SYSTEM"
)

// Limits.
const (
	// MaxParties per side: every party's initials box fits on one footer row.
	MaxParties          = 5
	MaxSections         = 60
	MaxClauses          = 30
	MaxSectionText      = 10000
	MaxHeading          = 200
	MaxTemplateName     = 120
	MaxNameRunes        = 120
	MaxAddressRunes     = 300
	MaxSpecialRunes     = 4000
	MaxReasonRunes      = 1000
	MaxNoticeDays       = 365
	DefaultPageSize     = 20
	MaxPageSize         = 100
	ObjectKeyPrefix     = "leases"
	ReferencePrefix     = "RPL"
	maxReferenceAttempt = 5
)

// Error codes of this module's endpoints.
const (
	CodeTemplateNotFound     = "LEASE_TEMPLATE_NOT_FOUND"
	CodeTemplateNotEditable  = "LEASE_TEMPLATE_NOT_EDITABLE"
	CodeTemplateDraftExists  = "LEASE_TEMPLATE_DRAFT_EXISTS"
	CodeTemplateState        = "LEASE_TEMPLATE_STATE_CONFLICT"
	CodeTemplateNotAvailable = "LEASE_TEMPLATE_NOT_AVAILABLE"
	CodeLeaseNotFound        = "LEASE_NOT_FOUND"
	CodeLeaseNotEditable     = "LEASE_NOT_EDITABLE"
	CodeLeaseState           = "LEASE_STATE_CONFLICT"
	CodeListingNotFound      = "LISTING_NOT_FOUND"
	CodeMarketMismatch       = "LEASE_MARKET_MISMATCH"
	CodeDocumentUnavailable  = "LEASE_DOCUMENT_UNAVAILABLE"
	CodeOrganisationNotFound = "ORGANISATION_NOT_FOUND"
)

// Template sources (TemplateView.Source).
const (
	SourceSample       = "SAMPLE"
	SourceOrganisation = "ORGANISATION"
)

// Errors, mapped to HTTP by the handler.
var (
	ErrForbidden           = errors.New("not allowed")
	ErrTemplateNotFound    = errors.New("lease template not found")
	ErrTemplateNotEditable = errors.New("a retired template cannot be edited")
	ErrTemplateDraftExists = errors.New("the template family already has a draft version")
	ErrTemplateState       = errors.New("the template is not in a state that allows this")
	ErrTemplateUnavailable = errors.New("the template is not approved for this country")
	ErrLeaseNotFound       = errors.New("lease not found")
	ErrLeaseNotEditable    = errors.New("only a draft lease can be changed")
	ErrLeaseState          = errors.New("the lease is not in a state that allows this")
	ErrListingNotFound     = errors.New("listing not found or not managed by the caller")
	ErrMarketMismatch      = errors.New("the template's country is not the listing's")
	ErrDocumentUnavailable = errors.New("the document is not available")
	ErrReferenceTaken      = errors.New("lease reference already used")
	// ErrTemplateForbidden: a member of the organisation who may not manage
	// (or read) its templates.
	ErrTemplateForbidden = errors.New("not allowed to manage the organisation's templates")
	// ErrOrganisationNotFound: the caller is not a member of the organisation.
	ErrOrganisationNotFound = errors.New("organisation not found")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

// Section is one numbered part of a template: a heading and its text.
type Section struct {
	Heading string `json:"heading"`
	Text    string `json:"text"`
}

// Clause is an optional clause an advertiser may switch on for a lease.
type Clause struct {
	Code  string `json:"code"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// LegalReview is who reviewed a template before it was approved.
type LegalReview struct {
	ReviewerName string `json:"reviewer_name"`
	Organisation string `json:"organisation"`
	// ReviewedOn is a calendar date, YYYY-MM-DD.
	ReviewedOn string `json:"reviewed_on"`
	Note       string `json:"note,omitempty"`
}

// Template is one version of a lease template.
type Template struct {
	ID uuid.UUID
	// FamilyID groups the versions of one template; Version counts from 1.
	FamilyID uuid.UUID
	Version  int
	// OrganisationID owns the template; nil for a sample template.
	OrganisationID *uuid.UUID
	MarketCode     string
	Name           string
	LeaseKind      string
	Description    string
	Sections       []Section
	Clauses        []Clause
	Status         string
	LegalReview    *LegalReview
	CreatedBy      *uuid.UUID
	CreatedByName  string
	ApprovedAt     *time.Time
	ApprovedBy     *uuid.UUID
	ApprovedByName string
	RetiredAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Clause returns the optional clause with that code.
func (t Template) Clause(code string) (Clause, bool) {
	for _, c := range t.Clauses {
		if c.Code == code {
			return c, true
		}
	}
	return Clause{}, false
}

// TemplateEvent is one record of a template's audit trail.
type TemplateEvent struct {
	ID          uuid.UUID
	TemplateID  uuid.UUID
	Action      string
	ActorUserID *uuid.UUID
	ActorName   string
	Detail      map[string]any
	At          time.Time
}

// IsSample reports whether the template is a platform sample.
func (t Template) IsSample() bool { return t.OrganisationID == nil }

// TemplateFilter selects templates. When Scoped, only the samples
// (Samples) and the templates of Organisations are selected.
type TemplateFilter struct {
	MarketCode    string
	Status        string
	FamilyID      *uuid.UUID
	Scoped        bool
	Samples       bool
	Organisations []uuid.UUID
}

// matches reports whether t is selected by the filter.
func (f TemplateFilter) matches(t Template) bool {
	if (f.MarketCode != "" && t.MarketCode != f.MarketCode) || (f.Status != "" && t.Status != f.Status) ||
		(f.FamilyID != nil && t.FamilyID != *f.FamilyID) {
		return false
	}
	if !f.Scoped {
		return true
	}
	if t.OrganisationID == nil {
		return f.Samples
	}
	for _, id := range f.Organisations {
		if id == *t.OrganisationID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Leases
// ---------------------------------------------------------------------------

// Party is a landlord or tenant as the lease's author enters them.
type Party struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone,omitempty"`
}

// Input is everything the author enters: the lease's input snapshot.
type Input struct {
	TemplateID        uuid.UUID  `json:"template_id"`
	ListingID         *uuid.UUID `json:"listing_id,omitempty"`
	OrganisationID    *uuid.UUID `json:"organisation_id,omitempty"`
	PropertyAddress   string     `json:"property_address"`
	Landlords         []Party    `json:"landlords"`
	Tenants           []Party    `json:"tenants"`
	StartDate         string     `json:"start_date"`
	EndDate           string     `json:"end_date,omitempty"`
	Periodic          bool       `json:"periodic"`
	RentAmount        string     `json:"rent_amount"`
	RentCurrency      string     `json:"rent_currency"`
	RentPeriod        string     `json:"rent_period"`
	PaymentDay        int        `json:"payment_day"`
	DepositAmount     string     `json:"deposit_amount,omitempty"`
	NoticePeriodDays  int        `json:"notice_period_days"`
	OptionalClauses   []string   `json:"optional_clauses"`
	SpecialConditions string     `json:"special_conditions,omitempty"`
	// LandlordStamp: each landlord also applies their stamp (a STAMP field
	// next to their signature block).
	LandlordStamp bool `json:"landlord_stamp,omitempty"`
}

// Document is one stored PDF of a lease.
type Document struct {
	Kind            string
	TemplateID      uuid.UUID
	TemplateVersion int
	// Snapshot is the JSON of everything the PDF was rendered from.
	Snapshot    []byte
	ObjectKey   string
	SHA256      string
	Size        int64
	GeneratedAt time.Time
}

// Lease is the aggregate the store loads and saves as a whole.
type Lease struct {
	ID               uuid.UUID
	Reference        string
	Status           string
	TemplateID       uuid.UUID
	TemplateVersion  int
	TemplateName     string
	MarketCode       string
	LeaseKind        string
	OrganisationID   *uuid.UUID
	OrganisationName string
	ListingID        *uuid.UUID
	ListingReference string
	ListingTitle     string
	CreatedBy        uuid.UUID
	CreatedByName    string
	Input            Input
	// Documents by kind (DocumentDraft, DocumentSigning, DocumentFinal).
	Documents map[string]Document
	// AgreementID is the agreement the lease was sent for signing with.
	AgreementID  *uuid.UUID
	SentAt       *time.Time
	SentBy       *uuid.UUID
	CompletedAt  *time.Time
	DeclinedAt   *time.Time
	CancelledAt  *time.Time
	CancelledBy  *uuid.UUID
	CancelReason string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Event is one record of a lease's audit trail. Immutable.
type Event struct {
	ID          uuid.UUID
	LeaseID     uuid.UUID
	Action      string
	ActorType   string
	ActorUserID *uuid.UUID
	ActorName   string
	Detail      map[string]any
	At          time.Time
}

// LeaseFilter selects leases. Exactly one scope applies: All (an
// administrator), or the union of the organisations whose every lease the
// caller sees and the leases the caller created in CreatorOrganisations
// (CreatorDirect: their leases without an organisation).
type LeaseFilter struct {
	All                  bool
	Organisations        []uuid.UUID
	Creator              *uuid.UUID
	CreatorOrganisations []uuid.UUID
	CreatorDirect        bool
	Status               string
	Limit, Offset        int
}

// Store persists templates and leases.
type Store interface {
	CreateTemplate(ctx context.Context, t Template, ev TemplateEvent) error
	Template(ctx context.Context, id uuid.UUID) (Template, error)
	Templates(ctx context.Context, f TemplateFilter) ([]Template, error)
	// ChangeTemplate locks the template, calls change with it and stores
	// what change returns (the same ID) with the events.
	ChangeTemplate(ctx context.Context, id uuid.UUID, change func(t Template) (Template, []TemplateEvent, error)) (Template, error)
	// CreateTemplateVersion stores t as the next version of its family:
	// ErrTemplateDraftExists when the family already has a DRAFT. t.Version
	// is set by the store (the family's highest + 1).
	CreateTemplateVersion(ctx context.Context, t Template, ev TemplateEvent) (Template, error)
	// ApproveTemplate makes a DRAFT APPROVED and retires the family's other
	// APPROVED version, with the events. ErrTemplateState when not a DRAFT.
	// review may be nil (no legal review recorded).
	ApproveTemplate(ctx context.Context, id uuid.UUID, review *LegalReview, by *uuid.UUID, byName string, at time.Time, events func(t Template, retired []Template) []TemplateEvent) (Template, error)
	TemplateEvents(ctx context.Context, templateID uuid.UUID) ([]TemplateEvent, error)

	// NextReference returns the next lease reference of that year.
	NextReference(ctx context.Context, year int) (string, error)
	CreateLease(ctx context.Context, l Lease, events []Event) error
	Lease(ctx context.Context, id uuid.UUID) (Lease, error)
	Leases(ctx context.Context, f LeaseFilter) ([]Lease, int, error)
	// ChangeLease locks the lease, calls change with a copy and saves the
	// lease and its documents with the events. An error from change stops
	// everything. The callback must not call the store.
	ChangeLease(ctx context.Context, id uuid.UUID, change func(l *Lease) ([]Event, error)) (Lease, error)
	// DeleteLease removes a DRAFT lease (ErrLeaseNotEditable otherwise).
	DeleteLease(ctx context.Context, id uuid.UUID) (Lease, error)
	Events(ctx context.Context, leaseID uuid.UUID) ([]Event, error)
}
