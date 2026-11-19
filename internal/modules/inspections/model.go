// Package inspections is property management, part (d) (API 0.36.3,
// BR-019, FR-047): property inspections in a book (an organisation's, or an
// approved direct owner's), on a property and optionally one of its units
// and a tenancy.
//
// Rules:
//
//   - The book and its access rules are the tenancies module's: a book that
//     is not the caller's is 404 BOOK_NOT_FOUND before any validation;
//     STAFF read; OWNER, MANAGER, AGENT and the direct owner write.
//   - An inspection is a MOVE_IN, ROUTINE or MOVE_OUT, scheduled for a UTC
//     time, with an inspector (the signed-in user by default) and a
//     checklist copied from a template of the reference set
//     inspection_checklist_templates when it is created: later edits of the
//     template never change it. While open, managers edit the checklist's
//     areas and items and record findings per item (OK, ATTENTION, DAMAGED,
//     NA, a note and photos), partially.
//   - Status: SCHEDULED -> IN_PROGRESS -> COMPLETED, or CANCELLED
//     (Transitions). Completing needs a status on every item and freezes
//     the inspection: every write on a COMPLETED (or CANCELLED) inspection
//     is ErrClosed (409).
//   - Photos are images only (sniffed), re-encoded as JPEG without metadata
//     (the maintenance module's ProcessPhoto), kept in the private object
//     storage and served by the API.
//   - The report is a PDF (gofpdf, the Go fonts, as the leases).
//   - A tenant sees the COMPLETED inspections of their own tenancy only,
//     read-only; anything else is 404.
//   - Scheduling and completing an inspection with a tenancy write an
//     outbox event (ids, reference, type and status only) from which the
//     notifications module tells the tenants with a verified account.
package inspections

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Statuses.
const (
	StatusScheduled  = "SCHEDULED"
	StatusInProgress = "IN_PROGRESS"
	StatusCompleted  = "COMPLETED"
	StatusCancelled  = "CANCELLED"
)

// Statuses returns every status in lifecycle order.
func Statuses() []string {
	return []string{StatusScheduled, StatusInProgress, StatusCompleted, StatusCancelled}
}

var statusLabels = map[string]string{StatusScheduled: "Scheduled", StatusInProgress: "In progress", StatusCompleted: "Completed",
	StatusCancelled: "Cancelled"}

// StatusLabel is the wording of a status.
func StatusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

// Open reports whether an inspection in that status may still change
// (SCHEDULED or IN_PROGRESS). COMPLETED and CANCELLED are frozen.
func Open(status string) bool { return status == StatusScheduled || status == StatusInProgress }

var transitions = map[string][]string{
	StatusScheduled:  {StatusInProgress, StatusCancelled},
	StatusInProgress: {StatusCompleted, StatusCancelled},
	StatusCompleted:  {},
	StatusCancelled:  {},
}

// Transitions returns the statuses an inspection may move to.
func Transitions(from string) []string { return slices.Clone(transitions[from]) }

// CanMove reports whether an inspection may move from one status to another.
func CanMove(from, to string) bool { return slices.Contains(transitions[from], to) }

// Types.
const (
	TypeMoveIn  = "MOVE_IN"
	TypeRoutine = "ROUTINE"
	TypeMoveOut = "MOVE_OUT"
)

// Types returns every type.
func Types() []string { return []string{TypeMoveIn, TypeRoutine, TypeMoveOut} }

var typeLabels = map[string]string{TypeMoveIn: "Move-in", TypeRoutine: "Routine", TypeMoveOut: "Move-out"}

// TypeLabel is the wording of a type.
func TypeLabel(t string) string {
	if l, ok := typeLabels[t]; ok {
		return l
	}
	return t
}

// Finding statuses.
const (
	FindingOK        = "OK"
	FindingAttention = "ATTENTION"
	FindingDamaged   = "DAMAGED"
	FindingNA        = "NA"
)

