package tenancies

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// MemoryStore keeps properties and tenancies in memory (development and
// tests). It mirrors the Postgres store's guards: one ACTIVE tenancy per
// unit, unique live unit labels, nothing in use archived.
type MemoryStore struct {
	mu         sync.Mutex
	properties map[uuid.UUID]Property
	units      map[uuid.UUID]Unit
	tenancies  map[uuid.UUID]Tenancy
	dues       map[uuid.UUID]Due
	receipts   map[uuid.UUID]Receipt
	audit      orgaudit.Sink
	// ledger posts a rent receipt's entry in the same "transaction" (ConnectLedger).
	ledger LedgerMemory
	// failNext, when set, fails the next Apply after its checks (tests).
	failNext error
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{properties: map[uuid.UUID]Property{}, units: map[uuid.UUID]Unit{}, tenancies: map[uuid.UUID]Tenancy{},
		dues: map[uuid.UUID]Due{}, receipts: map[uuid.UUID]Receipt{}}
}

// SetAuditSink receives the organisation audit records this store writes.
func (s *MemoryStore) SetAuditSink(sink orgaudit.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = sink
}

func (s *MemoryStore) withUnits(p Property) Property {
	p.Units = []Unit{}
	for _, u := range s.units {
		if u.PropertyID == p.ID {
			p.Units = append(p.Units, u)
		}
	}
	sortUnits(p.Units)
	return p
}

func sortUnits(list []Unit) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Position != list[j].Position {
			return list[i].Position < list[j].Position
		}
		return list[i].ID.String() < list[j].ID.String()
	})
}

func newestFirst[T any](list []T, at func(T) time.Time, id func(T) uuid.UUID) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := at(list[i]), at(list[j])
		if !a.Equal(b) {
			return a.After(b)
		}
		return id(list[i]).String() > id(list[j]).String()
	})
}

func page[T any](list []T, limit, offset int) []T {
	start := min(max(offset, 0), len(list))
	end := len(list)
	if limit > 0 {
		end = min(start+limit, len(list))
	}
	return slices.Clone(list[start:end])
}

// Property implements Store.
func (s *MemoryStore) Property(_ context.Context, id uuid.UUID) (Property, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.properties[id]
	if !ok {
		return Property{}, ErrPropertyNotFound
	}
	return s.withUnits(p), nil
}

// Properties implements Store.
func (s *MemoryStore) Properties(_ context.Context, book Book, f PropertyFilter) ([]Property, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(f.Q))
	var out []Property
	for _, p := range s.properties {
		switch {
		case !p.Book.Same(book) || p.DeletedAt != nil,
			f.MarketCode != "" && p.MarketCode != f.MarketCode,
			q != "" && !strings.Contains(strings.ToLower(p.Name), q) && !strings.Contains(strings.ToLower(p.Address), q):
			continue
		}
		out = append(out, s.withUnits(p))
	}
	newestFirst(out, func(p Property) time.Time { return p.CreatedAt }, func(p Property) uuid.UUID { return p.ID })
	return page(out, f.Limit, f.Offset), len(out), nil
}

// PropertiesByIDs implements Store.
func (s *MemoryStore) PropertiesByIDs(_ context.Context, ids []uuid.UUID) ([]Property, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Property
	for _, id := range ids {
		if p, ok := s.properties[id]; ok {
			out = append(out, s.withUnits(p))
		}
	}
	return out, nil
}

func (s *MemoryStore) uses(ids []uuid.UUID) []Use {
	var out []Use
	for _, t := range s.tenancies {
		if Open(t.Status) && slices.Contains(ids, t.PropertyID) {
			out = append(out, Use{TenancyID: t.ID, PropertyID: t.PropertyID, UnitID: t.UnitID, Status: t.Status})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TenancyID.String() < out[j].TenancyID.String() })
	return out
}

// Uses implements Store.
func (s *MemoryStore) Uses(_ context.Context, ids []uuid.UUID) ([]Use, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uses(ids), nil
}

func cloneTenancy(t Tenancy) Tenancy {
	t.Parties = slices.Clone(t.Parties)
	return t
}

// Tenancy implements Store.
func (s *MemoryStore) Tenancy(_ context.Context, id uuid.UUID) (Tenancy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tenancies[id]
	if !ok {
		return Tenancy{}, ErrTenancyNotFound
	}
	return cloneTenancy(t), nil
}

func matchesTenancy(t Tenancy, f TenancyFilter) bool {
	switch {
	case f.PropertyID != nil && t.PropertyID != *f.PropertyID,
		f.UnitID != nil && (t.UnitID == nil || *t.UnitID != *f.UnitID):
		return false
	case f.ContactID != nil:
		return slices.ContainsFunc(t.Parties, func(p Party) bool { return p.ContactID == *f.ContactID })
	}
	return true
}

// Tenancies implements Store.
func (s *MemoryStore) Tenancies(_ context.Context, book Book, f TenancyFilter) ([]Tenancy, int, map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	var out []Tenancy
	for _, t := range s.tenancies {
		if !t.Book.Same(book) || !matchesTenancy(t, f) {
			continue
		}
		counts[t.Status]++
		if f.Status == "" || t.Status == f.Status {
			out = append(out, cloneTenancy(t))
		}
	}
	newestFirst(out, func(t Tenancy) time.Time { return t.CreatedAt }, func(t Tenancy) uuid.UUID { return t.ID })
	return page(out, f.Limit, f.Offset), len(out), counts, nil
}

