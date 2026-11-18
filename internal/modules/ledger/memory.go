package ledger

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/money"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// MemoryStore keeps books in memory (development and tests). It mirrors the
// Postgres store, including its guards: nothing posted is ever changed and
// nothing is ever deleted.
type MemoryStore struct {
	mu          sync.Mutex
	entries     map[uuid.UUID]Entry
	order       []uuid.UUID
	notes       []Note
	attachments []Attachment
	categories  []Category
	audit       orgaudit.Sink
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{entries: map[uuid.UUID]Entry{}} }

// SetAuditSink receives the organisation audit records this store writes.
func (s *MemoryStore) SetAuditSink(sink orgaudit.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = sink
}

func (s *MemoryStore) counted(e Entry) Entry {
	e.NoteCount, e.AttachmentCount = 0, 0
	for _, n := range s.notes {
		if n.EntryID == e.ID {
			e.NoteCount++
		}
	}
	for _, a := range s.attachments {
		if a.EntryID == e.ID {
			e.AttachmentCount++
		}
	}
	return e
}

// Entry implements Store.
func (s *MemoryStore) Entry(_ context.Context, id uuid.UUID) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return Entry{}, ErrEntryNotFound
	}
	return s.counted(e), nil
}

func matches(e Entry, f Filter) bool {
	switch {
	case f.Currency != "" && e.Currency != f.Currency,
		f.Type != "" && e.Type != f.Type,
		f.Category != "" && e.Category != f.Category,
		f.Method != "" && e.Method != f.Method,
		f.Status != "" && e.Status != f.Status,
		f.From != "" && e.EffectiveDate < f.From,
		f.To != "" && e.EffectiveDate > f.To,
		f.Branch != nil && !strings.EqualFold(e.Branch, *f.Branch),
		f.ContactID != nil && (e.ContactID == nil || *e.ContactID != *f.ContactID),
		f.ListingID != nil && (e.ListingID == nil || *e.ListingID != *f.ListingID),
		f.LeaseID != nil && (e.LeaseID == nil || *e.LeaseID != *f.LeaseID),
		f.ExcludeReversed && (e.ReversedBy != nil || e.Type == TypeReversal):
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Q)); q != "" {
		return strings.Contains(strings.ToLower(e.Description), q) || strings.Contains(strings.ToLower(e.Reference), q)
	}
	return true
}

// sortEntries orders by effective date, then recorded_at, then id.
func sortEntries(list []Entry, ascending bool) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		less := func() bool {
			if a.EffectiveDate != b.EffectiveDate {
				return a.EffectiveDate < b.EffectiveDate
			}
			if !a.RecordedAt.Equal(b.RecordedAt) {
				return a.RecordedAt.Before(b.RecordedAt)
			}
			return a.ID.String() < b.ID.String()
		}
		if ascending {
			return less()
		}
		if a.EffectiveDate == b.EffectiveDate && a.RecordedAt.Equal(b.RecordedAt) && a.ID == b.ID {
			return false
		}
		return !less()
	})
}

// Entries implements Store.
func (s *MemoryStore) Entries(_ context.Context, book Book, f Filter) ([]Entry, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, id := range s.order {
		e := s.entries[id]
		if e.Book.Same(book) && matches(e, f) {
			out = append(out, s.counted(e))
		}
	}
	sortEntries(out, f.Ascending)
	total := len(out)
	start := min(max(f.Offset, 0), total)
	end := total
	if f.Limit > 0 {
		end = min(start+f.Limit, total)
	}
	return slices.Clone(out[start:end]), total, nil
}

// Related implements Store.
func (s *MemoryStore) Related(_ context.Context, book Book, id uuid.UUID) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, eid := range s.order {
		e := s.entries[eid]
		if !e.Book.Same(book) || e.ID == id {
			continue
		}
		for _, p := range []*uuid.UUID{e.ReversalOf, e.CorrectionOf, e.TransferPair, e.Settles} {
			if p != nil && *p == id {
				out = append(out, s.counted(e))
				break
			}
		}
	}
	sortEntries(out, true)
	return out, nil
}

