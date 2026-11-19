package maintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// LedgerMemory is the ledger memory store's transaction hook.
type LedgerMemory interface {
	ApplyWith(ctx context.Context, ch ledger.Change, with func() error) error
}

// MemoryStore keeps maintenance requests in memory (development and tests).
// It mirrors the Postgres store's guards.
type MemoryStore struct {
	mu       sync.Mutex
	requests map[uuid.UUID]Request
	events   []Event
	photos   map[uuid.UUID]Photo
	costs    map[uuid.UUID]Cost
	refs     map[int]int
	outbox   []outbox.Event
	sink     outbox.Sink
	audit    orgaudit.Sink
	ledger   LedgerMemory
	// failNext, when set, fails the next Apply after its checks (tests).
	failNext error
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{requests: map[uuid.UUID]Request{}, photos: map[uuid.UUID]Photo{}, costs: map[uuid.UUID]Cost{}, refs: map[int]int{}}
}

// SetLedger connects the ledger's memory store: a cost and its entry are
// kept together, or neither.
func (s *MemoryStore) SetLedger(l LedgerMemory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ledger = l
}

// SetAuditSink receives the organisation audit records this store writes.
func (s *MemoryStore) SetAuditSink(sink orgaudit.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = sink
}

// SetOutboxSink registers a receiver for outbox events as they are written.
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// FailNext makes the next Apply fail with err after its checks (tests).
func (s *MemoryStore) FailNext(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = err
}

// Outbox returns the outbox events written so far (tests).
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.outbox)
}

// NextReference implements Store.
func (s *MemoryStore) NextReference(_ context.Context, year int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[year]++
	return fmt.Sprintf("%s-%d-%06d", ReferencePrefix, year, s.refs[year]), nil
}

func (s *MemoryStore) counted(r Request) Request {
	r.PhotoCount, r.CommentCount = 0, 0
	for _, p := range s.photos {
		if p.RequestID == r.ID {
			r.PhotoCount++
		}
	}
	for _, e := range s.events {
		if e.RequestID == r.ID && e.Type == EventComment {
			r.CommentCount++
		}
	}
	return r
}

func clone(r Request) Request {
	if r.Provider != nil {
		p := *r.Provider
		r.Provider = &p
	}
	return r
}

// Request implements Store.
func (s *MemoryStore) Request(_ context.Context, id uuid.UUID) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.requests[id]
	if !ok {
		return Request{}, ErrNotFound
	}
	return s.counted(clone(r)), nil
}

func newestFirst(list []Request) {
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		}
		return list[i].ID.String() > list[j].ID.String()
	})
}

// Requests implements Store.
func (s *MemoryStore) Requests(_ context.Context, book tenancies.Book, f Filter) ([]Request, int, map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	var out []Request
	q := strings.ToLower(f.Q)
	for _, r := range s.requests {
		if !r.Book.Same(book) || (f.Category != "" && r.Category != f.Category) || (f.Priority != "" && r.Priority != f.Priority) ||
			(f.PropertyID != nil && r.PropertyID != *f.PropertyID) || (f.TenancyID != nil && (r.TenancyID == nil || *r.TenancyID != *f.TenancyID)) ||
			(q != "" && !strings.Contains(strings.ToLower(r.Reference), q) && !strings.Contains(strings.ToLower(r.Title), q)) {
			continue
		}
		counts[r.Status]++
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		out = append(out, s.counted(clone(r)))
	}
	newestFirst(out)
	total := len(out)
	if f.Limit > 0 {
		lo := min(f.Offset, total)
		out = out[lo:min(lo+f.Limit, total)]
	}
	return out, total, counts, nil
}

// RequestsOfTenancy implements Store.
func (s *MemoryStore) RequestsOfTenancy(_ context.Context, tenancyID uuid.UUID) ([]Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Request
	for _, r := range s.requests {
		if r.TenancyID != nil && *r.TenancyID == tenancyID {
			out = append(out, s.counted(clone(r)))
		}
	}
	newestFirst(out)
	return out, nil
}

