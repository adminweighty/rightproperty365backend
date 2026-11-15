package leases

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore is the in-memory Store (development and tests). Every method
// holds one lock, which stands in for the Postgres store's transactions.
type MemoryStore struct {
	mu             sync.Mutex
	templates      map[uuid.UUID]Template
	templateEvents []TemplateEvent
	leases         map[uuid.UUID]Lease
	events         []Event
	refs           map[int]int
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{templates: map[uuid.UUID]Template{}, leases: map[uuid.UUID]Lease{}, refs: map[int]int{}}
}

func copyTemplate(t Template) Template {
	t.Sections = append([]Section{}, t.Sections...)
	t.Clauses = append([]Clause{}, t.Clauses...)
	if t.LegalReview != nil {
		r := *t.LegalReview
		t.LegalReview = &r
	}
	return t
}

func copyInput(in Input) Input {
	in.Landlords = append([]Party{}, in.Landlords...)
	in.Tenants = append([]Party{}, in.Tenants...)
	in.OptionalClauses = append([]string{}, in.OptionalClauses...)
	return in
}

func copyLease(l Lease) Lease {
	l.Input = copyInput(l.Input)
	docs := make(map[string]Document, len(l.Documents))
	for k, d := range l.Documents {
		d.Snapshot = append([]byte{}, d.Snapshot...)
		docs[k] = d
	}
	l.Documents = docs
	return l
}

// CreateTemplate implements Store.
func (s *MemoryStore) CreateTemplate(_ context.Context, t Template, ev TemplateEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.templates[t.ID]; ok {
		return fmt.Errorf("template %s exists", t.ID)
	}
	s.templates[t.ID] = copyTemplate(t)
	s.templateEvents = append(s.templateEvents, ev)
	return nil
}

// PutTemplate stores a template as it is (the development seed).
func (s *MemoryStore) PutTemplate(t Template) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.templates[t.ID] = copyTemplate(t)
}

// Template implements Store.
func (s *MemoryStore) Template(_ context.Context, id uuid.UUID) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.templates[id]
	if !ok {
		return Template{}, ErrTemplateNotFound
	}
	return copyTemplate(t), nil
}

// Templates implements Store.
func (s *MemoryStore) Templates(_ context.Context, f TemplateFilter) ([]Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Template{}
	for _, t := range s.templates {
		if (f.MarketCode == "" || t.MarketCode == f.MarketCode) && (f.Status == "" || t.Status == f.Status) &&
			(f.FamilyID == nil || t.FamilyID == *f.FamilyID) {
			out = append(out, copyTemplate(t))
		}
	}
	sortTemplates(out)
	return out, nil
}

func sortTemplates(list []Template) {
	slices.SortStableFunc(list, func(a, b Template) int {
		if a.MarketCode != b.MarketCode {
			if a.MarketCode < b.MarketCode {
				return -1
			}
			return 1
		}
		if a.Name != b.Name {
			if a.Name < b.Name {
				return -1
			}
			return 1
		}
		return b.Version - a.Version
	})
}

// ChangeTemplate implements Store.
func (s *MemoryStore) ChangeTemplate(_ context.Context, id uuid.UUID, change func(t Template) (Template, []TemplateEvent, error)) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.templates[id]
	if !ok {
		return Template{}, ErrTemplateNotFound
	}
	next, events, err := change(copyTemplate(t))
	if err != nil {
		return Template{}, err
	}
	next.ID = id
	s.templates[id] = copyTemplate(next)
	s.templateEvents = append(s.templateEvents, events...)
	return copyTemplate(next), nil
}

// CreateTemplateVersion implements Store.
func (s *MemoryStore) CreateTemplateVersion(_ context.Context, t Template, ev TemplateEvent) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	highest := 0
	for _, other := range s.templates {
		if other.FamilyID != t.FamilyID {
			continue
		}
		if other.Status == TemplateDraft {
			return Template{}, ErrTemplateDraftExists
		}
		highest = max(highest, other.Version)
	}
	t.Version = highest + 1
	s.templates[t.ID] = copyTemplate(t)
	ev.TemplateID = t.ID
	if ev.Detail == nil {
		ev.Detail = map[string]any{}
	}
	ev.Detail["version"] = t.Version
	s.templateEvents = append(s.templateEvents, ev)
	return copyTemplate(t), nil
}

