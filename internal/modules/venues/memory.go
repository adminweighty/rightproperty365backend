package venues

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore is a mutex-guarded in-memory Store for development and tests.
// One mutex guards everything, so a confirmation's capacity check and its
// write are one critical section, as the Postgres transaction is.
type MemoryStore struct {
	mu       sync.Mutex
	settings map[uuid.UUID]Settings
	quotes   map[uuid.UUID]Quote
	bookings map[uuid.UUID]Booking
	order    []uuid.UUID
	refs     map[string]bool
	events   []Event
	outbox   []outbox.Event
	sink     outbox.Sink
	audit    orgaudit.Sink
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{settings: map[uuid.UUID]Settings{}, quotes: map[uuid.UUID]Quote{}, bookings: map[uuid.UUID]Booking{}, refs: map[string]bool{}}
}

// SetOutboxSink registers a receiver for events as they are written.
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// SetAuditSink registers the organisation audit trail.
func (s *MemoryStore) SetAuditSink(sink orgaudit.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = sink
}

func (s *MemoryStore) emit(eventType string, b Booking, e Event) error {
	payload, err := eventPayload(b, e)
	if err != nil {
		return err
	}
	ev := outbox.Event{ID: uuid.New(), Type: eventType, AggregateType: AggregateVenueBooking, AggregateID: b.ID, Payload: payload, Attempts: 1, CreatedAt: e.At}
	s.outbox = append(s.outbox, ev)
	if s.sink != nil {
		s.sink(ev)
	}
	return nil
}

func cloneSettings(v Settings) Settings {
	v.OpeningHours = append([]OpeningRange{}, v.OpeningHours...)
	v.Exceptions = append([]Exception{}, v.Exceptions...)
	v.Fees = append([]Fee{}, v.Fees...)
	return v
}

// Settings implements Store.
func (s *MemoryStore) Settings(_ context.Context, listingID uuid.UUID) (Settings, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.settings[listingID]
	return cloneSettings(v), ok, nil
}

// SettingsOf implements Store.
func (s *MemoryStore) SettingsOf(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]Settings{}
	for _, id := range ids {
		if v, ok := s.settings[id]; ok {
			out[id] = cloneSettings(v)
		}
	}
	return out, nil
}

// PutSettings implements Store.
func (s *MemoryStore) PutSettings(_ context.Context, v Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings[v.ListingID] = cloneSettings(v)
	return nil
}

// InsertQuote implements Store.
func (s *MemoryStore) InsertQuote(_ context.Context, q Quote) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotes[q.ID] = q
	return nil
}

// Quote implements Store.
func (s *MemoryStore) Quote(_ context.Context, id uuid.UUID) (Quote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.quotes[id]
	if !ok {
		return Quote{}, ErrQuoteNotFound
	}
	return q, nil
}

func (s *MemoryStore) occupied(listingID uuid.UUID, from, to time.Time, except *uuid.UUID) []Booking {
	window := Interval{Start: from, End: to}
	var out []Booking
	for _, id := range s.order {
		b := s.bookings[id]
		if b.ListingID != listingID || !b.Occupies() || (except != nil && *except == b.ID) || !b.Block().Overlaps(window) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// Occupied implements Store.
func (s *MemoryStore) Occupied(_ context.Context, listingID uuid.UUID, from, to time.Time, except *uuid.UUID) ([]Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.occupied(listingID, from, to, except), nil
}

// CreateBooking implements Store.
func (s *MemoryStore) CreateBooking(_ context.Context, b Booking, e Event) (Booking, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		if s.bookings[id].QuoteID == b.QuoteID {
			return s.bookings[id], false, nil
		}
	}
	q, ok := s.quotes[b.QuoteID]
	if !ok {
		return Booking{}, false, ErrQuoteNotFound
	}
	if q.UsedBy != nil {
		return Booking{}, false, ErrQuoteUsed
	}
	if s.refs[b.Reference] {
		return Booking{}, false, ErrReferenceCollision
	}
	used := b.ID
	q.UsedBy = &used
	s.quotes[q.ID] = q
	s.put(b, e)
	return b, true, s.emit(EventRequested, b, e)
}

func (s *MemoryStore) put(b Booking, events ...Event) {
	if _, exists := s.bookings[b.ID]; !exists {
		s.order = append(s.order, b.ID)
	}
	s.bookings[b.ID] = b
	s.refs[b.Reference] = true
	s.events = append(s.events, events...)
}

// Seed stores a booking, its quote and its history as they are, without
// events (development sample data and tests).
func (s *MemoryStore) Seed(q Quote, b Booking, events ...Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	used := b.ID
	q.UsedBy = &used
	s.quotes[q.ID] = q
	s.put(b, events...)
}

// BookingByQuote implements Store.
func (s *MemoryStore) BookingByQuote(_ context.Context, quoteID uuid.UUID) (Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		if s.bookings[id].QuoteID == quoteID {
			return s.bookings[id], nil
		}
	}
	return Booking{}, ErrNotFound
}

