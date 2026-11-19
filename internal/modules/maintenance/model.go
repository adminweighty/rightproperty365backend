// Package maintenance is property management, part (c) (API 0.36.2,
// BR-019, FR-047): maintenance requests in a book (an organisation's, or an
// approved direct owner's), on a property and optionally one of its units
// and a tenancy.
//
// Rules:
//
//   - The book and its access rules are the tenancies module's: a book that
//     is not the caller's is 404 BOOK_NOT_FOUND before any validation;
//     STAFF read; OWNER, MANAGER, AGENT and the direct owner write.
//   - A request is raised by a manager, or by a tenant through their own
//     tenancy (/v1/me/tenancies/{id}/maintenance). A tenant sees and
//     comments on the requests of their own tenancy only; anything else is
//     404. A tenant never changes a status, assigns or records a cost.
//   - Status: NEW -> TRIAGED -> ASSIGNED -> IN_PROGRESS -> RESOLVED ->
//     CLOSED, or CANCELLED (Transitions). ASSIGNED is reached by assigning
//     a directory service provider, which sends that provider one business
//     enquiry through the business-enquiries service (idempotent), quoting
//     the reference and never the tenant's details.
//   - A cost writes exactly one EXPENSE ledger entry (category maintenance,
//     linked to the property) in the same transaction as the cost.
//   - Photos are images only (sniffed), re-encoded as JPEG without
//     metadata, kept in the private object storage and served by the API.
//   - Every change goes on the timeline; creating a request and changing
//     its status write an outbox event (ids, reference and statuses only)
//     from which the notifications module tells the managers and the
//     tenants, in-app and by email.
package maintenance

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Statuses.
const (
	StatusNew        = "NEW"
	StatusTriaged    = "TRIAGED"
	StatusAssigned   = "ASSIGNED"
	StatusInProgress = "IN_PROGRESS"
	StatusResolved   = "RESOLVED"
	StatusClosed     = "CLOSED"
	StatusCancelled  = "CANCELLED"
)

// Statuses returns every status in lifecycle order.
func Statuses() []string {
	return []string{StatusNew, StatusTriaged, StatusAssigned, StatusInProgress, StatusResolved, StatusClosed, StatusCancelled}
}

var statusLabels = map[string]string{StatusNew: "New", StatusTriaged: "Triaged", StatusAssigned: "Assigned", StatusInProgress: "In progress",
	StatusResolved: "Resolved", StatusClosed: "Closed", StatusCancelled: "Cancelled"}

// StatusLabel is the wording of a status.
func StatusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

// Open reports whether a request in that status is still open (not CLOSED or CANCELLED).
func Open(status string) bool { return status != StatusClosed && status != StatusCancelled }

// transitions are the moves of POST .../status. ASSIGNED is reached only
// by POST .../assign (Assignable).
var transitions = map[string][]string{
	StatusNew:        {StatusTriaged, StatusCancelled},
	StatusTriaged:    {StatusInProgress, StatusCancelled},
	StatusAssigned:   {StatusInProgress, StatusCancelled},
	StatusInProgress: {StatusResolved, StatusCancelled},
	StatusResolved:   {StatusClosed, StatusInProgress},
	StatusClosed:     {},
	StatusCancelled:  {},
}

// Transitions returns the statuses POST .../status may move a request to.
func Transitions(from string) []string { return slices.Clone(transitions[from]) }

// CanMove reports whether POST .../status may move a request from one status to another.
func CanMove(from, to string) bool { return slices.Contains(transitions[from], to) }

// Assignable reports whether a request in that status may be assigned to a provider.
func Assignable(status string) bool {
	return status == StatusNew || status == StatusTriaged || status == StatusAssigned
}

// Who raised or did something.
const (
	ActorManager = "MANAGER"
	ActorTenant  = "TENANT"
	ActorSystem  = "SYSTEM"
)

// Timeline item types.
const (
	EventCreated       = "CREATED"
	EventStatusChanged = "STATUS_CHANGED"
	EventComment       = "COMMENT"
	EventAssigned      = "ASSIGNED"
	EventCostAdded     = "COST_ADDED"
	EventPhotoAdded    = "PHOTO_ADDED"
	EventUpdated       = "UPDATED"
)