// Settled implements Store.
func (s *MemoryStore) Settled(_ context.Context, book Book, ids []uuid.UUID) (map[uuid.UUID][]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settledLocked(book, ids), nil
}

func (s *MemoryStore) settledLocked(book Book, ids []uuid.UUID) map[uuid.UUID][]Entry {
	out := map[uuid.UUID][]Entry{}
	for _, eid := range s.order {
		e := s.entries[eid]
		if e.Book.Same(book) && e.Settles != nil && e.ReversedBy == nil && slices.Contains(ids, *e.Settles) {
			out[*e.Settles] = append(out[*e.Settles], e)
		}
	}
	return out
}

// OpeningBalance implements Store.
func (s *MemoryStore) OpeningBalance(_ context.Context, book Book, currency, method, branch string) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.openingLocked(book, currency, method, branch)
	return e, ok, nil
}

func (s *MemoryStore) openingLocked(book Book, currency, method, branch string) (Entry, bool) {
	for _, id := range s.order {
		e := s.entries[id]
		if e.Book.Same(book) && e.Type == TypeOpeningBalance && e.ReversedBy == nil && e.Currency == currency && e.Method == method &&
			strings.EqualFold(e.Branch, branch) {
			return e, true
		}
	}
	return Entry{}, false
}

// Branches implements Store.
func (s *MemoryStore) Branches(_ context.Context, book Book) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, id := range s.order {
		e := s.entries[id]
		if e.Book.Same(book) && e.Branch != "" && !slices.Contains(out, e.Branch) {
			out = append(out, e.Branch)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Notes implements Store.
func (s *MemoryStore) Notes(_ context.Context, entryID uuid.UUID) ([]Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Note
	for _, n := range s.notes {
		if n.EntryID == entryID {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Attachments implements Store.
func (s *MemoryStore) Attachments(_ context.Context, entryID uuid.UUID) ([]Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Attachment
	for _, a := range s.attachments {
		if a.EntryID == entryID {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UploadedAt.Before(out[j].UploadedAt) })
	return out, nil
}

// Categories implements Store.
func (s *MemoryStore) Categories(_ context.Context, book Book) ([]Category, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Category
	for _, c := range s.categories {
		if !c.Book.Same(book) {
			continue
		}
		c.Used = false
		for _, e := range s.entries {
			if e.Book.Same(book) && e.Category == c.Code {
				c.Used = true
				break
			}
		}
		out = append(out, c)
	}
	sortCategories(out)
	return out, nil
}

func sortCategories(list []Category) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].SortOrder != list[j].SortOrder {
			return list[i].SortOrder < list[j].SortOrder
		}
		return strings.ToLower(list[i].Label) < strings.ToLower(list[j].Label)
	})
}

func (s *MemoryStore) categoryIndex(book Book, code string) int {
	return slices.IndexFunc(s.categories, func(c Category) bool { return c.Book.Same(book) && c.Code == code })
}

// EnsureCategories implements Store.
func (s *MemoryStore) EnsureCategories(_ context.Context, book Book, defaults []Category) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range defaults {
		if s.categoryIndex(book, c.Code) >= 0 {
			continue
		}
		c.Book = book
		c = stamped(c)
		s.categories = append(s.categories, c)
	}
	return nil
}

// CreateCategory implements Store.
func (s *MemoryStore) CreateCategory(_ context.Context, c Category) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.categoryIndex(c.Book, c.Code) >= 0 {
		return ErrCategoryExists
	}
	s.categories = append(s.categories, c)
	return nil
}

// UpdateCategory implements Store.
func (s *MemoryStore) UpdateCategory(_ context.Context, c Category) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.categoryIndex(c.Book, c.Code)
	if i < 0 {
		return ErrCategoryNotFound
	}
	cur := s.categories[i]
	cur.Label, cur.Kind, cur.Active, cur.UpdatedAt = c.Label, c.Kind, c.Active, c.UpdatedAt
	s.categories[i] = cur
	return nil
}