// Booking implements Store.
func (s *MemoryStore) Booking(_ context.Context, id uuid.UUID) (Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bookings[id]
	if !ok {
		return Booking{}, ErrNotFound
	}
	return b, nil
}

// Events implements Store.
func (s *MemoryStore) Events(_ context.Context, id uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Event{}
	for _, e := range s.events {
		if e.BookingID == id {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Update implements Store: the whole decision, the capacity check and the
// write run under the store's mutex.
func (s *MemoryStore) Update(_ context.Context, id uuid.UUID, decide func(Booking, Settings) (Mutation, error)) (Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.bookings[id]
	if !ok {
		return Booking{}, ErrNotFound
	}
	m, err := decide(cur, cloneSettings(s.settings[cur.ListingID]))
	if err != nil {
		return Booking{}, err
	}
	next := m.Next
	if m.Lock {
		block := next.Block()
		if err := lockCheck(next, s.settings[cur.ListingID], s.occupied(cur.ListingID, block.Start, block.End, &next.ID)); err != nil {
			return Booking{}, err
		}
	}
	if m.UseQuote != nil {
		q, ok := s.quotes[*m.UseQuote]
		if !ok {
			return Booking{}, ErrQuoteNotFound
		}
		if q.UsedBy != nil && *q.UsedBy != next.ID {
			return Booking{}, ErrQuoteUsed
		}
		used := next.ID
		q.UsedBy = &used
		s.quotes[q.ID] = q
	}
	s.put(next, m.Event)
	if m.Outbox != "" {
		if err := s.emit(m.Outbox, next, m.Event); err != nil {
			return Booking{}, err
		}
	}
	if m.Audit != nil && s.audit != nil {
		s.audit(*m.Audit)
	}
	return next, nil
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, f Filter) ([]Booking, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	matched := []Booking{}
	for _, id := range s.order {
		if b := s.bookings[id]; f.Matches(b) {
			matched = append(matched, b)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].Start.Equal(matched[j].Start) {
			return matched[i].Start.Before(matched[j].Start)
		}
		return matched[i].ID.String() < matched[j].ID.String()
	})
	total := len(matched)
	start := (f.Page - 1) * f.PageSize
	if start < 0 || start >= total {
		return []Booking{}, total, nil
	}
	return matched[start:min(start+f.PageSize, total)], total, nil
}

// Outbox returns a copy of the outbox events (test helper).
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]outbox.Event(nil), s.outbox...)
}

// All returns every booking in creation order (test helper).
func (s *MemoryStore) All() []Booking {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Booking, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.bookings[id])
	}
	return out
}

// DeleteCustomer removes every booking of a customer whose account is being
// deleted (the Postgres store does the same in the account deletion
// transaction). Their events go too; quotes that used them stay unlinked.
func (s *MemoryStore) DeleteCustomer(userID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gone := map[uuid.UUID]bool{}
	for id, b := range s.bookings {
		if b.CustomerUserID == userID {
			gone[id] = true
			delete(s.bookings, id)
		}
	}
	if len(gone) == 0 {
		return
	}
	order := s.order[:0]
	for _, id := range s.order {
		if !gone[id] {
			order = append(order, id)
		}
	}
	s.order = order
	events := s.events[:0]
	for _, e := range s.events {
		if !gone[e.BookingID] {
			events = append(events, e)
		}
	}
	s.events = events
}