// Outbox events (the notifications module turns them into in-app
// notifications and emails).
const (
	OutboxRequestCreated     = "MAINTENANCE_REQUEST_CREATED"
	OutboxStatusChanged      = "MAINTENANCE_STATUS_CHANGED"
	AggregateMaintenance     = "MAINTENANCE_REQUEST"
	ReferencePrefix          = "RPM"
	ProviderKind             = "service-providers"
	DefaultEnquiryPrivacyVer = "agent-maintenance-1"
)

// OutboxEventTypes lists the outbox events this module writes.
func OutboxEventTypes() []string { return []string{OutboxRequestCreated, OutboxStatusChanged} }

// Limits.
const (
	MaxTitle       = 200
	MaxDescription = 5000
	MaxNote        = 500
	MaxComment     = 2000
	MaxMessage     = 1500
	MaxCostText    = 500
	MaxPhotos      = 10
	// MaxPhotoBytes is the largest upload; MaxPhotoSide and MaxPhotoPixels
	// bound what is decoded; StoredPhotoSide is the longer side kept.
	MaxPhotoBytes     = 10 << 20
	MaxPhotoSide      = 8000
	MaxPhotoPixels    = 40_000_000
	StoredPhotoSide   = 2048
	PhotoJPEGQuality  = 82
	PhotoKeyPrefix    = "maintenance"
	MaxPhotoBodyBytes = MaxPhotoBytes + 64<<10
	MaxProviders      = 25
)

// Errors.
var (
	ErrUnauthenticated   = errors.New("not signed in")
	ErrBookNotFound      = errors.New("book not found")
	ErrNotFound          = errors.New("maintenance request not found")
	ErrPhotoNotFound     = errors.New("maintenance photo not found")
	ErrForbidden         = errors.New("not allowed")
	ErrClosed            = errors.New("maintenance request is closed")
	ErrPhotoLimit        = errors.New("maintenance photo limit reached")
	ErrConflict          = errors.New("maintenance request changed meanwhile")
	ErrPhotoTooLarge     = errors.New("photo too large")
	ErrPhotoUnsupported  = errors.New("a photo must be a JPEG, PNG or WebP image")
	ErrPhotoDimensions   = errors.New("photo dimensions out of range")
	ErrNoLedger          = errors.New("maintenance: no ledger connected")
	ErrNoFiles           = errors.New("maintenance: no file storage")
	ErrTenancyNotFound   = tenancies.ErrTenancyNotFound
	ErrTenancyNotActive  = errors.New("tenancy is not active")
	ErrProviderUnusable  = errors.New("provider cannot receive enquiries")
	ErrEnquiryNotAllowed = errors.New("enquiry refused")
)

// Provider is the directory profile a request is assigned to, as it was then.
type Provider struct {
	// ProfileID is nil when the profile no longer exists.
	ProfileID *uuid.UUID
	Kind      string
	Slug      string
	Name      string
}

// Request is one maintenance request.
type Request struct {
	ID            uuid.UUID
	Reference     string
	Book          tenancies.Book
	PropertyID    uuid.UUID
	UnitID        *uuid.UUID
	TenancyID     *uuid.UUID
	Category      string
	CategoryLabel string
	Priority      string
	PriorityLabel string
	Title         string
	Description   string
	Status        string
	RaisedByKind  string
	RaisedBy      *uuid.UUID
	RaisedByName  string
	// Provider is the current assignment (nil when never assigned).
	Provider         *Provider
	EnquiryID        *uuid.UUID
	EnquiryReference string
	// Assignments counts the assignments so far (the enquiry idempotency key).
	Assignments     int
	AssignedAt      *time.Time
	AssignedBy      *uuid.UUID
	AssignedByName  string
	StatusChangedAt time.Time
	ResolvedAt      *time.Time
	ClosedAt        *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time

	// Read-only, computed by the stores.
	PhotoCount   int
	CommentCount int
}

