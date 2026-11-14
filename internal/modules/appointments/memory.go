package appointments

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore is a mutex-guarded in-memory Store for development and tests
// only. Data is lost on restart.
type MemoryStore struct {
	mu       sync.Mutex
	byID     map[uuid.UUID]Appointment
	order    []uuid.UUID
	byKey    map[string]uuid.UUID
	refs     map[string]struct{}
	timeline []TimelineEntry
	outbox   []outbox.Event
	sink     outbox.Sink
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: map[uuid.UUID]Appointment{}, byKey: map[string]uuid.UUID{}, refs: map[string]struct{}{}}
}

// SetOutboxSink registers a receiver for events as they are written
// (development without a database). The sink must not call back into the
// store synchronously.
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// emit records an outbox event and hands it to the sink. Callers hold the lock.
func (s *MemoryStore) emit(eventType string, a Appointment, e TimelineEntry) error {
	payload, err := eventPayload(a, e)
	if err != nil {
		return err
	}
	ev := outbox.Event{
		ID: uuid.New(), Type: eventType, AggregateType: AggregateAppointment, AggregateID: a.ID,
		Payload: payload, Attempts: 1, CreatedAt: e.At,
	}
	s.outbox = append(s.outbox, ev)
	if s.sink != nil {
		s.sink(ev)
	}
	return nil
}

// FindByIdempotencyKey implements Store.
func (s *MemoryStore) FindByIdempotencyKey(_ context.Context, key string) (Appointment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byKey[key]
	if !ok {
		return Appointment{}, ErrNotFound
	}
	return s.byID[id], nil
}

// Create implements Store. Holding the mutex for the whole call gives the
// same all-or-nothing behaviour as the Postgres transaction.
func (s *MemoryStore) Create(_ context.Context, a Appointment, entry TimelineEntry) (Appointment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.create(a, entry, EventRequested)
}

func (s *MemoryStore) create(a Appointment, entry TimelineEntry, event string) (Appointment, bool, error) {
	if a.IdempotencyKey != "" {
		if id, ok := s.byKey[a.IdempotencyKey]; ok {
			return s.byID[id], false, nil
		}
	}
	if _, taken := s.refs[a.Reference]; taken {
		return Appointment{}, false, ErrReferenceCollision
	}
	s.byID[a.ID] = a
	s.order = append(s.order, a.ID)
	s.refs[a.Reference] = struct{}{}
	if a.IdempotencyKey != "" {
		s.byKey[a.IdempotencyKey] = a.ID
	}
	s.timeline = append(s.timeline, entry)
	if event != "" {
		if err := s.emit(event, a, entry); err != nil {
			return Appointment{}, false, err
		}
	}
	return a, true, nil
}

// CreateLinked is the hook the in-memory property request store calls, under
// its own lock, to create the appointment that belongs to a request: the two
// are written together or not at all.
func (s *MemoryStore) CreateLinked(req propertyrequests.NewRequest) error {
	if req.Appointment == nil {
		return nil
	}
	linked, ok := req.Appointment.Data.(Linked)
	if !ok {
		return errors.New("appointments: unexpected linked appointment data")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.refs[linked.Appointment.Reference]; taken {
		return propertyrequests.ErrReferenceCollision
	}
	_, _, err := s.create(linked.Appointment, linked.Entry, EventRequested)
	return err
}

// Seed stores an appointment and its timeline as they are, without events
// (development sample data and tests).
func (s *MemoryStore) Seed(a Appointment, entries ...TimelineEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[a.ID]; !exists {
		s.order = append(s.order, a.ID)
	}
	s.byID[a.ID] = a
	s.refs[a.Reference] = struct{}{}
	s.timeline = append(s.timeline, entries...)
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Appointment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[id]
	if !ok {
		return Appointment{}, ErrNotFound
	}
	return a, nil
}

// Timeline implements Store.
func (s *MemoryStore) Timeline(_ context.Context, id uuid.UUID) ([]TimelineEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []TimelineEntry{}
	for _, e := range s.timeline {
		if e.AppointmentID == id {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Update implements Store. The overlap check runs under the store lock, so
// two confirmations of overlapping appointments cannot both succeed.
func (s *MemoryStore) Update(_ context.Context, id uuid.UUID, decide func(Appointment) (Mutation, error)) (Appointment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.byID[id]
	if !ok {
		return Appointment{}, ErrNotFound
	}
	m, err := decide(current)
	if err != nil {
		return Appointment{}, err
	}
	next := m.Next
	if next.Status == StatusConfirmed {
		if key, ok := next.BusyKey(); ok {
			for otherID, other := range s.byID {
				if otherID == id || other.Status != StatusConfirmed {
					continue
				}
				if otherKey, ok := other.BusyKey(); ok && otherKey == key && other.Overlaps(next) {
					return Appointment{}, ErrSlotTaken
				}
			}
		}
	}
	s.byID[id] = next
	s.timeline = append(s.timeline, m.Entry)
	if m.Event != "" {
		if err := s.emit(m.Event, next, m.Entry); err != nil {
			return Appointment{}, err
		}
	}
	return next, nil
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, f ListFilter) ([]Appointment, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	matched := []Appointment{}
	for _, id := range s.order {
		if a := s.byID[id]; f.matches(a) {
			matched = append(matched, a)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if !a.StartUTC.Equal(b.StartUTC) {
			if f.Ascending {
				return a.StartUTC.Before(b.StartUTC)
			}
			return a.StartUTC.After(b.StartUTC)
		}
		return a.ID.String() < b.ID.String()
	})
	total := len(matched)
	start := (f.Page - 1) * f.PageSize
	if start < 0 || start >= total {
		return []Appointment{}, total, nil
	}
	end := min(start+f.PageSize, total)
	return matched[start:end], total, nil
}

// Count implements Store.
func (s *MemoryStore) Count(_ context.Context, scope Scope, now time.Time) (Counts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Counts
	for _, a := range s.byID {
		if !scope.Sees(a) {
			continue
		}
		switch {
		case a.Status == StatusRequested:
			c.Requested++
		case a.Status == StatusRescheduleRequested:
			c.RescheduleRequested++
		case a.Status == StatusConfirmed && a.EndUTC().After(now):
			c.UpcomingConfirmed++
		}
	}
	return c, nil
}

// All returns a copy of every appointment in creation order (test helper).
func (s *MemoryStore) All() []Appointment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Appointment, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.byID[id])
	}
	return out
}

// Outbox returns a copy of the outbox events (test helper).
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]outbox.Event(nil), s.outbox...)
}