// Events implements Store.
func (s *MemoryStore) Events(_ context.Context, requestID uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Event{}
	for _, e := range s.events {
		if e.RequestID == requestID {
			e.Details = maps.Clone(e.Details)
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Photos implements Store.
func (s *MemoryStore) Photos(_ context.Context, requestID uuid.UUID) ([]Photo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Photo{}
	for _, p := range s.photos {
		if p.RequestID == requestID {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UploadedAt.Equal(out[j].UploadedAt) {
			return out[i].UploadedAt.Before(out[j].UploadedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// Costs implements Store.
func (s *MemoryStore) Costs(_ context.Context, ids []uuid.UUID) ([]Cost, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Cost{}
	for _, c := range s.costs {
		if slices.Contains(ids, c.RequestID) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].RecordedAt.Equal(out[j].RecordedAt) {
			return out[i].RecordedAt.After(out[j].RecordedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out, nil
}

// Apply implements Store. A Change with a Ledger part is applied through
// the ledger store's ApplyWith: both are kept, or neither.
func (s *MemoryStore) Apply(ctx context.Context, ch Change) error {
	if ch.Ledger != nil {
		s.mu.Lock()
		l := s.ledger
		s.mu.Unlock()
		if l == nil {
			return ErrNoLedger
		}
		return l.ApplyWith(ctx, *ch.Ledger, func() error { return s.apply(ch) })
	}
	return s.apply(ch)
}

// Put stores a Change without its ledger part, audit or outbox (the seed).
func (s *MemoryStore) Put(ch Change) error {
	ch.Ledger, ch.Audit, ch.Notices = nil, nil, nil
	return s.apply(ch)
}

func (s *MemoryStore) apply(ch Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, want := range ch.Expect {
		r, ok := s.requests[id]
		if !ok || r.Status != want.Status || r.Assignments != want.Assignments {
			return ErrConflict
		}
	}
	if ch.Create != nil {
		if _, ok := s.requests[ch.Create.ID]; ok {
			return fmt.Errorf("maintenance request %s exists", ch.Create.ID)
		}
		for _, r := range s.requests {
			if r.Reference == ch.Create.Reference {
				return fmt.Errorf("maintenance reference %s is taken", r.Reference)
			}
		}
	}
	for _, r := range ch.Requests {
		if _, ok := s.requests[r.ID]; !ok {
			return ErrNotFound
		}
	}
	for id := range ch.Touch {
		if _, ok := s.requests[id]; !ok {
			return ErrNotFound
		}
	}
	added := map[uuid.UUID]int{}
	for _, p := range ch.Photos {
		added[p.RequestID]++
	}
	for id, n := range added {
		have := 0
		for _, p := range s.photos {
			if p.RequestID == id && !slices.Contains(ch.DeletePhotos, p.ID) {
				have++
			}
		}
		if have+n > MaxPhotos {
			return ErrPhotoLimit
		}
	}
	if err := s.failNext; err != nil {
		s.failNext = nil
		return err
	}
	if ch.Create != nil {
		s.requests[ch.Create.ID] = clone(*ch.Create)
	}
	for _, r := range ch.Requests {
		cur := s.requests[r.ID]
		r.Reference, r.Book, r.CreatedAt = cur.Reference, cur.Book, cur.CreatedAt
		s.requests[r.ID] = clone(r)
	}
	for id, at := range ch.Touch {
		r := s.requests[id]
		r.UpdatedAt = at
		s.requests[id] = r
	}
	for _, id := range ch.DeletePhotos {
		delete(s.photos, id)
	}
	for _, p := range ch.Photos {
		s.photos[p.ID] = p
	}
	for _, c := range ch.Costs {
		s.costs[c.ID] = c
	}
	for _, e := range ch.Events {
		e.Details = maps.Clone(e.Details)
		s.events = append(s.events, e)
	}
	if s.audit != nil {
		for _, e := range ch.Audit {
			s.audit(e)
		}
	}
	for _, n := range ch.Notices {
		payload, err := json.Marshal(n.Payload)
		if err != nil {
			return err
		}
		ev := outbox.Event{ID: uuid.New(), Type: n.Type, AggregateType: AggregateMaintenance, AggregateID: n.Payload.RequestID,
			Payload: payload, Attempts: 1, CreatedAt: n.Payload.OccurredAt}
		s.outbox = append(s.outbox, ev)
		if s.sink != nil {
			s.sink(ev)
		}
	}
	return nil
}

// ForgetUser handles a deleted account's rows (the users row is
// anonymised, so nothing cascades): the person is unlinked everywhere, and
// where they acted as a tenant their name is replaced with deletedName
// (managers' names stay, as the ledger's author names do).
func (s *MemoryStore) ForgetUser(userID uuid.UUID, deletedName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	is := func(id *uuid.UUID) bool { return id != nil && *id == userID }
	for id, r := range s.requests {
		if is(r.RaisedBy) {
			r.RaisedBy = nil
			if r.RaisedByKind == ActorTenant {
				r.RaisedByName = deletedName
			}
		}
		if is(r.AssignedBy) {
			r.AssignedBy = nil
		}
		s.requests[id] = r
	}
	for i, e := range s.events {
		if is(e.ActorUserID) {
			e.ActorUserID = nil
			if e.ActorKind == ActorTenant {
				e.ActorName = deletedName
			}
			s.events[i] = e
		}
	}
	for id, p := range s.photos {
		if is(p.UploadedBy) {
			p.UploadedBy = nil
			if p.UploaderKind == ActorTenant {
				p.UploadedByName = deletedName
			}
			s.photos[id] = p
		}
	}
	for id, c := range s.costs {
		if is(c.RecordedBy) {
			c.RecordedBy = nil
			s.costs[id] = c
		}
	}
}

// ConnectLedger connects a maintenance store to the ledger's store of the
// same kind: a memory store posts a cost's entry through the ledger memory
// store's ApplyWith; a Postgres store uses ledger.ApplyTx in its own
// transaction and needs nothing.
func ConnectLedger(store Store, ledgerStore ledger.Store) error {
	if m, ok := store.(*MemoryStore); ok {
		l, ok := ledgerStore.(*ledger.MemoryStore)
		if !ok {
			return fmt.Errorf("maintenance: a memory store needs the ledger's memory store")
		}
		m.SetLedger(l)
	}
	return nil
}
