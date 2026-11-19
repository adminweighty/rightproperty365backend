// Package applications holds the screening questionnaires (forms) an
// organisation or a direct owner builds, the one questionnaire of a listing
// and its intake, and the customers' applications on them (BR-010, FR-015,
// FR-017 to FR-019, API 0.32.0).
//
// A form is versioned like a lease template: a DRAFT is edited in place,
// editing a PUBLISHED version creates the family's next DRAFT, publishing
// retires the previous PUBLISHED version, and a version with a submitted
// application never changes again (also enforced by the database, migration
// 0031). An application keeps the form version it was started on, so it
// always shows the exact questions and rules it was answered against.
//
// Applications are private: the applicant sees their own, the receiving
// organisation (OWNER, MANAGER, the listing's assigned AGENT) or the direct
// owner sees those it received, and nobody else sees any (404). Every read
// of an application's answers and every file download is recorded in the
// organisation's audit trail (FR-018).
package applications

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/saved"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Application statuses (FR-015).
const (
	StatusDraft       = "DRAFT"
	StatusSubmitted   = "SUBMITTED"
	StatusUnderReview = "UNDER_REVIEW"
	StatusMoreInfo    = "MORE_INFO"
	StatusShortlisted = "SHORTLISTED"
	StatusAccepted    = "ACCEPTED"
	StatusRejected    = "REJECTED"
	StatusWithdrawn   = "WITHDRAWN"
)

// Statuses returns every status in order.
func Statuses() []string {
	return []string{StatusDraft, StatusSubmitted, StatusUnderReview, StatusMoreInfo, StatusShortlisted, StatusAccepted, StatusRejected, StatusWithdrawn}
}

var statusLabels = map[string]string{
	StatusDraft: "Draft", StatusSubmitted: "Submitted", StatusUnderReview: "In review", StatusMoreInfo: "More information requested",
	StatusShortlisted: "Shortlisted", StatusAccepted: "Accepted", StatusRejected: "Not successful", StatusWithdrawn: "Withdrawn",
}

// StatusLabel is how a status is shown.
func StatusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

// Final reports whether a status is final (nothing moves it again).
func Final(s string) bool { return s == StatusAccepted || s == StatusRejected || s == StatusWithdrawn }

// staffTransitions are the moves the receiving side makes (MORE_INFO only
// through request-info). WITHDRAWN is the applicant's, from any non-final
// state.
var staffTransitions = map[string][]string{
	StatusSubmitted:   {StatusUnderReview, StatusMoreInfo, StatusShortlisted, StatusAccepted, StatusRejected},
	StatusUnderReview: {StatusMoreInfo, StatusShortlisted, StatusAccepted, StatusRejected},
	StatusShortlisted: {StatusAccepted, StatusRejected},
}

// StaffTransitions returns where the receiving side may move an application from a status.
func StaffTransitions(from string) []string { return append([]string{}, staffTransitions[from]...) }

// CanTransition reports whether the receiving side may move from one status to another.
func CanTransition(from, to string) bool { return slices.Contains(staffTransitions[from], to) }

// Intake statuses (FR-019). NONE is "no questionnaire attached".
const (
	IntakeOpen   = "OPEN"
	IntakePaused = "PAUSED"
	IntakeClosed = "CLOSED"
	IntakeNone   = "NONE"
)

// Why intake is not open.
const (
	ReasonPaused         = "PAUSED"
	ReasonClosed         = "CLOSED"
	ReasonClosesAtPassed = "CLOSES_AT_PASSED"
	ReasonFormExpired    = "FORM_EXPIRED"
	ReasonFormRetired    = "FORM_RETIRED"
	ReasonNone           = "NONE"
)

var reasonMessages = map[string]string{
	ReasonPaused:         "Applications for this property are paused. Please check back later.",
	ReasonClosed:         "Applications for this property are closed.",
	ReasonClosesAtPassed: "Applications for this property closed at the advertised closing time.",
	ReasonFormExpired:    "Applications for this property are closed: the application form has expired.",
	ReasonFormRetired:    "Applications for this property are closed: the application form was withdrawn.",
	ReasonNone:           "This property does not take applications.",
}