// FindingStatuses returns every finding status.
func FindingStatuses() []string {
	return []string{FindingOK, FindingAttention, FindingDamaged, FindingNA}
}

var findingLabels = map[string]string{FindingOK: "OK", FindingAttention: "Needs attention", FindingDamaged: "Damaged",
	FindingNA: "Not applicable"}

// FindingLabel is the wording of a finding status.
func FindingLabel(s string) string {
	if l, ok := findingLabels[s]; ok {
		return l
	}
	return s
}

// Outbox events (the notifications module turns them into in-app
// notifications and emails for the tenants).
const (
	OutboxScheduled     = "INSPECTION_SCHEDULED"
	OutboxCompleted     = "INSPECTION_COMPLETED"
	AggregateInspection = "INSPECTION"
	ReferencePrefix     = "RPI"
)

// OutboxEventTypes lists the outbox events this module writes.
func OutboxEventTypes() []string { return []string{OutboxScheduled, OutboxCompleted} }

// Limits.
const (
	MaxAreas         = 40
	MaxItemsPerArea  = 60
	MaxItems         = 400
	MaxLabel         = 120
	MaxNote          = 1000
	MaxSummary       = 5000
	MaxSignOff       = 200
	MaxReason        = 500
	MaxPhotosPerItem = 6
	MaxPhotos        = 200
	// MaxPhotoBytes is the largest upload (the maintenance module's rules
	// and processing apply).
	MaxPhotoBytes  = 10 << 20
	PhotoKeyPrefix = "inspections"
	// MaxReportThumbnails is how many photos the PDF embeds; the rest are
	// only counted.
	MaxReportThumbnails = 60
	// MaxIncompleteIDs is how many missing item ids an INSPECTION_INCOMPLETE names.
	MaxIncompleteIDs = 50
	// ScheduleHorizon is how far ahead an inspection may be scheduled.
	ScheduleHorizon = 2 * 365 * 24 * time.Hour
)

// Errors.
var (
	ErrUnauthenticated  = errors.New("not signed in")
	ErrBookNotFound     = errors.New("book not found")
	ErrNotFound         = errors.New("inspection not found")
	ErrItemNotFound     = errors.New("inspection item not found")
	ErrPhotoNotFound    = errors.New("inspection photo not found")
	ErrForbidden        = errors.New("not allowed")
	ErrConflict         = errors.New("inspection changed meanwhile")
	ErrPhotoLimit       = errors.New("inspection photo limit reached")
	ErrPhotoTooLarge    = errors.New("photo too large")
	ErrPhotoUnsupported = errors.New("a photo must be a JPEG, PNG or WebP image")
	ErrPhotoDimensions  = errors.New("photo dimensions out of range")
	ErrNoFiles          = errors.New("inspections: no file storage")
	ErrTenancyNotFound  = tenancies.ErrTenancyNotFound
)

