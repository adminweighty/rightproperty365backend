package saved

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/outbox"
)

type key struct{ user, listing uuid.UUID }

// MemoryStore is the in-memory Store for development and tests. It mirrors
// the Postgres store, outbox included.
type MemoryStore struct {
	mu     sync.Mutex
	rows   map[key]Saved
	outbox []outbox.Event
	sink   outbox.Sink
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{rows: map[key]Saved{}} }

// SetOutboxSink registers a receiver for events as they are written.
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// Outbox returns a copy of the outbox events (test helper).
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]outbox.Event(nil), s.outbox...)
}

// Put stores a save as it is, without an event (the development seed).
func (s *MemoryStore) Put(x Saved) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[key{x.UserID, x.ListingID}] = x
}

// DeleteUser removes every save of a user (account deletion; in Postgres
// the deletion transaction does it).
func (s *MemoryStore) DeleteUser(userID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.rows {
		if k.user == userID {
			delete(s.rows, k)
		}
	}
}

// emit writes an outbox event. Caller holds the lock.
func (s *MemoryStore) emit(eventType string, userID, listingID uuid.UUID, at time.Time) {
	ev := outbox.Event{ID: uuid.New(), Type: eventType, AggregateType: AggregateListing, AggregateID: listingID,
		Payload: eventPayload(userID, listingID, at), Attempts: 1, CreatedAt: at}
	s.outbox = append(s.outbox, ev)
	if s.sink != nil {
		s.sink(ev)
	}
}

// Save implements Store.
func (s *MemoryStore) Save(_ context.Context, x Saved) (Saved, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{x.UserID, x.ListingID}
	if existing, ok := s.rows[k]; ok {
		return existing, false, nil
	}
	s.rows[k] = x
	s.emit(EventListingSaved, x.UserID, x.ListingID, x.SavedAt)
	return x, true, nil
}

// Remove implements Store.
func (s *MemoryStore) Remove(_ context.Context, userID, listingID uuid.UUID, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{userID, listingID}
	if _, ok := s.rows[k]; !ok {
		return false, nil
	}
	delete(s.rows, k)
	s.emit(EventListingUnsaved, userID, listingID, at)
	return true, nil
}

// mine returns the user's saves in list order. Caller holds the lock.
func (s *MemoryStore) mine(userID uuid.UUID) []Saved {
	var out []Saved
	for k, x := range s.rows {
		if k.user == userID {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].SavedAt.Equal(out[j].SavedAt) {
			return out[i].SavedAt.After(out[j].SavedAt)
		}
		return out[i].ListingID.String() < out[j].ListingID.String()
	})
	return out
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, userID uuid.UUID, limit, offset int) ([]Saved, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.mine(userID)
	total := len(all)
	if offset >= total {
		return []Saved{}, total, nil
	}
	end := min(offset+limit, total)
	return append([]Saved{}, all[offset:end]...), total, nil
}

// ListingIDs implements Store.
func (s *MemoryStore) ListingIDs(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []uuid.UUID{}
	for _, x := range s.mine(userID) {
		out = append(out, x.ListingID)
	}
	return out, nil
}

// UpdateSnapshot implements Store.
func (s *MemoryStore) UpdateSnapshot(_ context.Context, userID, listingID uuid.UUID, snap Snapshot, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{userID, listingID}
	if x, ok := s.rows[k]; ok {
		x.Snapshot, x.SnapshotAt = snap, at
		s.rows[k] = x
	}
	return nil
}