// ReasonMessage explains why intake is not open.
func ReasonMessage(reason string) string { return reasonMessages[reason] }

// Intake is a listing's questionnaire and intake status.
type Intake struct {
	ListingID uuid.UUID
	FormID    uuid.UUID
	Status    string
	ClosesAt  *time.Time
	UpdatedBy *uuid.UUID
	UpdatedAt time.Time
}

// Effective returns the intake status as applicants meet it and why it is
// not open ("" when open): OPEN turns CLOSED once closes_at has passed or
// the form expired or was retired.
func Effective(in Intake, form Form, now time.Time) (string, string) {
	switch {
	case form.Status == FormRetired || form.Status == FormDraft:
		return IntakeClosed, ReasonFormRetired
	case form.Expired(now):
		return IntakeClosed, ReasonFormExpired
	case in.Status == IntakePaused:
		return IntakePaused, ReasonPaused
	case in.Status == IntakeClosed:
		return IntakeClosed, ReasonClosed
	case in.ClosesAt != nil && !now.Before(*in.ClosesAt):
		return IntakeClosed, ReasonClosesAtPassed
	}
	return IntakeOpen, ""
}

// Application is one stored application.
type Application struct {
	ID              uuid.UUID
	Reference       string
	ListingID       uuid.UUID
	FormID          uuid.UUID
	FormVersion     int
	ApplicantUserID *uuid.UUID
	ApplicantName   string
	// Who receives it: the listing's organisation, or its direct owner.
	OrganisationID *uuid.UUID
	OwnerUserID    *uuid.UUID
	Status         string
	Answers        Answers
	// Listing is the card as last seen (shown when the listing is gone).
	Listing         saved.Snapshot
	CreatedAt       time.Time
	UpdatedAt       time.Time
	SubmittedAt     *time.Time
	LastSubmittedAt *time.Time
	DecidedAt       *time.Time
	ErasedAt        *time.Time
}

// Book returns who received it.
func (a Application) Book() Book {
	if a.OrganisationID != nil {
		return OrganisationBook(*a.OrganisationID)
	}
	if a.OwnerUserID != nil {
		return PersonalBook(*a.OwnerUserID)
	}
	return Book{}
}

// Editable reports whether the applicant may change the answers and files.
func (a Application) Editable() bool {
	return a.ErasedAt == nil && (a.Status == StatusDraft || a.Status == StatusMoreInfo)
}

// File is one private upload of an application.
type File struct {
	ID            uuid.UUID
	ApplicationID uuid.UUID
	FieldKey      string
	FileName      string
	ContentType   string
	Size          int64
	SHA256        string
	ObjectKey     string
	UploadedAt    time.Time
	// ScanStatus is the malware scan status (storage.Scan*), read from the
	// scan records when a view is built; "" when it has none. Never stored.
	ScanStatus string
}

// Timeline kinds.
const (
	EventCreated       = "CREATED"
	EventSubmitted     = "SUBMITTED"
	EventResubmitted   = "RESUBMITTED"
	EventStatusChanged = "STATUS_CHANGED"
	EventInfoRequested = "INFO_REQUESTED"
	EventWithdrawn     = "WITHDRAWN"
	EventErased        = "ERASED"
)

// Actor types of a timeline event.
const (
	ActorApplicant = "APPLICANT"
	ActorStaff     = "STAFF"
	ActorSystem    = "SYSTEM"
)

// Event is one timeline entry.
type Event struct {
	ID            uuid.UUID
	ApplicationID uuid.UUID
	Kind          string
	FromStatus    string
	ToStatus      string
	Message       string
	ActorType     string
	ActorUserID   *uuid.UUID
	ActorName     string
	At            time.Time
}

