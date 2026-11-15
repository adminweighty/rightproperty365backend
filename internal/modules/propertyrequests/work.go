package propertyrequests

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// This file is everything that happens to a request AFTER it was received:
// the lead stage the receiving side moves it through, their internal notes,
// what the customer who sent it may read back, and the routing exceptions a
// platform administrator clears.

// Lead stages, in pipeline order (the eight stages of the updated functional
// specification). A request starts at StageNewEnquiry.
const (
	StageNewEnquiry       = "NEW_ENQUIRY"
	StageAssigned         = "ASSIGNED"
	StageContacted        = "CONTACTED"
	StageViewingRequested = "VIEWING_REQUESTED"
	StageViewingScheduled = "VIEWING_SCHEDULED"
	StageFollowUpRequired = "FOLLOW_UP_REQUIRED"
	StageConverted        = "CONVERTED"
	StageClosed           = "CLOSED"
)

var stageLabels = map[string]string{
	StageNewEnquiry:       "New enquiry",
	StageAssigned:         "Assigned",
	StageContacted:        "Contacted",
	StageViewingRequested: "Viewing requested",
	StageViewingScheduled: "Viewing scheduled",
	StageFollowUpRequired: "Follow-up required",
	StageConverted:        "Converted",
	StageClosed:           "Closed",
}

// Stages returns every lead stage in pipeline order.
func Stages() []string {
	return []string{
		StageNewEnquiry, StageAssigned, StageContacted, StageViewingRequested,
		StageViewingScheduled, StageFollowUpRequired, StageConverted, StageClosed,
	}
}

// StageLabel is the wording shown for a stage.
func StageLabel(stage string) string {
	if l, ok := stageLabels[stage]; ok {
		return l
	}
	return stage
}

// stageMoves says which stages a request may move to from each stage:
//
//   - nothing ever goes back to NEW_ENQUIRY;
//   - a request is CONVERTED only after there was contact or a viewing;
//   - a CONVERTED request can only be CLOSED;
//   - a CLOSED request can only be reopened as FOLLOW_UP_REQUIRED.
var stageMoves = map[string][]string{
	StageNewEnquiry:       {StageAssigned, StageContacted, StageViewingRequested, StageViewingScheduled, StageFollowUpRequired, StageClosed},
	StageAssigned:         {StageContacted, StageViewingRequested, StageViewingScheduled, StageFollowUpRequired, StageClosed},
	StageContacted:        {StageViewingRequested, StageViewingScheduled, StageFollowUpRequired, StageConverted, StageClosed},
	StageViewingRequested: {StageContacted, StageViewingScheduled, StageFollowUpRequired, StageClosed},
	StageViewingScheduled: {StageContacted, StageViewingRequested, StageFollowUpRequired, StageConverted, StageClosed},
	StageFollowUpRequired: {StageContacted, StageViewingRequested, StageViewingScheduled, StageConverted, StageClosed},
	StageConverted:        {StageClosed},
	StageClosed:           {StageFollowUpRequired},
}

// NextStages returns the stages a request at stage may move to.
func NextStages(stage string) []string { return slices.Clone(stageMoves[stage]) }

// Customer statuses: what the person who sent a request is told about it.
// They are derived from the lead stage and say nothing about who handles the
// request or how. A request that could not be routed is still RECEIVED to its
// customer (see StatusReceived).
const (
	CustomerReceived         = "RECEIVED"
	CustomerInProgress       = "IN_PROGRESS"
	CustomerResponded        = "RESPONDED"
	CustomerViewingScheduled = "VIEWING_SCHEDULED"
	CustomerClosed           = "CLOSED"
)

var customerLabels = map[string]string{
	CustomerReceived:         "Received",
	CustomerInProgress:       "In progress",
	CustomerResponded:        "Responded",
	CustomerViewingScheduled: "Viewing scheduled",
	CustomerClosed:           "Closed",
}

// CustomerStatuses returns every customer status in order.
func CustomerStatuses() []string {
	return []string{CustomerReceived, CustomerInProgress, CustomerResponded, CustomerViewingScheduled, CustomerClosed}
}

// CustomerStatusLabel is the wording shown for a customer status.
func CustomerStatusLabel(status string) string {
	if l, ok := customerLabels[status]; ok {
		return l
	}
	return status
}

// CustomerStatus maps a lead stage to what the customer is told.
func CustomerStatus(stage string) string {
	switch stage {
	case StageAssigned, StageViewingRequested, StageFollowUpRequired:
		return CustomerInProgress
	case StageContacted:
		return CustomerResponded
	case StageViewingScheduled:
		return CustomerViewingScheduled
	case StageConverted, StageClosed:
		return CustomerClosed
	}
	return CustomerReceived
}

