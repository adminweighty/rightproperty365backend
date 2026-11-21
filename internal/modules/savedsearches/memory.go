package savedsearches

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore is the in-memory Store (development and tests).
type MemoryStore struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]SavedSearch
	outbox []outbox.Event
	sink   outbox.Sink
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{byID: map[uuid.UUID]SavedSearch{}} }

// SetOutboxSink registers a receiver for events as they are written.
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// Outbox returns the events written so far.
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]outbox.Event{}, s.outbox...)
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, userID uuid.UUID) ([]SavedSearch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []SavedSearch{}
	for _, ss := range s.byID {
		if ss.UserID == userID {
			out = append(out, ss)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out, nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (SavedSearch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.byID[id]
	if !ok {
		return SavedSearch{}, ErrNotFound
	}
	return ss, nil
}

// Create implements Store.
func (s *MemoryStore) Create(_ context.Context, ss SavedSearch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, x := range s.byID {
		if x.UserID == ss.UserID {
			n++
		}
	}
	if n >= MaxPerUser {
		return ErrLimit
	}
	s.byID[ss.ID] = ss
	return nil
}

// Update implements Store.
func (s *MemoryStore) Update(_ context.Context, ss SavedSearch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.byID[ss.ID]
	if !ok {
		return ErrNotFound
	}
	cur.Name, cur.Frequency, cur.LastCheckedAt, cur.NextCheckAt, cur.UpdatedAt = ss.Name, ss.Frequency, ss.LastCheckedAt, ss.NextCheckAt, ss.UpdatedAt
	s.byID[ss.ID] = cur
	return nil
}

// Delete implements Store.
func (s *MemoryStore) Delete(_ context.Context, userID, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.byID[id]
	if !ok || cur.UserID != userID {
		return ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

// Unsubscribe implements Store.
func (s *MemoryStore) Unsubscribe(_ context.Context, token string, now time.Time) (SavedSearch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ss := range s.byID {
		if ss.UnsubscribeToken == token {
			if ss.Frequency != FrequencyOff {
				ss.Frequency, ss.NextCheckAt, ss.UpdatedAt = FrequencyOff, nil, now
				s.byID[id] = ss
			}
			return ss, nil
		}
	}
	return SavedSearch{}, ErrInvalidToken
}

// Claim implements Store.
func (s *MemoryStore) Claim(_ context.Context, now time.Time, lease time.Duration, limit int) ([]SavedSearch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	due := []SavedSearch{}
	for _, ss := range s.byID {
		if ss.Frequency != FrequencyOff && ss.NextCheckAt != nil && !ss.NextCheckAt.After(now) {
			due = append(due, ss)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].NextCheckAt.Before(*due[j].NextCheckAt) })
	if len(due) > limit {
		due = due[:limit]
	}
	for _, ss := range due {
		next := now.Add(lease)
		ss.NextCheckAt = &next
		s.byID[ss.ID] = ss
	}
	return due, nil
}

// Record implements Store.
func (s *MemoryStore) Record(_ context.Context, id uuid.UUID, checkedAt time.Time, alert *Alert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.byID[id]
	if !ok {
		return nil // deleted meanwhile
	}
	ss.LastCheckedAt, ss.NextCheckAt = checkedAt, nextCheck(ss.Frequency, checkedAt)
	if alert != nil && ss.Frequency != FrequencyOff {
		at := checkedAt
		ss.LastAlertAt = &at
		payload, err := json.Marshal(EventPayload{SavedSearchID: id, UserID: ss.UserID, Total: alert.Total, More: alert.More, Lines: alert.Lines, OccurredAt: checkedAt})
		if err != nil {
			return err
		}
		ev := outbox.Event{ID: uuid.New(), Type: EventMatched, AggregateType: AggregateSavedSearch, AggregateID: id, Payload: payload, Attempts: 1, CreatedAt: checkedAt}
		s.outbox = append(s.outbox, ev)
		if s.sink != nil {
			s.sink(ev)
		}
	}
	s.byID[id] = ss
	return nil
}

// DeleteUser removes the user's saved searches (account deletion).
func (s *MemoryStore) DeleteUser(userID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ss := range s.byID {
		if ss.UserID == userID {
			delete(s.byID, id)
		}
	}
}
