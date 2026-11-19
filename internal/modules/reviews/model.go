// Package reviews holds the reviews of service providers (API 0.36.4,
// BR-019, FR-047; Phase 2 part e).
//
// Only someone with a completed, linked job or enquiry may review a public
// service-provider profile of the directory:
//
//   - an organisation (or a direct owner), on POST
//     /v1/agent/organisations/{id}/reviews, for a RESOLVED or CLOSED
//     maintenance request of its book currently assigned to that provider
//     (basis MAINTENANCE);
//   - a signed-in customer, on POST /v1/me/reviews, for a business enquiry
//     they sent to that provider at least 14 days ago and that the provider
//     marked as replied to (basis ENQUIRY).
//
// One review per basis. A new review is PENDING until a moderator
// publishes or rejects it; a published one may be hidden (and published
// again). The provider replies once (published directly, reportable),
// editable for 7 days. Anybody signed in may report a published review; it
// stays visible until a moderator acts. Reviews are never anonymous to the
// platform: the reviewer's user ID (and organisation) is always stored; the
// public sees the first name and last initial, or the organisation's name.
package reviews

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// Basis kinds.
const (
	BasisMaintenance = "MAINTENANCE"
	BasisEnquiry     = "ENQUIRY"
)

// Reviewer kinds: a customer (POST /v1/me/reviews), an organisation or a
// direct owner (the book of POST /v1/agent/organisations/{id}/reviews).
const (
	ReviewerCustomer     = "CUSTOMER"
	ReviewerOrganisation = "ORGANISATION"
	ReviewerDirectOwner  = "DIRECT_OWNER"
)

// Review statuses.
const (
	StatusPending   = "PENDING"
	StatusPublished = "PUBLISHED"
	StatusHidden    = "HIDDEN"
	StatusRejected  = "REJECTED"
)

// Report statuses.
const (
	ReportOpen     = "OPEN"
	ReportResolved = "RESOLVED"
)

// Moderation actions (the admin routes and the outbox payload).
const (
	ActionPublish     = "PUBLISH"
	ActionHide        = "HIDE"
	ActionReject      = "REJECT"
	ActionRemoveReply = "REMOVE_REPLY"
)

// Limits and rules.
const (
	MinRating, MaxRating = 1, 5
	MinText, MaxText     = 20, 2000
	MaxReply             = 2000
	MaxReason            = 500
	// EnquiryMinAge is how old a replied enquiry must be to be a basis.
	EnquiryMinAge = 14 * 24 * time.Hour
	// ReplyEditWindow is how long the provider may edit their reply.
	ReplyEditWindow = 7 * 24 * time.Hour
	// ReviewableKind is the directory kind whose profiles may be reviewed.
	ReviewableKind = "service-providers"
	// FormerCustomer is the public name of a review whose (individual)
	// reviewer deleted their account.
	FormerCustomer = "Former customer"
	// MaxOwnReviews caps the reviewer's own lists (never paged).
	MaxOwnReviews = 200
)

// Ineligibility reasons (409 REVIEW_NOT_ELIGIBLE details.reason).
const (
	ReasonBasisNotFound = "BASIS_NOT_FOUND"
	ReasonWrongProvider = "WRONG_PROVIDER"
	ReasonNotCompleted  = "NOT_COMPLETED"
	ReasonTooRecent     = "TOO_RECENT"
	ReasonNotReplied    = "NOT_REPLIED"
)

var statusLabels = map[string]string{
	StatusPending: "Awaiting moderation", StatusPublished: "Published", StatusHidden: "Hidden", StatusRejected: "Not published",
}

// StatusLabel is a status as people read it.
func StatusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

// Statuses returns every status, in order.
func Statuses() []string {
	return []string{StatusPending, StatusPublished, StatusHidden, StatusRejected}
}