// Event is one timeline item.
type Event struct {
	ID          uuid.UUID
	RequestID   uuid.UUID
	Type        string
	ActorKind   string
	ActorUserID *uuid.UUID
	ActorName   string
	FromStatus  string
	ToStatus    string
	Text        string
	Internal    bool
	Details     map[string]any
	At          time.Time
}

// Photo is one stored image (always image/jpeg).
type Photo struct {
	ID             uuid.UUID
	RequestID      uuid.UUID
	ObjectKey      string
	ContentType    string
	Width, Height  int
	Size           int64
	SHA256         string
	UploaderKind   string
	UploadedBy     *uuid.UUID
	UploadedByName string
	UploadedAt     time.Time
}

// Cost is one cost with its ledger entry.
type Cost struct {
	ID             uuid.UUID
	RequestID      uuid.UUID
	Amount         string
	Currency       string
	Description    string
	Method         string
	IncurredOn     string
	LedgerEntryID  uuid.UUID
	RecordedBy     *uuid.UUID
	RecordedByName string
	RecordedAt     time.Time
}

// Filter narrows a list of requests.
type Filter struct {
	Status     string
	Category   string
	Priority   string
	Q          string
	PropertyID *uuid.UUID
	TenancyID  *uuid.UUID
	// Limit 0 means no limit.
	Limit, Offset int
}

// Expect is the state a Change was decided on.
type Expect struct {
	Status      string
	Assignments int
}

// Notice is one outbox event of a Change.
type Notice struct {
	Type    string
	Payload EventPayload
}

// EventPayload is the outbox payload: ids, the reference and statuses only
// (never a title, description, comment, name or contact detail).
type EventPayload struct {
	RequestID   uuid.UUID  `json:"request_id"`
	Reference   string     `json:"reference"`
	FromStatus  string     `json:"from_status,omitempty"`
	ToStatus    string     `json:"to_status"`
	RaisedBy    string     `json:"raised_by"`
	ActorKind   string     `json:"actor_kind"`
	ActorUserID *uuid.UUID `json:"actor_user_id,omitempty"`
	OccurredAt  time.Time  `json:"occurred_at"`
}

// Change is one atomic write. Stores apply it in this order: lock the
// requests of Expect and fail with ErrConflict when one is missing or has
// another status or assignment count; apply Ledger (the ledger's own Change:
// a cost's entry) in the same transaction; insert Create; update Requests
// (every column but the reference and the book); set updated_at of Touch
// (a missing request: ErrNotFound); delete DeletePhotos; insert Photos
// (more than MaxPhotos on a request: ErrPhotoLimit); insert Costs and
// Events; insert Audit and Notices.
type Change struct {
	Expect       map[uuid.UUID]Expect
	Ledger       *ledger.Change
	Create       *Request
	Requests     []Request
	Touch        map[uuid.UUID]time.Time
	DeletePhotos []uuid.UUID
	Photos       []Photo
	Costs        []Cost
	Events       []Event
	Audit        []orgaudit.Event
	Notices      []Notice
}

// Store persists maintenance requests.
type Store interface {
	// NextReference allocates a reference (RPM-<year>-<6 digits>).
	NextReference(ctx context.Context, year int) (string, error)
	// Request returns one request, or ErrNotFound.
	Request(ctx context.Context, id uuid.UUID) (Request, error)
	// Requests returns one page of a book's requests, newest first, the
	// total, and the counts per status (the filter's Status ignored).
	Requests(ctx context.Context, book tenancies.Book, f Filter) ([]Request, int, map[string]int, error)
	// RequestsOfTenancy returns the requests linked to a tenancy, newest first.
	RequestsOfTenancy(ctx context.Context, tenancyID uuid.UUID) ([]Request, error)
	// Events returns a request's timeline, oldest first.
	Events(ctx context.Context, requestID uuid.UUID) ([]Event, error)
	// Photos returns a request's photos, oldest first.
	Photos(ctx context.Context, requestID uuid.UUID) ([]Photo, error)
	// Costs returns the costs of the requests, newest first.
	Costs(ctx context.Context, requestIDs []uuid.UUID) ([]Cost, error)
	// Apply writes a Change atomically.
	Apply(ctx context.Context, ch Change) error
}

func newID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}