// stagesOfCustomerStatuses returns the lead stages behind the given customer
// statuses (nil when statuses is empty: no filter).
func stagesOfCustomerStatuses(statuses []string) []string {
	if len(statuses) == 0 {
		return nil
	}
	var out []string
	for _, stage := range Stages() {
		if slices.Contains(statuses, CustomerStatus(stage)) {
			out = append(out, stage)
		}
	}
	return out
}

// Timeline event types written after creation.
const (
	TimelineStageChanged      = "STAGE_CHANGED"
	TimelineNoteAdded         = "NOTE_ADDED"
	TimelineExceptionResolved = "ROUTING_EXCEPTION_RESOLVED"

	// ActorProfessional is the receiving side (stored as AGENT, the value the
	// timeline has always allowed for it); ActorAdmin a platform administrator.
	ActorProfessional = "AGENT"
	ActorAdmin        = "ADMIN"
)

// Outbox events written after creation. Nothing sends an email for them: the
// worker acknowledges them (see WorkEventTypes).
const (
	EventStageChanged      = "PROPERTY_REQUEST_STAGE_CHANGED"
	EventExceptionResolved = "PROPERTY_REQUEST_EXCEPTION_RESOLVED"
)

// WorkEventTypes lists the outbox events of this file. They carry
// identifiers and stages only. No notification exists for them yet, so the
// worker registers a handler that acknowledges them.
func WorkEventTypes() []string { return []string{EventStageChanged, EventExceptionResolved} }

// Limits.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
	MaxNoteLength   = 2000
	MaxSearchLength = 100
)

// Errors of this file.
var (
	// ErrInvalidTransition: the stage cannot be reached from the current one.
	ErrInvalidTransition = errors.New("stage transition not allowed")
	// ErrAlreadyResolved: the routing exception was resolved before.
	ErrAlreadyResolved = errors.New("routing exception already resolved")
)

// StoredRequest is a request as it is stored, with its working state.
type StoredRequest struct {
	ID                     uuid.UUID
	Reference              string
	MarketID               uuid.UUID
	ListingID              uuid.UUID
	RequestType            string
	Status                 string // RECEIVED or ROUTING_EXCEPTION
	Stage                  string
	StageChangedAt         *time.Time
	StageChangedBy         *uuid.UUID
	Message                string
	PreferredContactMethod string
	PreferredSlot          *time.Time
	CustomerUserID         *uuid.UUID
	ContactName            string
	ContactEmail           string
	ContactPhone           string // "" when not given
	Recipient              *Recipient
	Stay                   *StayDetails
	CreatedAt              time.Time
	UpdatedAt              time.Time
	// Routing exceptions only.
	ResolvedAt     *time.Time
	ResolvedBy     *uuid.UUID
	ResolutionNote string
}

// Sees reports whether a request is visible under the scope. It is the one
// statement of the rule documented on InboxScope; the Postgres list query
// expresses the same in SQL.
func (s InboxScope) Sees(r StoredRequest) bool {
	if r.Recipient == nil {
		return false // routing exception: in nobody's inbox
	}
	assignee := r.Recipient.UserID
	if r.Recipient.OrganisationID != nil {
		org := *r.Recipient.OrganisationID
		if slices.Contains(s.ManagedOrganisationIDs, org) {
			return true
		}
		if slices.Contains(s.MemberOrganisationIDs, org) && (assignee == nil || *assignee == s.UserID) {
			return true
		}
	}
	return r.Recipient.Type == RecipientSeller && assignee != nil && *assignee == s.UserID
}

// InboxFilter selects one page of an inbox. Scope decides which requests can
// be seen at all; everything else only narrows that.
type InboxFilter struct {
	Scope     InboxScope
	Stages    []string
	Type      string
	ListingID *uuid.UUID
	// From (inclusive) and To (exclusive) bound created_at.
	From *time.Time
	To   *time.Time
	// Q is matched, case-insensitively, against the reference, the customer's
	// name and email and the message.
	Q      string
	Limit  int
	Offset int
}