// Errors.
var (
	ErrUnauthenticated = errors.New("not signed in")
	ErrBookNotFound    = errors.New("book not found")
	ErrForbidden       = errors.New("forbidden")
	ErrNotFound        = errors.New("review not found")
	ErrProfileNotFound = errors.New("profile not found")
	ErrConflict        = errors.New("review changed meanwhile")
	// ErrBasisReviewed: the basis already has a review (one per basis).
	ErrBasisReviewed = errors.New("basis already reviewed")
	// ErrAlreadyReported: the caller has an open report of this review.
	ErrAlreadyReported = errors.New("already reported")
	ErrReplyExists     = errors.New("reply exists")
	ErrNoReply         = errors.New("no reply")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// IneligibleError: the basis does not allow a review (409 REVIEW_NOT_ELIGIBLE).
type IneligibleError struct {
	Reason string
	// EligibleFrom is when a too recent enquiry becomes a basis.
	EligibleFrom *time.Time
}

func (e *IneligibleError) Error() string { return "not eligible: " + e.Reason }

// StateError: the review's status does not allow the change (409).
type StateError struct {
	Code   string
	Status string
	// Until is when the reply stopped being editable (REVIEW_REPLY_LOCKED).
	Until *time.Time
}

func (e *StateError) Error() string { return fmt.Sprintf("%s (status %s)", e.Code, e.Status) }

// Reply is the provider's one reply.
type Reply struct {
	Text      string
	At        time.Time
	UpdatedAt time.Time
	ByUserID  *uuid.UUID
	ByName    string
}

// Review is one review of a service provider.
type Review struct {
	ID uuid.UUID
	// The profile as it was reviewed.
	ProfileID              uuid.UUID
	ProviderOrganisationID uuid.UUID
	ProviderKind           string
	ProviderSlug           string
	ProviderName           string

	ReviewerKind           string
	ReviewerUserID         *uuid.UUID
	ReviewerOrganisationID *uuid.UUID
	// ReviewerName is the public name; AuthorName the member who wrote an
	// organisation's review (private).
	ReviewerName string
	AuthorName   string

	BasisKind      string
	BasisID        uuid.UUID
	BasisReference string

	Rating int
	Text   string
	Status string

	ModerationReason string
	ModeratedAt      *time.Time
	ModeratedBy      *uuid.UUID
	ModeratedByName  string
	// PublishedAt is the first publication (kept when hidden).
	PublishedAt *time.Time

	Reply              *Reply
	ReplyRemovedAt     *time.Time
	ReplyRemovedReason string

	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time

	// OpenReports is computed by the stores.
	OpenReports int
}

// Report is one report of a published review.
type Report struct {
	ID             uuid.UUID
	ReviewID       uuid.UUID
	ReporterUserID *uuid.UUID
	Reason         string
	Status         string
	CreatedAt      time.Time
	ResolvedAt     *time.Time
	ResolvedBy     *uuid.UUID
	// Resolution is the moderation action that closed it.
	Resolution string
}

// Rating is the published reviews of one profile: their count and sum.
type Rating struct {
	Count int
	Sum   int
}

// Filter narrows a list of reviews. Empty fields match everything.
type Filter struct {
	ProfileID              *uuid.UUID
	ProviderOrganisationID *uuid.UUID
	// Reviewer: a customer's own (kind CUSTOMER and user), or a book's
	// (an organisation, or a direct owner's user with kind DIRECT_OWNER).
	ReviewerKind           string
	ReviewerUserID         *uuid.UUID
	ReviewerOrganisationID *uuid.UUID
	Statuses               []string
	// Queue keeps PENDING reviews and those with an open report.
	Queue bool
	// Reported keeps reviews with an open report.
	Reported  bool
	BasisKind string
	Rating    int
	// Q: the provider's name or the text contains (case-insensitive).
	Q string
	// Order: "published" (published_at, newest first) or "" (created_at,
	// newest first); "queue" is oldest first.
	Order         string
	Limit, Offset int
}

// ReportResolution closes the open reports of a review.
type ReportResolution struct {
	ReviewID   uuid.UUID
	At         time.Time
	By         uuid.UUID
	Resolution string
}

// EventPayload is the outbox payload of a moderation: ids and statuses only.
type EventPayload struct {
	ReviewID     uuid.UUID  `json:"review_id"`
	Action       string     `json:"action"`
	Status       string     `json:"status"`
	FirstPublish bool       `json:"first_publish"`
	ActorUserID  *uuid.UUID `json:"actor_user_id,omitempty"`
	OccurredAt   time.Time  `json:"occurred_at"`
}

// Outbox event and aggregate.
const (
	OutboxModerated = "PROVIDER_REVIEW_MODERATED"
	AggregateReview = "provider_review"
)

// OutboxEventTypes are the events the notifications module handles.
func OutboxEventTypes() []string { return []string{OutboxModerated} }

// Change is one atomic write. Stores apply it in this order: insert
// Create (a second review of its basis: ErrBasisReviewed); update Update
// when its stored version is ExpectVersion (else ErrConflict; a missing
// review ErrNotFound), storing version ExpectVersion+1; delete Delete;
// resolve the open reports of Resolve; insert Reports (a second open report
// by the same person: ErrAlreadyReported; a review that is not PUBLISHED:
// ErrNotFound); insert Audit and Notices.
type Change struct {
	Create        *Review
	Update        *Review
	ExpectVersion int
	Delete        *uuid.UUID
	Resolve       *ReportResolution
	Reports       []Report
	Audit         []adminaudit.Event
	Notices       []EventPayload
}

// Store persists reviews.
type Store interface {
	// Review returns one review (with OpenReports), or ErrNotFound.
	Review(ctx context.Context, id uuid.UUID) (Review, error)
	// Reviews returns one page of reviews and the total.
	Reviews(ctx context.Context, f Filter) ([]Review, int, error)
	// ByBasis returns the reviews of those bases of one kind, by basis ID.
	ByBasis(ctx context.Context, kind string, ids []uuid.UUID) (map[uuid.UUID]Review, error)
	// Reports returns a review's reports, newest first.
	Reports(ctx context.Context, reviewID uuid.UUID) ([]Report, error)
	// Ratings returns the published reviews' count and sum per profile.
	Ratings(ctx context.Context, profileIDs []uuid.UUID) (map[uuid.UUID]Rating, error)
	// QueueCounts returns how many reviews are PENDING and how many have an
	// open report.
	QueueCounts(ctx context.Context) (pending, reported int, err error)
	// Apply writes a Change atomically.
	Apply(ctx context.Context, ch Change) error
}

// PublicName is how an individual reviewer is shown: the first name and
// the initial of the last name ("Tariro T."), the only name when there is
// one, or "Customer" when there is none.
func PublicName(full string) string {
	words := strings.Fields(full)
	switch len(words) {
	case 0:
		return "Customer"
	case 1:
		return words[0]
	}
	last := []rune(words[len(words)-1])
	return words[0] + " " + string(unicode.ToUpper(last[0])) + "."
}

// Average is the mean rating with one decimal, rounded half up ("4.5"), or
// nil when there is no review.
func Average(r Rating) *string {
	if r.Count <= 0 {
		return nil
	}
	tenths := (r.Sum*20 + r.Count) / (2 * r.Count)
	s := fmt.Sprintf("%d.%d", tenths/10, tenths%10)
	return &s
}

func newID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}