// ApproveTemplate implements Store.
func (s *MemoryStore) ApproveTemplate(_ context.Context, id uuid.UUID, review LegalReview, by *uuid.UUID, byName string, at time.Time,
	events func(t Template, retired []Template) []TemplateEvent) (Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.templates[id]
	if !ok {
		return Template{}, ErrTemplateNotFound
	}
	if t.Status != TemplateDraft {
		return Template{}, ErrTemplateState
	}
	var retired []Template
	for oid, other := range s.templates {
		if other.FamilyID == t.FamilyID && other.Status == TemplateApproved {
			other.Status, other.RetiredAt, other.UpdatedAt = TemplateRetired, &at, at
			s.templates[oid] = other
			retired = append(retired, other)
		}
	}
	r := review
	t.Status, t.LegalReview, t.ApprovedAt, t.ApprovedBy, t.ApprovedByName, t.UpdatedAt = TemplateApproved, &r, &at, by, byName, at
	s.templates[id] = t
	s.templateEvents = append(s.templateEvents, events(copyTemplate(t), retired)...)
	return copyTemplate(t), nil
}

// TemplateEvents implements Store.
func (s *MemoryStore) TemplateEvents(_ context.Context, templateID uuid.UUID) ([]TemplateEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []TemplateEvent{}
	for _, e := range s.templateEvents {
		if e.TemplateID == templateID {
			out = append(out, e)
		}
	}
	return out, nil
}

// NextReference implements Store.
func (s *MemoryStore) NextReference(_ context.Context, year int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[year]++
	return fmt.Sprintf("%s-%d-%06d", ReferencePrefix, year, s.refs[year]), nil
}

// CreateLease implements Store.
func (s *MemoryStore) CreateLease(_ context.Context, l Lease, events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, other := range s.leases {
		if other.Reference == l.Reference {
			return ErrReferenceTaken
		}
	}
	s.leases[l.ID] = copyLease(l)
	s.events = append(s.events, events...)
	return nil
}

// PutLease stores a lease as it is (the development seed), with its events.
func (s *MemoryStore) PutLease(l Lease, events ...Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases[l.ID] = copyLease(l)
	s.events = append(s.events, events...)
}

// Lease implements Store.
func (s *MemoryStore) Lease(_ context.Context, id uuid.UUID) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.leases[id]
	if !ok {
		return Lease{}, ErrLeaseNotFound
	}
	out := copyLease(l)
	return out, nil
}

// matches reports whether l is in the filter's scope.
func (f LeaseFilter) matches(l Lease) bool {
	if f.Status != "" && l.Status != f.Status {
		return false
	}
	if f.All {
		return true
	}
	if l.OrganisationID != nil && slices.Contains(f.Organisations, *l.OrganisationID) {
		return true
	}
	if f.Creator == nil || l.CreatedBy != *f.Creator {
		return false
	}
	if l.OrganisationID == nil {
		return f.CreatorDirect
	}
	return slices.Contains(f.CreatorOrganisations, *l.OrganisationID)
}

// Leases implements Store.
func (s *MemoryStore) Leases(_ context.Context, f LeaseFilter) ([]Lease, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []Lease
	for _, l := range s.leases {
		if f.matches(l) {
			c := copyLease(l)
			all = append(all, c)
		}
	}
	slices.SortStableFunc(all, func(a, b Lease) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return bytes.Compare(b.ID[:], a.ID[:])
	})
	total := len(all)
	if f.Limit > 0 {
		start := min(f.Offset, total)
		end := min(start+f.Limit, total)
		all = all[start:end]
	}
	if all == nil {
		all = []Lease{}
	}
	return all, total, nil
}

// ChangeLease implements Store.
func (s *MemoryStore) ChangeLease(_ context.Context, id uuid.UUID, change func(l *Lease) ([]Event, error)) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.leases[id]
	if !ok {
		return Lease{}, ErrLeaseNotFound
	}
	next := copyLease(l)
	events, err := change(&next)
	if err != nil {
		return Lease{}, err
	}
	next.ID = id
	s.leases[id] = copyLease(next)
	s.events = append(s.events, events...)
	out := copyLease(next)
	return out, nil
}

// DeleteLease implements Store.
func (s *MemoryStore) DeleteLease(_ context.Context, id uuid.UUID) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.leases[id]
	if !ok {
		return Lease{}, ErrLeaseNotFound
	}
	if l.Status != StatusDraft {
		return Lease{}, ErrLeaseNotEditable
	}
	delete(s.leases, id)
	kept := s.events[:0]
	for _, e := range s.events {
		if e.LeaseID != id {
			kept = append(kept, e)
		}
	}
	s.events = kept
	return l, nil
}

// Events implements Store.
func (s *MemoryStore) Events(_ context.Context, leaseID uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Event{}
	for _, e := range s.events {
		if e.LeaseID == leaseID {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b Event) int { return a.At.Compare(b.At) })
	return out, nil
}
