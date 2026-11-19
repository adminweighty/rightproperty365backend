package inspections

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore keeps inspections in memory (development and tests). It
// mirrors the Postgres store's guards.
type MemoryStore struct {
	mu          sync.Mutex
	inspections map[uuid.UUID]Inspection
	photos      map[uuid.UUID]Photo
	refs        map[int]int
	outbox      []outbox.Event
	sink        outbox.Sink
	audit       orgaudit.Sink
	// failNext, when set, fails the next Apply after its checks (tests).
	failNext error
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{inspections: map[uuid.UUID]Inspection{}, photos: map[uuid.UUID]Photo{}, refs: map[int]int{}}
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

func clone(in Inspection) Inspection {
	in.Checklist = in.Checklist.Clone()
	return in
}

func (s *MemoryStore) counted(in Inspection) Inspection {
	in = clone(in)
	in.PhotoCount = 0
	for _, p := range s.photos {
		if p.InspectionID == in.ID {
			in.PhotoCount++
		}
	}
	return in
}

// Inspection implements Store.
func (s *MemoryStore) Inspection(_ context.Context, id uuid.UUID) (Inspection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.inspections[id]
	if !ok {
		return Inspection{}, ErrNotFound
	}
	return s.counted(in), nil
}

func latestFirst(list []Inspection) {
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].ScheduledFor.Equal(list[j].ScheduledFor) {
			return list[i].ScheduledFor.After(list[j].ScheduledFor)
		}
		return list[i].ID.String() > list[j].ID.String()
	})
}

// Inspections implements Store.
func (s *MemoryStore) Inspections(_ context.Context, book tenancies.Book, f Filter) ([]Inspection, int, map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	var out []Inspection
	for _, in := range s.inspections {
		day := in.ScheduledFor.UTC().Format(time.DateOnly)
		if !in.Book.Same(book) || (f.Type != "" && in.Type != f.Type) || (f.PropertyID != nil && in.PropertyID != *f.PropertyID) ||
			(f.TenancyID != nil && (in.TenancyID == nil || *in.TenancyID != *f.TenancyID)) ||
			(f.From != "" && day < f.From) || (f.To != "" && day > f.To) {
			continue
		}
		counts[in.Status]++
		if f.Status != "" && in.Status != f.Status {
			continue
		}
		out = append(out, s.counted(in))
	}
	latestFirst(out)
	total := len(out)
	if f.Limit > 0 {
		lo := min(f.Offset, total)
		out = out[lo:min(lo+f.Limit, total)]
	}
	return out, total, counts, nil
}

// InspectionsOfTenancy implements Store.
func (s *MemoryStore) InspectionsOfTenancy(_ context.Context, tenancyID uuid.UUID) ([]Inspection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Inspection
	for _, in := range s.inspections {
		if in.TenancyID != nil && *in.TenancyID == tenancyID {
			out = append(out, s.counted(in))
		}
	}
	latestFirst(out)
	return out, nil
}

// Photos implements Store.
func (s *MemoryStore) Photos(_ context.Context, inspectionID uuid.UUID) ([]Photo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Photo{}
	for _, p := range s.photos {
		if p.InspectionID == inspectionID {
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

// Apply implements Store.
func (s *MemoryStore) Apply(_ context.Context, ch Change) error {
	return s.apply(ch)
}

// Put stores a Change without its audit or outbox (the seed).
func (s *MemoryStore) Put(ch Change) error {
	ch.Audit, ch.Notices = nil, nil
	return s.apply(ch)
}

func (s *MemoryStore) apply(ch Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, want := range ch.Expect {
		in, ok := s.inspections[id]
		if !ok || in.Status != want.Status || (!want.AnyVersion && in.Version != want.Version) {
			return ErrConflict
		}
	}
	if ch.Create != nil {
		if _, ok := s.inspections[ch.Create.ID]; ok {
			return fmt.Errorf("inspection %s exists", ch.Create.ID)
		}
		for _, in := range s.inspections {
			if in.Reference == ch.Create.Reference {
				return fmt.Errorf("inspection reference %s is taken", in.Reference)
			}
		}
	}
	for _, in := range ch.Inspections {
		if _, ok := s.inspections[in.ID]; !ok {
			return ErrNotFound
		}
	}
	for id := range ch.Touch {
		if _, ok := s.inspections[id]; !ok {
			return ErrNotFound
		}
	}
	perItem, perInspection := map[uuid.UUID]int{}, map[uuid.UUID]int{}
	for _, p := range s.photos {
		if !slices.Contains(ch.DeletePhotos, p.ID) {
			perItem[p.ItemID]++
			perInspection[p.InspectionID]++
		}
	}
	for _, p := range ch.Photos {
		if _, ok := s.inspections[p.InspectionID]; !ok {
			return ErrNotFound
		}
		perItem[p.ItemID]++
		perInspection[p.InspectionID]++
		if perItem[p.ItemID] > MaxPhotosPerItem || perInspection[p.InspectionID] > MaxPhotos {
			return ErrPhotoLimit
		}
	}
	if err := s.failNext; err != nil {
		s.failNext = nil
		return err
	}
	if ch.Create != nil {
		s.inspections[ch.Create.ID] = clone(*ch.Create)
	}
	for _, in := range ch.Inspections {
		cur := s.inspections[in.ID]
		in.Reference, in.Book, in.CreatedAt, in.CreatedBy, in.CreatedByName = cur.Reference, cur.Book, cur.CreatedAt, cur.CreatedBy,
			cur.CreatedByName
		s.inspections[in.ID] = clone(in)
	}
	for id, at := range ch.Touch {
		in := s.inspections[id]
		in.UpdatedAt = at
		s.inspections[id] = in
	}
	for _, id := range ch.DeletePhotos {
		delete(s.photos, id)
	}
	for _, p := range ch.Photos {
		s.photos[p.ID] = p
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
		ev := outbox.Event{ID: uuid.New(), Type: n.Type, AggregateType: AggregateInspection, AggregateID: n.Payload.InspectionID,
			Payload: payload, Attempts: 1, CreatedAt: n.Payload.OccurredAt}
		s.outbox = append(s.outbox, ev)
		if s.sink != nil {
			s.sink(ev)
		}
	}
	return nil
}

// ForgetUser handles a deleted account's rows (the users row is
// anonymised, so nothing cascades): the person is unlinked as inspector,
// creator, completer, canceller and uploader; the names stay (they acted
// for the book, as the ledger's author names do).
func (s *MemoryStore) ForgetUser(userID uuid.UUID, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	is := func(id *uuid.UUID) bool { return id != nil && *id == userID }
	for id, in := range s.inspections {
		for _, p := range []**uuid.UUID{&in.InspectorID, &in.CreatedBy, &in.CompletedBy, &in.CancelledBy} {
			if is(*p) {
				*p = nil
			}
		}
		s.inspections[id] = in
	}
	for id, p := range s.photos {
		if is(p.UploadedBy) {
			p.UploadedBy = nil
			s.photos[id] = p
		}
	}
}