// TenanciesOfTenants implements Store.
func (s *MemoryStore) TenanciesOfTenants(_ context.Context, contactIDs []uuid.UUID) ([]Tenancy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Tenancy
	for _, t := range s.tenancies {
		if t.Status == StatusDraft {
			continue
		}
		if slices.ContainsFunc(contactIDs, t.HasTenant) {
			out = append(out, cloneTenancy(t))
		}
	}
	newestFirst(out, func(t Tenancy) time.Time { return t.CreatedAt }, func(t Tenancy) uuid.UUID { return t.ID })
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

func (s *MemoryStore) apply(ch Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ch); err != nil {
		return err
	}
	if err := s.checkRent(ch); err != nil {
		return err
	}
	if err := s.failNext; err != nil {
		s.failNext = nil
		return err
	}
	for _, p := range ch.Properties {
		p.Units = nil
		s.properties[p.ID] = p
	}
	for _, u := range ch.Units {
		s.units[u.ID] = u
	}
	for _, id := range ch.DeleteTenancies {
		delete(s.tenancies, id)
	}
	for _, t := range ch.Tenancies {
		s.tenancies[t.ID] = cloneTenancy(t)
	}
	s.applyRent(ch)
	if s.audit != nil {
		for _, e := range ch.Audit {
			s.audit(e)
		}
	}
	return nil
}

// check verifies a Change against the current state, as Postgres would.
func (s *MemoryStore) check(ch Change) error {
	for id, status := range ch.ExpectStatus {
		t, ok := s.tenancies[id]
		if !ok || t.Status != status {
			return ErrConflict
		}
	}
	for _, id := range ch.PropertyFree {
		if len(s.uses([]uuid.UUID{id})) > 0 {
			return ErrPropertyInUse
		}
	}
	for _, id := range ch.UnitFree {
		for _, t := range s.tenancies {
			if Open(t.Status) && t.UnitID != nil && *t.UnitID == id {
				return ErrUnitInUse
			}
		}
	}
	// Unit labels after the change.
	units := map[uuid.UUID]Unit{}
	for id, u := range s.units {
		units[id] = u
	}
	for _, u := range ch.Units {
		units[u.ID] = u
	}
	seen := map[string]bool{}
	for _, u := range units {
		if u.DeletedAt != nil {
			continue
		}
		key := u.PropertyID.String() + "/" + strings.ToLower(u.Label)
		if seen[key] {
			return ErrUnitLabelTaken
		}
		seen[key] = true
	}
	// Tenancies after the change.
	props := map[uuid.UUID]Property{}
	for id, p := range s.properties {
		props[id] = p
	}
	for _, p := range ch.Properties {
		props[p.ID] = p
	}
	after := map[uuid.UUID]Tenancy{}
	for id, t := range s.tenancies {
		after[id] = t
	}
	for _, id := range ch.DeleteTenancies {
		delete(after, id)
	}
	for _, t := range ch.Tenancies {
		p, ok := props[t.PropertyID]
		if !ok || (p.DeletedAt != nil && Open(t.Status)) {
			return ErrPropertyNotFound
		}
		if t.UnitID != nil {
			if u, ok := units[*t.UnitID]; !ok || u.PropertyID != t.PropertyID {
				return ErrUnitNotFound
			}
		}
		after[t.ID] = t
	}
	active := map[string]bool{}
	for _, t := range after {
		if t.Status != StatusActive {
			continue
		}
		key := t.PropertyID.String() + "/"
		if t.UnitID != nil {
			key += t.UnitID.String()
		}
		if active[key] {
			return ErrUnitOccupied
		}
		active[key] = true
	}
	return nil
}

// Put stores a Change without checks or audit (the development seed).
func (s *MemoryStore) Put(ch Change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range ch.Properties {
		p.Units = nil
		s.properties[p.ID] = p
	}
	for _, u := range ch.Units {
		s.units[u.ID] = u
	}
	for _, t := range ch.Tenancies {
		s.tenancies[t.ID] = cloneTenancy(t)
	}
	s.applyRent(ch)
}

// ForgetUser handles a deleted account's rows (the users row is
// anonymised, so nothing cascades): the direct owner's DRAFT tenancies are
// deleted, the manager name shown to tenants of their personal book is
// replaced, and the person is unlinked as creator, activator and ender.
func (s *MemoryStore) ForgetUser(userID uuid.UUID, deletedName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	is := func(id *uuid.UUID) bool { return id != nil && *id == userID }
	for id, t := range s.tenancies {
		personal := t.Book.UserID != nil && *t.Book.UserID == userID
		if personal && t.Status == StatusDraft {
			delete(s.tenancies, id)
			continue
		}
		if personal {
			t.BookName = deletedName
		}
		if is(t.CreatedBy) {
			t.CreatedBy = nil
		}
		if is(t.ActivatedBy) {
			t.ActivatedBy = nil
		}
		if is(t.EndedBy) {
			t.EndedBy = nil
		}
		s.tenancies[id] = t
	}
	for id, p := range s.properties {
		if is(p.CreatedBy) {
			p.CreatedBy = nil
			s.properties[id] = p
		}
	}
	for id, d := range s.dues {
		if is(d.WaivedBy) {
			d.WaivedBy = nil
			s.dues[id] = d
		}
	}
	for id, r := range s.receipts {
		if is(r.RecordedBy) {
			r.RecordedBy = nil
			s.receipts[id] = r
		}
	}
}
