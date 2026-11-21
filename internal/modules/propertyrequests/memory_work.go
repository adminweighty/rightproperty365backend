package propertyrequests

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
)

// memoryWork is what migration 0018 adds to a property_requests row.
type memoryWork struct {
	Stage          string
	StageChangedAt *time.Time
	StageChangedBy *uuid.UUID
	UpdatedAt      time.Time
	ResolvedAt     *time.Time
	ResolvedBy     *uuid.UUID
	ResolutionNote string
	// Recipient replaces the routed recipient once the request was assigned
	// or unassigned (nil: as routed).
	Recipient *Recipient
	// Status replaces the stored status once a routing exception was
	// reassigned (API 0.49.0; "" keeps it).
	Status string
	// The conversation summary and read markers (migration 0048).
	MessageCount           int
	LastMessageAt          *time.Time
	LastMessageAuthor      string
	CustomerLastReadAt     *time.Time
	ProfessionalLastReadAt *time.Time
}

// stored builds the stored form of a request. Caller holds the lock.
func (s *MemoryStore) stored(req NewRequest) StoredRequest {
	r := StoredRequest{
		ID: req.ID, Reference: req.Reference, MarketID: req.MarketID, ListingID: req.ListingID,
		RequestType: req.RequestType, Status: req.Status, Stage: StageNewEnquiry,
		Message: req.Message, PreferredContactMethod: req.PreferredContactMethod, PreferredSlot: req.PreferredSlot,
		CustomerUserID: req.CustomerUserID, ContactName: req.GuestName, ContactEmail: req.GuestEmail, ContactPhone: req.GuestPhone,
		Recipient: req.Recipient, Stay: req.Stay, CreatedAt: req.CreatedAt, UpdatedAt: req.CreatedAt,
	}
	if b, ok := s.branches[req.ListingID]; ok {
		r.ListingBranchID = &b
	}
	if w, ok := s.work[req.ID]; ok {
		r.Stage, r.StageChangedAt, r.StageChangedBy, r.UpdatedAt = w.Stage, w.StageChangedAt, w.StageChangedBy, w.UpdatedAt
		r.ResolvedAt, r.ResolvedBy, r.ResolutionNote = w.ResolvedAt, w.ResolvedBy, w.ResolutionNote
		r.MessageCount, r.LastMessageAt, r.LastMessageAuthor = w.MessageCount, w.LastMessageAt, w.LastMessageAuthor
		r.CustomerLastReadAt, r.ProfessionalLastReadAt = w.CustomerLastReadAt, w.ProfessionalLastReadAt
		if w.Recipient != nil {
			recipient := *w.Recipient
			r.Recipient = &recipient
		}
		if w.Status != "" {
			r.Status = w.Status
		}
	}
	return r
}

// workOf returns the mutable working state of a request. Caller holds the lock.
func (s *MemoryStore) workOf(req NewRequest) *memoryWork {
	w, ok := s.work[req.ID]
	if !ok {
		w = &memoryWork{Stage: StageNewEnquiry, UpdatedAt: req.CreatedAt}
		s.work[req.ID] = w
	}
	return w
}

// find returns the request with the ID. Caller holds the lock.
func (s *MemoryStore) find(id uuid.UUID) (NewRequest, bool) {
	for _, req := range s.requests {
		if req.ID == id {
			return req, true
		}
	}
	return NewRequest{}, false
}

// newestFirst orders by creation time, newest first; UUIDv7 IDs break ties.
func newestFirst(items []StoredRequest) {
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID.String() > items[j].ID.String()
	})
}

func pageOf(items []StoredRequest, limit, offset int) []StoredRequest {
	start := min(max(offset, 0), len(items))
	end := len(items)
	if limit > 0 {
		end = min(start+limit, len(items))
	}
	return append([]StoredRequest{}, items[start:end]...)
}

// ListInbox implements Store with the visibility rule of InboxScope.Sees.
func (s *MemoryStore) ListInbox(_ context.Context, f InboxFilter) (InboxPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	page := InboxPage{StageCounts: map[string]int{}}
	var kept []StoredRequest
	for _, req := range s.requests {
		r := s.stored(req)
		if !f.matches(r) {
			continue
		}
		page.StageCounts[r.Stage]++
		if len(f.Stages) == 0 || slices.Contains(f.Stages, r.Stage) {
			kept = append(kept, r)
		}
	}
	newestFirst(kept)
	page.Total = len(kept)
	page.Items = pageOf(kept, f.Limit, f.Offset)
	return page, nil
}

// GetRequest implements Store.
func (s *MemoryStore) GetRequest(_ context.Context, id uuid.UUID) (StoredRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.find(id)
	if !ok {
		return StoredRequest{}, ErrNotFound
	}
	return s.stored(req), nil
}

// ListByCustomer implements Store.
func (s *MemoryStore) ListByCustomer(_ context.Context, f CustomerFilter) ([]StoredRequest, int, map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	var kept []StoredRequest
	for _, req := range s.requests {
		if req.CustomerUserID == nil || *req.CustomerUserID != f.UserID {
			continue
		}
		r := s.stored(req)
		counts[r.Stage]++
		if f.Stages == nil || slices.Contains(f.Stages, r.Stage) {
			kept = append(kept, r)
		}
	}
	newestFirst(kept)
	return pageOf(kept, f.Limit, f.Offset), len(kept), counts, nil
}

