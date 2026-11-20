package propertyrequests

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// The memory side of messages.go.

// Messages implements Store.
func (s *MemoryStore) Messages(_ context.Context, id uuid.UUID) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Message{}
	for _, m := range s.messages {
		if m.RequestID == id {
			out = append(out, m)
		}
	}
	return out, nil
}

// AddMessage implements Store. The lock makes it all-or-nothing. Like the
// other events of work.go, the events are kept for inspection and not passed
// to the development sink (its handlers would call back into this store).
func (s *MemoryStore) AddMessage(_ context.Context, m Message, allow func(StoredRequest) error) (StoredRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.find(m.RequestID)
	if !ok {
		return StoredRequest{}, ErrNotFound
	}
	cur := s.stored(req)
	if err := allow(cur); err != nil {
		return StoredRequest{}, err
	}
	actor := Actor{Name: m.AuthorName}
	if m.AuthorUserID != nil {
		actor.UserID = *m.AuthorUserID
	}
	w := s.workOf(req)
	event, timelineEvent, actorType := EventCustomerReplied, TimelineCustomerReplied, ActorCustomer
	if m.AuthorType == AuthorProfessional {
		event, timelineEvent, actorType = EventReplied, TimelineReplied, ActorProfessional
		if to := replyAdvance(cur.Stage); to != "" {
			c := StageChange{To: to, Actor: actor, At: m.CreatedAt, Via: StageViaReply}
			if err := s.record(EventStageChanged, m.RequestID, stageEventPayload(cur, cur.Stage, c), m.CreatedAt); err != nil {
				return StoredRequest{}, err
			}
			at, by := c.At, actor.UserID
			w.Stage, w.StageChangedAt, w.StageChangedBy = to, &at, &by
			s.timeline = append(s.timeline, MemoryTimelineEntry{
				RequestID: m.RequestID, EventType: TimelineStageChanged, ActorType: ActorProfessional, CreatedAt: m.CreatedAt,
				ActorUserID: &by, ActorName: actor.Name, FromStage: cur.Stage, ToStage: to,
			})
		}
	}
	if err := s.record(event, m.RequestID, messageEventPayload(cur, m), m.CreatedAt); err != nil {
		return StoredRequest{}, err
	}
	s.messages = append(s.messages, m)
	at := m.CreatedAt
	w.MessageCount++
	w.LastMessageAt, w.LastMessageAuthor, w.UpdatedAt = &at, m.AuthorType, at
	s.timeline = append(s.timeline, MemoryTimelineEntry{
		RequestID: m.RequestID, EventType: timelineEvent, ActorType: actorType, CreatedAt: m.CreatedAt,
		ActorUserID: m.AuthorUserID, ActorName: m.AuthorName,
	})
	return s.stored(req), nil
}

// MarkRead implements Store.
func (s *MemoryStore) MarkRead(_ context.Context, id uuid.UUID, side string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.find(id)
	if !ok {
		return ErrNotFound
	}
	w := s.workOf(req)
	if side == AuthorCustomer {
		w.CustomerLastReadAt = &at
	} else {
		w.ProfessionalLastReadAt = &at
	}
	return nil
}

// StoredMessages returns every message (tests).
func (s *MemoryStore) StoredMessages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.messages...)
}