// Apply implements Store. Everything is checked before anything is written.
func (s *MemoryStore) Apply(_ context.Context, ch Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make(map[uuid.UUID]Entry, len(s.entries))
	for k, v := range s.entries {
		entries[k] = v
	}
	order := slices.Clone(s.order)
	for _, id := range ch.NotReversed {
		e, ok := entries[id]
		if !ok {
			return ErrEntryNotFound
		}
		if e.ReversedBy != nil {
			return ErrAlreadyReversed
		}
	}
	insert := func(list []Entry) error {
		for _, e := range list {
			if _, ok := entries[e.ID]; ok {
				return ErrImmutable
			}
			if e.Status == StatusGatewayConfirmed {
				return ErrImmutable
			}
			if e.Type == TypeOpeningBalance {
				for _, id := range order {
					o := entries[id]
					if o.Book.Same(e.Book) && o.Type == TypeOpeningBalance && o.ReversedBy == nil && o.Currency == e.Currency &&
						o.Method == e.Method && strings.EqualFold(o.Branch, e.Branch) {
						return ErrOpeningBalanceExists
					}
				}
			}
			if e.Settles != nil {
				target, ok := entries[*e.Settles]
				if !ok || !target.Book.Same(e.Book) {
					return ErrEntryNotFound
				}
				left := money.Rat(target.Amount)
				for _, id := range order {
					o := entries[id]
					if o.Settles != nil && *o.Settles == target.ID && o.ReversedBy == nil {
						left.Sub(left, money.Rat(o.Amount))
					}
				}
				if money.Rat(e.Amount).Cmp(left) > 0 {
					return ErrOverSettled
				}
			}
			entries[e.ID] = e
			order = append(order, e.ID)
		}
		return nil
	}
	link := func(list []LinkSet) error {
		for _, l := range list {
			e, ok := entries[l.EntryID]
			if !ok {
				return ErrEntryNotFound
			}
			set := func(cur **uuid.UUID, v *uuid.UUID) error {
				if v == nil {
					return nil
				}
				if *cur != nil && **cur != *v {
					return ErrImmutable
				}
				x := *v
				*cur = &x
				return nil
			}
			if err := set(&e.ReversedBy, l.ReversedBy); err != nil {
				return err
			}
			if err := set(&e.CorrectedBy, l.CorrectedBy); err != nil {
				return err
			}
			if err := set(&e.TransferPair, l.TransferPair); err != nil {
				return err
			}
			entries[e.ID] = e
		}
		return nil
	}
	if err := insert(ch.Reversals); err != nil {
		return err
	}
	if err := link(ch.Links); err != nil {
		return err
	}
	if err := insert(ch.Entries); err != nil {
		return err
	}
	if err := link(ch.PostLinks); err != nil {
		return err
	}
	if r := ch.Reconcile; r != nil {
		e, ok := entries[r.EntryID]
		if !ok {
			return ErrEntryNotFound
		}
		e.Status = r.Status
		if r.Status == StatusReconciled {
			at := r.At
			e.ReconciledAt, e.ReconciledBy, e.ReconciledName, e.ReconcileNote = &at, r.By, r.ByName, r.Note
		} else {
			e.ReconciledAt, e.ReconciledBy, e.ReconciledName, e.ReconcileNote = nil, nil, "", ""
		}
		entries[e.ID] = e
	}
	for _, n := range ch.Notes {
		if _, ok := entries[n.EntryID]; !ok {
			return ErrEntryNotFound
		}
	}
	for _, a := range ch.Attachments {
		if _, ok := entries[a.EntryID]; !ok {
			return ErrEntryNotFound
		}
	}
	s.entries, s.order = entries, order
	s.notes = append(s.notes, ch.Notes...)
	s.attachments = append(s.attachments, ch.Attachments...)
	if s.audit != nil {
		for _, e := range ch.Audit {
			s.audit(e)
		}
	}
	return nil
}

// Put stores a Change without audit (the development seed).
func (s *MemoryStore) Put(ch Change) error {
	s.mu.Lock()
	sink := s.audit
	s.audit = nil
	s.mu.Unlock()
	err := s.Apply(context.Background(), ch)
	s.mu.Lock()
	s.audit = sink
	s.mu.Unlock()
	return err
}