// matches applies everything but the stage (memory store; Postgres has the
// same in SQL). The stage is applied separately so that the counts per stage
// cover every stage under the other filters.
func (f InboxFilter) matches(r StoredRequest) bool {
	if !f.Scope.Sees(r) {
		return false
	}
	if f.Type != "" && r.RequestType != f.Type {
		return false
	}
	if f.ListingID != nil && r.ListingID != *f.ListingID {
		return false
	}
	if f.From != nil && r.CreatedAt.Before(*f.From) {
		return false
	}
	if f.To != nil && !r.CreatedAt.Before(*f.To) {
		return false
	}
	if f.Q != "" {
		q := strings.ToLower(f.Q)
		if !strings.Contains(strings.ToLower(r.Reference), q) && !strings.Contains(strings.ToLower(r.ContactName), q) &&
			!strings.Contains(strings.ToLower(r.ContactEmail), q) && !strings.Contains(strings.ToLower(r.Message), q) {
			return false
		}
	}
	return true
}

// InboxPage is one page of an inbox.
type InboxPage struct {
	Items []StoredRequest
	// Total is the number of requests matching the whole filter.
	Total int
	// StageCounts is the number of requests per stage under the filter
	// WITHOUT its stage part.
	StageCounts map[string]int
}

// CustomerFilter selects one page of the requests an account sent.
type CustomerFilter struct {
	UserID uuid.UUID
	Stages []string // nil: every stage
	Limit  int
	Offset int
}

// TimelineEntry is one row of a request's history.
type TimelineEntry struct {
	EventType   string
	ActorType   string
	ActorUserID *uuid.UUID
	ActorName   string
	FromStage   string
	ToStage     string
	// AssigneeName is who an ASSIGNED entry gave the request to.
	AssigneeName string
	At           time.Time
}

// Note is an internal note of the receiving side.
type Note struct {
	ID           uuid.UUID
	RequestID    uuid.UUID
	AuthorUserID uuid.UUID
	AuthorName   string
	Body         string
	CreatedAt    time.Time
}

// Actor is who changes a request.
type Actor struct {
	UserID uuid.UUID
	Name   string
}

// StageChange is one move of a request to another stage.
type StageChange struct {
	To    string
	Actor Actor
	At    time.Time
}

// Resolution marks a routing exception as dealt with.
type Resolution struct {
	Actor Actor
	Note  string
	At    time.Time
}

// ExceptionFilter selects one page of the routing exceptions.
type ExceptionFilter struct {
	// MarketIDs limits the list to these markets; nil means every market.
	MarketIDs []uuid.UUID
	// Resolved: nil all, false only open, true only resolved.
	Resolved *bool
	Limit    int
	Offset   int
}

// AdminCounts are the request numbers of the administrators' overview.
type AdminCounts struct {
	Total      int
	ByStage    map[string]int
	Last7Days  int
	Last30Days int
	// Routing exceptions that are still open, and those resolved.
	ExceptionsOpen     int
	ExceptionsResolved int
}

// WorkStore is the part of Store behind this file.
type WorkStore interface {
	// ListInbox returns one page under the filter, newest first, with the
	// total and the counts per stage (see InboxPage).
	ListInbox(ctx context.Context, f InboxFilter) (InboxPage, error)
	// GetRequest returns one request whoever it belongs to, or ErrNotFound.
	// The caller decides whether it may be shown.
	GetRequest(ctx context.Context, id uuid.UUID) (StoredRequest, error)
	// ListByCustomer returns one page of the requests sent by an account,
	// newest first, the total under the filter and the account's number of
	// requests per stage (all of them, whatever the filter).
	ListByCustomer(ctx context.Context, f CustomerFilter) (items []StoredRequest, total int, stageCounts map[string]int, err error)
	// RequestTimeline returns a request's history, oldest first.
	RequestTimeline(ctx context.Context, id uuid.UUID) ([]TimelineEntry, error)
	// Notes returns a request's internal notes, oldest first.
	Notes(ctx context.Context, id uuid.UUID) ([]Note, error)
	// ChangeStage moves a request atomically: it reads the current row under
	// a lock, calls allow with it, and only when allow returns nil writes the
	// new stage, its timeline entry and its outbox event together. An error
	// from allow is returned as is. ErrNotFound when the request is missing.
	ChangeStage(ctx context.Context, id uuid.UUID, change StageChange, allow func(current StoredRequest) error) (StoredRequest, error)
	// AddNote stores a note and its NOTE_ADDED timeline entry (which never
	// carries the note's text) after allow accepted the current row.
	AddNote(ctx context.Context, note Note, allow func(current StoredRequest) error) error
	// ListExceptions returns one page of routing exceptions, oldest first
	// (the longest-waiting first), and the total.
	ListExceptions(ctx context.Context, f ExceptionFilter) ([]StoredRequest, int, error)
	// ResolveException marks a routing exception resolved, with its timeline
	// entry and outbox event, after allow accepted the current row.
	ResolveException(ctx context.Context, id uuid.UUID, res Resolution, allow func(current StoredRequest) error) (StoredRequest, error)
	// AdminCounts counts requests in the markets (nil: every market).
	AdminCounts(ctx context.Context, marketIDs []uuid.UUID, now time.Time) (AdminCounts, error)
	// Assign gives a request to a person or back to the organisation inbox
	// (see assign.go) after allow accepted the current row: the recipient,
	// its ASSIGNED / UNASSIGNED timeline entry, the move from NEW_ENQUIRY to
	// ASSIGNED (with its own timeline entry and event), the
	// PROPERTY_REQUEST_ASSIGNED event and the audit record, together.
	Assign(ctx context.Context, id uuid.UUID, a Assignment, allow func(current StoredRequest) error) (StoredRequest, error)
	// OpenLeads counts, per assignee, the organisation's open requests
	// (not CONVERTED or CLOSED) assigned to someone.
	OpenLeads(ctx context.Context, organisationID uuid.UUID) (map[uuid.UUID]int, error)
}

// stageEventPayload is the body of PROPERTY_REQUEST_STAGE_CHANGED:
// identifiers and stages only.
func stageEventPayload(r StoredRequest, from string, c StageChange) map[string]any {
	return map[string]any{
		"request_id": r.ID, "reference": r.Reference, "listing_id": r.ListingID, "market_id": r.MarketID,
		"from": from, "to": c.To, "actor_user_id": c.Actor.UserID, "occurred_at": c.At,
	}
}

// resolvedEventPayload is the body of PROPERTY_REQUEST_EXCEPTION_RESOLVED.
func resolvedEventPayload(r StoredRequest, res Resolution) map[string]any {
	return map[string]any{
		"request_id": r.ID, "reference": r.Reference, "listing_id": r.ListingID, "market_id": r.MarketID,
		"actor_user_id": res.Actor.UserID, "occurred_at": res.At,
	}
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// InboxPage returns one page of the requests visible under the filter's scope.
func (s *Service) InboxPage(ctx context.Context, f InboxFilter) (InboxPage, error) {
	if f.Limit <= 0 || f.Limit > MaxPageSize {
		f.Limit = DefaultPageSize
	}
	page, err := s.store.ListInbox(ctx, f)
	if err != nil {
		return InboxPage{}, fmt.Errorf("list inbox: %w", err)
	}
	if page.StageCounts == nil {
		page.StageCounts = map[string]int{}
	}
	return page, nil
}

// StageCounts returns the number of visible requests per stage.
func (s *Service) StageCounts(ctx context.Context, scope InboxScope) (map[string]int, error) {
	page, err := s.InboxPage(ctx, InboxFilter{Scope: scope, Limit: 1})
	return page.StageCounts, err
}

// Received returns one request visible under scope. A request that does not
// exist and one that belongs to somebody else are the same answer: ErrNotFound.
func (s *Service) Received(ctx context.Context, scope InboxScope, id uuid.UUID) (StoredRequest, error) {
	r, err := s.store.GetRequest(ctx, id)
	if err != nil {
		return StoredRequest{}, err
	}
	if !scope.Sees(r) {
		return StoredRequest{}, ErrNotFound
	}
	return r, nil
}

// History returns the timeline and the notes of a request. The caller must
// have loaded the request through Received or Mine first.
func (s *Service) History(ctx context.Context, id uuid.UUID) ([]TimelineEntry, []Note, error) {
	entries, err := s.store.RequestTimeline(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("load timeline: %w", err)
	}
	notes, err := s.store.Notes(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("load notes: %w", err)
	}
	return entries, notes, nil
}

func seen(scope InboxScope) func(StoredRequest) error {
	return func(cur StoredRequest) error {
		if !scope.Sees(cur) {
			return ErrNotFound
		}
		return nil
	}
}

// MoveStage moves a request the caller can see to another stage.
func (s *Service) MoveStage(ctx context.Context, scope InboxScope, actor Actor, id uuid.UUID, to string) (StoredRequest, error) {
	if !slices.Contains(Stages(), to) {
		return StoredRequest{}, &ValidationError{Fields: map[string]string{"stage": "must be one of " + strings.Join(Stages(), ", ")}}
	}
	visible := seen(scope)
	return s.store.ChangeStage(ctx, id, StageChange{To: to, Actor: actor, At: s.clock()}, func(cur StoredRequest) error {
		if err := visible(cur); err != nil {
			return err
		}
		if !slices.Contains(stageMoves[cur.Stage], to) {
			return ErrInvalidTransition
		}
		return nil
	})
}

// cleanText trims a free-text value and refuses control characters.
func cleanText(raw string, max int) (string, string) {
	text := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	switch {
	case text == "":
		return "", "is required"
	case !utf8.ValidString(text):
		return "", "must be valid text"
	case utf8.RuneCountInString(text) > max:
		return "", fmt.Sprintf("must be at most %d characters", max)
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", "must not contain control characters"
		}
	}
	return text, ""
}

// AddNote adds an internal note to a request the caller can see.
func (s *Service) AddNote(ctx context.Context, scope InboxScope, actor Actor, id uuid.UUID, body string) (Note, error) {
	text, problem := cleanText(body, MaxNoteLength)
	if problem != "" {
		return Note{}, &ValidationError{Fields: map[string]string{"body": problem}}
	}
	noteID, err := uuid.NewV7()
	if err != nil {
		return Note{}, fmt.Errorf("generate note id: %w", err)
	}
	note := Note{ID: noteID, RequestID: id, AuthorUserID: actor.UserID, AuthorName: actor.Name, Body: text, CreatedAt: s.clock()}
	if err := s.store.AddNote(ctx, note, seen(scope)); err != nil {
		return Note{}, err
	}
	return note, nil
}

// MinePage is one page of the requests an account sent.
type MinePage struct {
	Items        []StoredRequest
	Total        int
	StatusCounts map[string]int // by customer status
}

// Mine returns the requests the account itself sent while signed in, newest
// first. A request sent as a guest is not linked to an account, even one
// registered later with the same email address.
func (s *Service) Mine(ctx context.Context, userID uuid.UUID, statuses []string, limit, offset int) (MinePage, error) {
	items, total, byStage, err := s.store.ListByCustomer(ctx, CustomerFilter{
		UserID: userID, Stages: stagesOfCustomerStatuses(statuses), Limit: limit, Offset: offset,
	})
	if err != nil {
		return MinePage{}, fmt.Errorf("list own requests: %w", err)
	}
	out := MinePage{Items: items, Total: total, StatusCounts: map[string]int{}}
	for stage, n := range byStage {
		out.StatusCounts[CustomerStatus(stage)] += n
	}
	return out, nil
}

// MineOne returns one request the account sent, or ErrNotFound.
func (s *Service) MineOne(ctx context.Context, userID, id uuid.UUID) (StoredRequest, []TimelineEntry, error) {
	r, err := s.store.GetRequest(ctx, id)
	if err != nil {
		return StoredRequest{}, nil, err
	}
	if r.CustomerUserID == nil || *r.CustomerUserID != userID {
		return StoredRequest{}, nil, ErrNotFound
	}
	entries, err := s.store.RequestTimeline(ctx, id)
	if err != nil {
		return StoredRequest{}, nil, fmt.Errorf("load timeline: %w", err)
	}
	return r, entries, nil
}

// Exceptions returns one page of routing exceptions for an administrator.
func (s *Service) Exceptions(ctx context.Context, f ExceptionFilter) ([]StoredRequest, int, error) {
	items, total, err := s.store.ListExceptions(ctx, f)
	if err != nil {
		return nil, 0, fmt.Errorf("list routing exceptions: %w", err)
	}
	return items, total, nil
}

// ResolveException marks a routing exception as dealt with. marketIDs limits
// what the administrator may touch (nil: every market); a request outside
// them, and one that is not a routing exception, is ErrNotFound.
func (s *Service) ResolveException(ctx context.Context, marketIDs []uuid.UUID, actor Actor, id uuid.UUID, rawNote string) (StoredRequest, error) {
	note := ""
	if strings.TrimSpace(rawNote) != "" {
		var problem string
		if note, problem = cleanText(rawNote, MaxNoteLength); problem != "" {
			return StoredRequest{}, &ValidationError{Fields: map[string]string{"note": problem}}
		}
	}
	return s.store.ResolveException(ctx, id, Resolution{Actor: actor, Note: note, At: s.clock()}, func(cur StoredRequest) error {
		switch {
		case cur.Status != StatusRoutingException, marketIDs != nil && !slices.Contains(marketIDs, cur.MarketID):
			return ErrNotFound
		case cur.ResolvedAt != nil:
			return ErrAlreadyResolved
		}
		return nil
	})
}

// AdminCounts returns the request numbers of the administrators' overview.
func (s *Service) AdminCounts(ctx context.Context, marketIDs []uuid.UUID) (AdminCounts, error) {
	c, err := s.store.AdminCounts(ctx, marketIDs, s.clock())
	if err != nil {
		return AdminCounts{}, fmt.Errorf("count requests: %w", err)
	}
	if c.ByStage == nil {
		c.ByStage = map[string]int{}
	}
	return c, nil
}