// Note is an internal note (never shown to the applicant).
type Note struct {
	ID            uuid.UUID
	ApplicationID uuid.UUID
	Text          string
	AuthorUserID  *uuid.UUID
	AuthorName    string
	At            time.Time
}

// Outbox events (aggregate "application"). Payloads carry IDs only.
const (
	AggregateApplication    = "application"
	EventTypeSubmitted      = "APPLICATION_SUBMITTED"
	EventTypeInfoRequested  = "APPLICATION_INFO_REQUESTED"
	EventTypeStatusChanged  = "APPLICATION_STATUS_CHANGED"
	EventTypeWithdrawn      = "APPLICATION_WITHDRAWN"
	DeletedApplicantName    = "Deleted applicant"
	ErasedByRetention       = "RETENTION"
	ErasedByAccountDeletion = "ACCOUNT_DELETED"
	MaxMessageLength        = 2000
	MaxNoteLength           = 5000
	DefaultPageSize         = 20
	MaxPageSize             = 100
	ReferencePrefix         = "RPA-APP"
	DocumentKeyPrefix       = "applications"
	RetentionBatch          = 200
	PurgeBatch              = 500
)

// OutboxEventTypes are the outbox events this module writes.
func OutboxEventTypes() []string {
	return []string{EventTypeSubmitted, EventTypeInfoRequested, EventTypeStatusChanged, EventTypeWithdrawn}
}

// EventPayload is the payload of every application outbox event.
type EventPayload struct {
	ApplicationID uuid.UUID  `json:"application_id"`
	ListingID     uuid.UUID  `json:"listing_id"`
	From          string     `json:"from,omitempty"`
	To            string     `json:"to,omitempty"`
	Resubmission  bool       `json:"resubmission,omitempty"`
	ActorUserID   *uuid.UUID `json:"actor_user_id,omitempty"`
	OccurredAt    time.Time  `json:"occurred_at"`
}

// Outbox is one event to write with a change.
type Outbox struct {
	ID      uuid.UUID
	Type    string
	Payload EventPayload
}

// PayloadJSON encodes the payload.
func (o Outbox) PayloadJSON() []byte {
	raw, _ := json.Marshal(o.Payload)
	return raw
}

// Change is what one write stores, all together.
type Change struct {
	// Application is written when set: inserted when Insert, otherwise
	// updated by ID.
	Application *Application
	Insert      bool
	// Expect is the updated_at the stored row must still have (optimistic
	// concurrency; zero: no check).
	Expect   time.Time
	Events   []Event
	Notes    []Note
	Outbox   []Outbox
	Audit    []orgaudit.Event
	AddFiles []File
	// RemoveFiles are deleted and their objects queued for the purge.
	RemoveFiles []uuid.UUID
	// NextReference asks the store to give the application its reference
	// (RPA-APP-<year>-<6 digits>) in the same write; the year is the
	// application's LastSubmittedAt year.
	NextReference bool
}

// AppFilter narrows a list of received applications.
type AppFilter struct {
	Book Book
	// ListingIDs limits to these listings (nil: all of the book).
	ListingIDs []uuid.UUID
	ListingID  *uuid.UUID
	Status     string
	Q          string
	Limit      int
	Offset     int
}