// Item is one checklist item with its finding.
type Item struct {
	ID    uuid.UUID `json:"id"`
	Label string    `json:"label"`
	// Status is "" until a finding is recorded.
	Status string `json:"status,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Area is one room or area of the checklist.
type Area struct {
	ID    uuid.UUID `json:"id"`
	Label string    `json:"label"`
	Items []Item    `json:"items"`
}

// Checklist is an inspection's own copy of a template, with the findings.
type Checklist struct {
	Areas []Area `json:"areas"`
}

// Inspection is one inspection.
type Inspection struct {
	ID            uuid.UUID
	Reference     string
	Book          tenancies.Book
	PropertyID    uuid.UUID
	UnitID        *uuid.UUID
	TenancyID     *uuid.UUID
	Type          string
	Status        string
	ScheduledFor  time.Time
	InspectorID   *uuid.UUID
	InspectorName string
	TemplateCode  string
	TemplateLabel string
	Checklist     Checklist
	Summary       string
	TenantSignOff string
	AgentSignOff  string
	CancelReason  string
	// Version counts the writes that replaced the row (Expect).
	Version         int
	StartedAt       *time.Time
	CompletedAt     *time.Time
	CompletedBy     *uuid.UUID
	CompletedByName string
	CancelledAt     *time.Time
	CancelledBy     *uuid.UUID
	CancelledByName string
	CreatedBy       *uuid.UUID
	CreatedByName   string
	CreatedAt       time.Time
	UpdatedAt       time.Time

	// Read-only, computed by the stores.
	PhotoCount int
}

// Photo is one stored image of an item (always image/jpeg).
type Photo struct {
	ID             uuid.UUID
	InspectionID   uuid.UUID
	ItemID         uuid.UUID
	ObjectKey      string
	ContentType    string
	Width, Height  int
	Size           int64
	SHA256         string
	UploadedBy     *uuid.UUID
	UploadedByName string
	UploadedAt     time.Time
}

// Filter narrows a list of inspections.
type Filter struct {
	Status     string
	Type       string
	PropertyID *uuid.UUID
	TenancyID  *uuid.UUID
	// From and To are dates (YYYY-MM-DD) on the UTC date of scheduled_for.
	From, To string
	// Limit 0 means no limit.
	Limit, Offset int
}

// Expect is the state a Change was decided on. AnyVersion skips the
// version check (photos added to an IN_PROGRESS inspection, which do not
// replace the row).
type Expect struct {
	Status     string
	Version    int
	AnyVersion bool
}

// Notice is one outbox event of a Change.
type Notice struct {
	Type    string
	Payload EventPayload
}

// EventPayload is the outbox payload: ids, the reference, the type and the
// status only (never a note, summary, name or contact detail).
type EventPayload struct {
	InspectionID uuid.UUID  `json:"inspection_id"`
	Reference    string     `json:"reference"`
	Type         string     `json:"type"`
	Status       string     `json:"status"`
	TenancyID    *uuid.UUID `json:"tenancy_id,omitempty"`
	Rescheduled  bool       `json:"rescheduled,omitempty"`
	ActorUserID  *uuid.UUID `json:"actor_user_id,omitempty"`
	OccurredAt   time.Time  `json:"occurred_at"`
}

// Change is one atomic write. Stores apply it in this order: lock the
// inspections of Expect and fail with ErrConflict when one is missing or
// has another status (or, unless AnyVersion, another version); insert
// Create; update Inspections (every column but the reference, the book and
// created_*); set updated_at of Touch (a missing inspection: ErrNotFound);
// delete DeletePhotos; insert Photos (more than MaxPhotosPerItem on an item
// or MaxPhotos on an inspection: ErrPhotoLimit); insert Audit and Notices.
type Change struct {
	Expect       map[uuid.UUID]Expect
	Create       *Inspection
	Inspections  []Inspection
	Touch        map[uuid.UUID]time.Time
	DeletePhotos []uuid.UUID
	Photos       []Photo
	Audit        []orgaudit.Event
	Notices      []Notice
}

// Store persists inspections.
type Store interface {
	// NextReference allocates a reference (RPI-<year>-<6 digits>).
	NextReference(ctx context.Context, year int) (string, error)
	// Inspection returns one inspection, or ErrNotFound.
	Inspection(ctx context.Context, id uuid.UUID) (Inspection, error)
	// Inspections returns one page of a book's inspections, latest
	// scheduled_for first, the total, and the counts per status (the
	// filter's Status ignored).
	Inspections(ctx context.Context, book tenancies.Book, f Filter) ([]Inspection, int, map[string]int, error)
	// InspectionsOfTenancy returns the inspections linked to a tenancy,
	// latest scheduled_for first.
	InspectionsOfTenancy(ctx context.Context, tenancyID uuid.UUID) ([]Inspection, error)
	// Photos returns an inspection's photos, oldest first.
	Photos(ctx context.Context, inspectionID uuid.UUID) ([]Photo, error)
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