// RequestTimeline implements Store.
func (s *MemoryStore) RequestTimeline(_ context.Context, id uuid.UUID) ([]TimelineEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []TimelineEntry{}
	for _, e := range s.timeline {
		if e.RequestID == id {
			out = append(out, TimelineEntry{
				EventType: e.EventType, ActorType: e.ActorType, ActorUserID: e.ActorUserID, ActorName: e.ActorName,
				FromStage: e.FromStage, ToStage: e.ToStage, At: e.CreatedAt, AssigneeName: e.AssigneeName,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Notes implements Store.
func (s *MemoryStore) Notes(_ context.Context, id uuid.UUID) ([]Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Note{}
	for _, n := range s.notes {
		if n.RequestID == id {
			out = append(out, n)
		}
	}
	return out, nil
}

// record appends an outbox event of work.go. These events have no consumer
// yet, so they are kept for inspection and not passed to the sink (whose
// development handler only knows the events that send email).
func (s *MemoryStore) record(eventType string, id uuid.UUID, payload map[string]any, at time.Time) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.outbox = append(s.outbox, MemoryOutboxEvent{EventType: eventType, AggregateID: id, Payload: body, CreatedAt: at})
	return nil
}

// ChangeStage implements Store. The lock makes it all-or-nothing.
func (s *MemoryStore) ChangeStage(_ context.Context, id uuid.UUID, c StageChange, allow func(StoredRequest) error) (StoredRequest, error) {
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
	if err := s.record(EventStageChanged, id, stageEventPayload(cur, cur.Stage, c), c.At); err != nil {
		return StoredRequest{}, err
	}
	w := s.workOf(req)
	at, by := c.At, c.Actor.UserID
	w.Stage, w.StageChangedAt, w.StageChangedBy, w.UpdatedAt = c.To, &at, &by, c.At
	s.timeline = append(s.timeline, MemoryTimelineEntry{
		RequestID: id, EventType: TimelineStageChanged, ActorType: ActorProfessional, CreatedAt: c.At,
		ActorUserID: &by, ActorName: c.Actor.Name, FromStage: cur.Stage, ToStage: c.To,
	})
	return s.stored(req), nil
}

// AddNote implements Store.
func (s *MemoryStore) AddNote(_ context.Context, n Note, allow func(StoredRequest) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.find(n.RequestID)
	if !ok {
		return ErrNotFound
	}
	if err := allow(s.stored(req)); err != nil {
		return err
	}
	s.notes = append(s.notes, n)
	s.workOf(req).UpdatedAt = n.CreatedAt
	by := n.AuthorUserID
	s.timeline = append(s.timeline, MemoryTimelineEntry{
		RequestID: n.RequestID, EventType: TimelineNoteAdded, ActorType: ActorProfessional, CreatedAt: n.CreatedAt,
		ActorUserID: &by, ActorName: n.AuthorName,
	})
	return nil
}

// ListExceptions implements Store.
func (s *MemoryStore) ListExceptions(_ context.Context, f ExceptionFilter) ([]StoredRequest, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []StoredRequest
	for _, req := range s.requests {
		r := s.stored(req)
		if (r.Status != StatusRoutingException && r.ResolvedAt == nil) || (f.MarketIDs != nil && !slices.Contains(f.MarketIDs, r.MarketID)) ||
			(f.Resolved != nil && *f.Resolved != (r.ResolvedAt != nil)) {
			continue
		}
		kept = append(kept, r)
	}
	newestFirst(kept)
	slices.Reverse(kept) // the longest-waiting first
	return pageOf(kept, f.Limit, f.Offset), len(kept), nil
}

// ResolveException implements Store.
func (s *MemoryStore) ResolveException(_ context.Context, id uuid.UUID, res Resolution, allow func(StoredRequest) error) (StoredRequest, error) {
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
	if err := s.record(EventExceptionResolved, id, resolvedEventPayload(cur, res), res.At); err != nil {
		return StoredRequest{}, err
	}
	w := s.workOf(req)
	at, by := res.At, res.Actor.UserID
	w.ResolvedAt, w.ResolvedBy, w.ResolutionNote, w.UpdatedAt = &at, &by, res.Note, res.At
	s.timeline = append(s.timeline, MemoryTimelineEntry{
		RequestID: id, EventType: TimelineExceptionResolved, ActorType: ActorAdmin, CreatedAt: res.At,
		ActorUserID: &by, ActorName: res.Actor.Name,
	})
	return s.stored(req), nil
}

// AdminCounts implements Store.
func (s *MemoryStore) AdminCounts(_ context.Context, marketIDs []uuid.UUID, now time.Time) (AdminCounts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := AdminCounts{ByStage: map[string]int{}}
	for _, req := range s.requests {
		r := s.stored(req)
		if marketIDs != nil && !slices.Contains(marketIDs, r.MarketID) {
			continue
		}
		c.Total++
		if r.CreatedAt.After(now.AddDate(0, 0, -7)) {
			c.Last7Days++
		}
		if r.CreatedAt.After(now.AddDate(0, 0, -30)) {
			c.Last30Days++
		}
		switch {
		case r.Status != StatusRoutingException:
			c.ByStage[r.Stage]++
		case r.ResolvedAt != nil:
			c.ExceptionsResolved++
		default:
			c.ExceptionsOpen++
		}
	}
	return c, nil
}

// StoredNotes returns a copy of every note (test helper).
func (s *MemoryStore) StoredNotes() []Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Note(nil), s.notes...)
}

// StoredRequests returns every request with its working state (reports).
func (s *MemoryStore) StoredRequests() []StoredRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StoredRequest, 0, len(s.requests))
	for _, req := range s.requests {
		out = append(out, s.stored(req))
	}
	return out
}
