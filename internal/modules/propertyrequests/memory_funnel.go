package propertyrequests

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
)

// HistoryStep is one step of a seeded request's history, recorded exactly as
// Assign and ChangeStage record it: an ASSIGNED row (AssignTo set: the
// request goes to that person, recipient type AGENT) or a STAGE_CHANGED row
// (Stage set). Transitions are not checked.
type HistoryStep struct {
	AssignTo     *uuid.UUID
	AssigneeName string
	Stage        string
	Actor        Actor
	At           time.Time
}

// SeedRequest is a request with its history, for the development seed and
// the funnel fixture. Only RECEIVED requests with a recipient belong here.
type SeedRequest struct {
	Request NewRequest
	Steps   []HistoryStep
}

// SeedRequests writes the requests with their histories, replacing any
// request with the same ID (and its timeline, notes and working state), so
// running it again gives the same data. Nothing goes to the outbox.
func (s *MemoryStore) SeedRequests(_ context.Context, seeds []SeedRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seed := range seeds {
		req := seed.Request
		if i := slices.IndexFunc(s.requests, func(r NewRequest) bool { return r.ID == req.ID }); i >= 0 {
			old := s.requests[i]
			s.requests = slices.Delete(s.requests, i, i+1)
			delete(s.byKey, old.IdempotencyKey)
			delete(s.refs, old.Reference)
			delete(s.work, old.ID)
			s.timeline = slices.DeleteFunc(s.timeline, func(e MemoryTimelineEntry) bool { return e.RequestID == old.ID })
			s.notes = slices.DeleteFunc(s.notes, func(n Note) bool { return n.RequestID == old.ID })
			s.messages = slices.DeleteFunc(s.messages, func(m Message) bool { return m.RequestID == old.ID })
		}
		s.requests = append(s.requests, req)
		s.byKey[req.IdempotencyKey] = Record{ID: req.ID, Reference: req.Reference, Status: req.Status, RequestHash: req.RequestHash, CreatedAt: req.CreatedAt}
		s.refs[req.Reference] = struct{}{}
		s.timeline = append(s.timeline, MemoryTimelineEntry{RequestID: req.ID, EventType: TimelineRequestReceived, ActorType: ActorCustomer, CreatedAt: req.CreatedAt})
		for _, step := range seed.Steps {
			w := s.workOf(req)
			by, at := step.Actor.UserID, step.At
			w.UpdatedAt = at
			entry := MemoryTimelineEntry{RequestID: req.ID, ActorType: ActorProfessional, CreatedAt: at, ActorUserID: &by, ActorName: step.Actor.Name}
			if step.AssignTo != nil {
				to := *step.AssignTo
				next := *s.stored(req).Recipient
				next.UserID, next.Type = &to, RecipientAgent
				w.Recipient = &next
				entry.EventType, entry.AssigneeName = TimelineAssigned, step.AssigneeName
			} else {
				entry.EventType, entry.FromStage, entry.ToStage = TimelineStageChanged, w.Stage, step.Stage
				w.Stage, w.StageChangedAt, w.StageChangedBy = step.Stage, &at, &by
			}
			s.timeline = append(s.timeline, entry)
		}
	}
	return nil
}

// FunnelFacts implements Store.
func (s *MemoryStore) FunnelFacts(_ context.Context, f FunnelFilter) ([]FunnelFact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byID := map[uuid.UUID]*FunnelFact{}
	var out []*FunnelFact
	for _, req := range s.requests {
		r := s.stored(req)
		if !f.matches(r) {
			continue
		}
		fact := &FunnelFact{
			RequestID: r.ID, ListingID: r.ListingID, RequestType: r.RequestType, Stage: r.Stage, CreatedAt: r.CreatedAt,
			RecipientType: r.Recipient.Type, HandlerUserID: r.Recipient.UserID, Campaign: r.Campaign,
		}
		byID[r.ID] = fact
		out = append(out, fact)
	}
	for _, e := range s.timeline {
		fact := byID[e.RequestID]
		if fact == nil {
			continue
		}
		switch e.EventType {
		case TimelineAssigned:
			fact.EverAssigned = true
		case TimelineStageChanged:
			fact.Moves = append(fact.Moves, FunnelMove{To: e.ToStage, ActorType: e.ActorType, At: e.CreatedAt})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].RequestID.String() < out[j].RequestID.String()
	})
	facts := make([]FunnelFact, 0, len(out))
	for _, fact := range out {
		sort.SliceStable(fact.Moves, func(i, j int) bool { return fact.Moves[i].At.Before(fact.Moves[j].At) })
		facts = append(facts, *fact)
	}
	return facts, nil
}