// Store persists forms, intakes and applications.
type Store interface {
	// Forms.
	CreateForm(ctx context.Context, f Form) error
	Form(ctx context.Context, id uuid.UUID) (Form, error)
	Forms(ctx context.Context, book Book, status, market string, familyID *uuid.UUID) ([]Form, error)
	// UpdateDraft replaces a DRAFT's content (ErrFormState otherwise;
	// ErrFormLocked when it has submissions).
	UpdateDraft(ctx context.Context, f Form) error
	// CreateVersion inserts the family's next DRAFT (ErrDraftExists when
	// the family has one); it sets f.Version.
	CreateVersion(ctx context.Context, f Form) (Form, error)
	// Publish makes a DRAFT PUBLISHED, retires the family's other PUBLISHED
	// version and moves the intakes that pointed at it.
	Publish(ctx context.Context, id uuid.UUID, at time.Time) (Form, error)
	// Retire makes a PUBLISHED (or DRAFT) version RETIRED.
	Retire(ctx context.Context, id uuid.UUID, at time.Time) (Form, error)

	// Intake.
	Intake(ctx context.Context, listingID uuid.UUID) (Intake, bool, error)
	Intakes(ctx context.Context, listingIDs []uuid.UUID) (map[uuid.UUID]Intake, error)
	PutIntake(ctx context.Context, in Intake) error
	DeleteIntake(ctx context.Context, listingID uuid.UUID) error

	// Applications.
	Application(ctx context.Context, id uuid.UUID) (Application, error)
	// Existing returns the applicant's application for a listing and form version.
	Existing(ctx context.Context, applicant, listingID, formID uuid.UUID) (Application, bool, error)
	Mine(ctx context.Context, applicant uuid.UUID, status string, limit, offset int) ([]Application, int, error)
	Received(ctx context.Context, f AppFilter) ([]Application, int, map[string]int, error)
	// Apply writes a change in one transaction (ErrConflict when Expect no
	// longer matches). It returns the stored application (with its
	// reference when one was assigned).
	Apply(ctx context.Context, ch Change) (Application, error)
	Files(ctx context.Context, applicationID uuid.UUID) ([]File, error)
	Events(ctx context.Context, applicationID uuid.UUID) ([]Event, error)
	Notes(ctx context.Context, applicationID uuid.UUID) ([]Note, error)
	// RecordAudit writes organisation audit events (views).
	RecordAudit(ctx context.Context, events []orgaudit.Event) error

	// Retention and erasure.
	// DueForErasure returns applications in a final state whose decision is
	// older than their form's retention period, not yet erased.
	DueForErasure(ctx context.Context, now time.Time, limit int) ([]Application, error)
	// Erase removes an application's answers and files (queued for the
	// purge) and records the event; the row stays as a tombstone.
	Erase(ctx context.Context, id uuid.UUID, reason string, at time.Time) (bool, error)
	// PendingPurges returns object keys of removed files; DonePurges drops them.
	PendingPurges(ctx context.Context, limit int) ([]string, error)
	DonePurges(ctx context.Context, keys []string) error
}

// Errors.
var (
	ErrUnauthenticated     = errors.New("not signed in")
	ErrForbidden           = errors.New("not allowed")
	ErrFormNotFound        = errors.New("form not found")
	ErrFormNotEditable     = errors.New("form not editable")
	ErrFormLocked          = errors.New("form version has submissions")
	ErrDraftExists         = errors.New("form family has a draft")
	ErrFormState           = errors.New("form state does not allow this")
	ErrListingNotFound     = errors.New("listing not found")
	ErrListingNotEligible  = errors.New("listing does not take applications")
	ErrFormUnavailable     = errors.New("no application form")
	ErrNotFound            = errors.New("application not found")
	ErrNotEditable         = errors.New("application not editable")
	ErrConflict            = errors.New("application changed concurrently")
	ErrFileNotFound        = errors.New("file not found")
	ErrFileTooLarge        = errors.New("file too large")
	ErrUnsupportedFile     = errors.New("unsupported file type")
	ErrTooManyFiles        = errors.New("too many files")
	ErrOrganisationUnknown = errors.New("organisation not found")
)

// ValidationError carries per-field messages (400 VALIDATION_FAILED).
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// ClosedError is 409 APPLICATIONS_CLOSED with the reason.
type ClosedError struct {
	Reason   string
	ClosesAt *time.Time
}

func (e *ClosedError) Error() string { return "applications closed: " + e.Reason }

// TransitionError is 409 INVALID_APPLICATION_TRANSITION.
type TransitionError struct {
	From, To string
	Allowed  []string
}

func (e *TransitionError) Error() string { return "invalid transition " + e.From + " -> " + e.To }

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }
