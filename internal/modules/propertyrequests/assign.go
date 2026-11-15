package propertyrequests

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// This file is the assignment of a received request to one person of the
// organisation that received it (POST /v1/agent/property-requests/{id}/assign)
// and the release of a person's open requests when they leave the team.
//
// "Assigned" is the request's recipient user: a request routed to the
// listing's agent is assigned to that agent from the start, one routed to the
// organisation inbox to nobody. Assigning sets the recipient user (recipient
// type AGENT); unassigning returns it to the organisation inbox (recipient
// type ORGANISATION). Who sees it follows from InboxScope: the organisation's
// OWNER and MANAGER always, its AGENT and STAFF only when it is theirs or
// nobody's.

// Timeline events and the outbox event of an assignment.
const (
	TimelineAssigned   = "ASSIGNED"
	TimelineUnassigned = "UNASSIGNED"

	// EventRequestAssigned is written when a request is given to a person;
	// the worker emails that person. Not written for an unassignment.
	EventRequestAssigned = "PROPERTY_REQUEST_ASSIGNED"
)

// Errors of this file.
var (
	// ErrForbidden: the caller sees the request but may not assign it (only
	// the OWNER and MANAGER of the receiving organisation may).
	ErrForbidden = errors.New("not allowed to assign this request")
)

// Assignees says who can hold the requests of an organisation.
type Assignees interface {
	// Assignable returns the name of a user who may be assigned the
	// organisation's requests (an ACTIVE account with an ACTIVE membership
	// whose role holds leads: auth.RoleHoldsLeads); ok is false otherwise.
	Assignable(ctx context.Context, organisationID, userID uuid.UUID) (name string, ok bool, err error)
	// Name returns a user's display name ("" when unknown).
	Name(ctx context.Context, userID uuid.UUID) (string, error)
}

// WithAssignees enables assignment (and the names of assignees in views).
func WithAssignees(a Assignees) Option { return func(s *Service) { s.assignees = a } }

// Assignment gives a request to a person, or back to the organisation inbox
// (To nil).
type Assignment struct {
	To     *uuid.UUID
	ToName string
	Actor  Actor
	At     time.Time
	// Audit is the organisation's audit record, written with the change.
	Audit orgaudit.Event
}

// assignedPayload is the body of PROPERTY_REQUEST_ASSIGNED: identifiers only.
func assignedPayload(r StoredRequest, a Assignment) map[string]any {
	return map[string]any{
		"request_id": r.ID, "reference": r.Reference, "listing_id": r.ListingID, "market_id": r.MarketID,
		"assignee_user_id": a.To, "actor_user_id": a.Actor.UserID, "occurred_at": a.At,
	}
}

// AssignedUserID returns the person a request is assigned to within its
// organisation, or nil (the organisation inbox, a direct seller's request, a
// routing exception).
func (r StoredRequest) AssignedUserID() *uuid.UUID {
	if r.Recipient == nil || r.Recipient.OrganisationID == nil {
		return nil
	}
	return r.Recipient.UserID
}

// Open reports whether a request is still being worked (not CONVERTED or CLOSED).
func (r StoredRequest) Open() bool { return r.Stage != StageConverted && r.Stage != StageClosed }

// CanAssign reports whether the scope's caller may assign the request: its
// OWNER or MANAGER.
func (s InboxScope) CanAssign(r StoredRequest) bool {
	return r.Recipient != nil && r.Recipient.OrganisationID != nil && slices.Contains(s.ManagedOrganisationIDs, *r.Recipient.OrganisationID)
}

// Assign gives a request the caller can see to a member of its organisation
// (to nil: back to the organisation inbox). Only the organisation's OWNER and
// MANAGER may (ErrForbidden); the assignee must be able to hold leads
// (ValidationError on user_id otherwise). Assigning a NEW_ENQUIRY moves it to
// ASSIGNED. Assigning to the current assignee changes nothing.
func (s *Service) Assign(ctx context.Context, scope InboxScope, actor Actor, id uuid.UUID, to *uuid.UUID) (StoredRequest, error) {
	if s.assignees == nil {
		return StoredRequest{}, errors.New("assignment is not configured")
	}
	cur, err := s.store.GetRequest(ctx, id)
	if err != nil {
		return StoredRequest{}, err
	}
	check := func(r StoredRequest) error {
		switch {
		case !scope.Sees(r):
			return ErrNotFound
		case !scope.CanAssign(r):
			return ErrForbidden
		}
		return nil
	}
	if err := check(cur); err != nil {
		return StoredRequest{}, err
	}
	org := *cur.Recipient.OrganisationID
	name := ""
	if to != nil {
		var ok bool
		if name, ok, err = s.assignees.Assignable(ctx, org, *to); err != nil {
			return StoredRequest{}, fmt.Errorf("check assignee: %w", err)
		}
		if !ok {
			return StoredRequest{}, &ValidationError{Fields: map[string]string{
				"user_id": "must be an active estate agent, manager or owner of the organisation that received the request"}}
		}
	}
	prev := cur.AssignedUserID()
	if (prev == nil && to == nil) || (prev != nil && to != nil && *prev == *to) {
		return cur, nil
	}
	before := map[string]any{"assigned_to": nil}
	if prev != nil {
		prevName, err := s.assignees.Name(ctx, *prev)
		if err != nil {
			return StoredRequest{}, err
		}
		before = map[string]any{"assigned_to": prev.String(), "assigned_to_name": prevName}
	}
	after := map[string]any{"assigned_to": nil}
	action := orgaudit.ActionRequestUnassigned
	if to != nil {
		after = map[string]any{"assigned_to": to.String(), "assigned_to_name": name}
		action = orgaudit.ActionRequestAssigned
	}
	now := s.clock()
	a := Assignment{To: to, ToName: name, Actor: actor, At: now,
		Audit: orgaudit.New(org, actor.UserID, actor.Name, action, orgaudit.TargetPropertyRequest, cur.ID, cur.Reference, before, after, now)}
	return s.store.Assign(ctx, id, a, check)
}

// AssigneeNames returns the names of the people the requests are assigned
// to, by user ID.
func (s *Service) AssigneeNames(ctx context.Context, items []StoredRequest) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if s.assignees == nil {
		return out, nil
	}
	for _, r := range items {
		id := r.AssignedUserID()
		if id == nil {
			continue
		}
		if _, done := out[*id]; done {
			continue
		}
		name, err := s.assignees.Name(ctx, *id)
		if err != nil {
			return nil, err
		}
		out[*id] = name
	}
	return out, nil
}

// OpenLeads returns, per member, how many open requests of the organisation
// are assigned to them.
func (s *Service) OpenLeads(ctx context.Context, organisationID uuid.UUID) (map[uuid.UUID]int, error) {
	return s.store.OpenLeads(ctx, organisationID)
}

// ---------------------------------------------------------------------------
// Memory store
// ---------------------------------------------------------------------------

// SetAuditSink receives the organisation audit records this store writes
// (the in-memory team store's log).
func (s *MemoryStore) SetAuditSink(sink orgaudit.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = sink
}

// Assign implements Store. The lock makes it all-or-nothing.
func (s *MemoryStore) Assign(_ context.Context, id uuid.UUID, a Assignment, allow func(StoredRequest) error) (StoredRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.find(id)
	if !ok {
		return StoredRequest{}, ErrNotFound
	}
	cur := s.stored(req)
	if err := allow(cur); err != nil {
		return StoredRequest{}, err
	}
	w := s.workOf(req)
	next := *cur.Recipient
	next.UserID, next.Type = a.To, RecipientOrganisation
	if a.To != nil {
		next.Type = RecipientAgent
	}
	w.Recipient, w.UpdatedAt = &next, a.At
	by := a.Actor.UserID
	event := TimelineUnassigned
	if a.To != nil {
		event = TimelineAssigned
	}
	s.timeline = append(s.timeline, MemoryTimelineEntry{
		RequestID: id, EventType: event, ActorType: ActorProfessional, CreatedAt: a.At,
		ActorUserID: &by, ActorName: a.Actor.Name, AssigneeName: a.ToName,
	})
	if a.To != nil && cur.Stage == StageNewEnquiry {
		change := StageChange{To: StageAssigned, Actor: a.Actor, At: a.At}
		if err := s.record(EventStageChanged, id, stageEventPayload(cur, cur.Stage, change), a.At); err != nil {
			return StoredRequest{}, err
		}
		at := a.At
		w.Stage, w.StageChangedAt, w.StageChangedBy = StageAssigned, &at, &by
		s.timeline = append(s.timeline, MemoryTimelineEntry{
			RequestID: id, EventType: TimelineStageChanged, ActorType: ActorProfessional, CreatedAt: a.At,
			ActorUserID: &by, ActorName: a.Actor.Name, FromStage: StageNewEnquiry, ToStage: StageAssigned,
		})
	}
	if a.To != nil {
		if err := s.emitWork(EventRequestAssigned, id, assignedPayload(cur, a), a.At); err != nil {
			return StoredRequest{}, err
		}
	}
	if s.audit != nil {
		s.audit(a.Audit)
	}
	return s.stored(req), nil
}

// emitWork records an outbox event and hands it to the sink (unlike record,
// for the work events that send an email). Caller holds the lock.
func (s *MemoryStore) emitWork(eventType string, id uuid.UUID, payload map[string]any, at time.Time) error {
	if err := s.record(eventType, id, payload, at); err != nil {
		return err
	}
	if s.sink != nil {
		ev := s.outbox[len(s.outbox)-1]
		s.sink(outboxEventOf(ev))
	}
	return nil
}

// OpenLeads implements Store.
func (s *MemoryStore) OpenLeads(_ context.Context, organisationID uuid.UUID) (map[uuid.UUID]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]int{}
	for _, req := range s.requests {
		r := s.stored(req)
		if id := r.AssignedUserID(); id != nil && *r.Recipient.OrganisationID == organisationID && r.Open() {
			out[*id]++
		}
	}
	return out, nil
}

// ReleaseMember returns the open requests of an organisation that are
// assigned to the user to the organisation inbox, each with an UNASSIGNED
// timeline entry, and returns how many there were. It is the in-memory half
// of removing or suspending a member (the team store calls it under its own
// lock; Postgres does the same in the team change's transaction).
func (s *MemoryStore) ReleaseMember(organisationID, userID uuid.UUID, actor Actor, at time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, req := range s.requests {
		r := s.stored(req)
		id := r.AssignedUserID()
		if id == nil || *id != userID || *r.Recipient.OrganisationID != organisationID || !r.Open() {
			continue
		}
		w := s.workOf(req)
		next := *r.Recipient
		next.UserID, next.Type = nil, RecipientOrganisation
		w.Recipient, w.UpdatedAt = &next, at
		by := actor.UserID
		s.timeline = append(s.timeline, MemoryTimelineEntry{
			RequestID: req.ID, EventType: TimelineUnassigned, ActorType: ActorProfessional, CreatedAt: at,
			ActorUserID: &by, ActorName: actor.Name,
		})
		n++
	}
	return n
}

// SetAgentEligible marks the user as able (or not) to receive new requests as
// the assigned agent of the organisation's listings. In Postgres this is read
// from the membership on every request (GetListing); the memory store keeps
// it on the listing, so the team store updates it when a member is suspended,
// reactivated or removed.
func (s *MemoryStore) SetAgentEligible(organisationID, userID uuid.UUID, eligible bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.listings {
		if l.OrganisationID != nil && *l.OrganisationID == organisationID && l.AgentUserID != nil && *l.AgentUserID == userID {
			l.AgentEligible = eligible
			s.listings[id] = l
		}
	}
}
