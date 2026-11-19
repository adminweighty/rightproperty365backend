package crm

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore keeps contact books in memory (development and tests). It
// mirrors the Postgres store, including the tombstones of merges.
type MemoryStore struct {
	mu        sync.Mutex
	contacts  map[uuid.UUID]Contact
	notes     []Note
	reminders []Reminder
	events    []Event
	links     []Link
	audit     orgaudit.Sink
	sink      outbox.Sink
	outbox    []outbox.Event
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{contacts: map[uuid.UUID]Contact{}} }

// SetAuditSink receives the organisation audit records this store writes
// (the team store's log, so that GET .../audit shows them).
func (s *MemoryStore) SetAuditSink(sink orgaudit.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = sink
}

// SetOutboxSink receives the outbox events this store writes.
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// Outbox returns the events written (test helper).
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.outbox)
}

func clone(c Contact) Contact {
	c.Emails, c.Phones, c.Tags = slices.Clone(c.Emails), slices.Clone(c.Phones), slices.Clone(c.Tags)
	if c.Marketing != nil {
		m := *c.Marketing
		c.Marketing = &m
	}
	return c
}

// withReminders fills the computed fields (DueReminders only when now is set).
func (s *MemoryStore) withReminders(c Contact, now ...time.Time) Contact {
	c = clone(c)
	c.OpenReminders, c.NextReminderAt, c.DueReminders, c.ListingIDs = 0, nil, 0, nil
	for _, l := range s.links {
		if l.ContactID == c.ID && l.Kind == KindListing {
			c.ListingIDs = append(c.ListingIDs, l.ItemID)
		}
	}
	for _, r := range s.reminders {
		if r.ContactID == c.ID && !r.Done {
			c.OpenReminders++
			if len(now) > 0 && !now[0].IsZero() && !r.DueAt.After(now[0]) {
				c.DueReminders++
			}
			if c.NextReminderAt == nil || r.DueAt.Before(*c.NextReminderAt) {
				due := r.DueAt
				c.NextReminderAt = &due
			}
		}
	}
	return c
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Contact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.contacts[id]
	if !ok {
		return Contact{}, ErrNotFound
	}
	return s.withReminders(c), nil
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// matches reports whether a contact passes a filter (List and Postgres agree).
func (s *MemoryStore) matches(c Contact, f ListFilter) bool {
	if q := strings.ToLower(strings.TrimSpace(f.Q)); q != "" {
		hit := strings.Contains(strings.ToLower(c.Name), q) || slices.ContainsFunc(c.Emails, func(e string) bool { return strings.Contains(e, q) })
		if d := strings.TrimPrefix(digits(q), "0"); !hit && len(d) >= 3 {
			hit = slices.ContainsFunc(c.Phones, func(p string) bool { return strings.Contains(p, d) })
		}
		if !hit {
			return false
		}
	}
	if f.Type != "" && c.Type != f.Type {
		return false
	}
	if f.Stage != "" && c.Stage != f.Stage {
		return false
	}
	if f.Tag != "" && !slices.Contains(c.Tags, normaliseTag(f.Tag)) {
		return false
	}
	if f.Owner != nil && !ptrEq(c.OwnerUserID, f.Owner) {
		return false
	}
	if f.Unowned && c.OwnerUserID != nil {
		return false
	}
	if f.ReminderDueBefore != nil && (c.NextReminderAt == nil || !c.NextReminderAt.Before(*f.ReminderDueBefore)) {
		return false
	}
	return true
}

// SortContacts orders contacts the way both stores do.
func SortContacts(out []Contact, by string) {
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch by {
		case SortName:
			if x, y := strings.ToLower(a.Name), strings.ToLower(b.Name); x != y {
				return x < y
			}
		case SortCreated:
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.After(b.CreatedAt)
			}
		default: // last contact, newest first, never contacted last
			switch {
			case a.LastContactAt != nil && b.LastContactAt == nil:
				return true
			case a.LastContactAt == nil && b.LastContactAt != nil:
				return false
			case a.LastContactAt != nil && !a.LastContactAt.Equal(*b.LastContactAt):
				return a.LastContactAt.After(*b.LastContactAt)
			}
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.After(b.CreatedAt)
			}
		}
		return a.ID.String() > b.ID.String()
	})
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, book Book, f ListFilter) ([]Contact, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Contact
	for _, c := range s.contacts {
		if !c.Book.Same(book) || !c.Live() {
			continue
		}
		c = s.withReminders(c, f.Now)
		if s.matches(c, f) {
			out = append(out, c)
		}
	}
	SortContacts(out, f.Sort)
	total := len(out)
	start := min(max(f.Offset, 0), total)
	end := total
	if f.Limit > 0 {
		end = min(start+f.Limit, total)
	}
	return slices.Clone(out[start:end]), total, nil
}

// StageCounts implements Store.
func (s *MemoryStore) StageCounts(_ context.Context, book Book, f ListFilter) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f.Stage = ""
	out := map[string]int{}
	for _, st := range Stages() {
		out[st] = 0
	}
	for _, c := range s.contacts {
		if c.Book.Same(book) && c.Live() && s.matches(s.withReminders(c), f) {
			out[c.Stage]++
		}
	}
	return out, nil
}

// Tags implements Store.
func (s *MemoryStore) Tags(_ context.Context, book Book) ([]TagCount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	for _, c := range s.contacts {
		if c.Book.Same(book) && c.Live() {
			for _, t := range c.Tags {
				counts[t]++
			}
		}
	}
	out := make([]TagCount, 0, len(counts))
	for t, n := range counts {
		out = append(out, TagCount{Tag: t, Count: n})
	}
	SortTags(out)
	return out, nil
}

// SortTags orders tags most used first, then by name.
func SortTags(out []TagCount) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Tag < out[j].Tag
	})
}

// Matching implements Store.
func (s *MemoryStore) Matching(_ context.Context, book Book, emails, phones []string) ([]Contact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Contact
	for _, c := range s.contacts {
		if c.Book.Same(book) && c.Live() && (shares(c.Emails, emails) || shares(c.Phones, phones)) {
			out = append(out, s.withReminders(c))
		}
	}
	SortContacts(out, SortCreated)
	return out, nil
}

// WithEmail implements Store.
func (s *MemoryStore) WithEmail(_ context.Context, email string) ([]Contact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Contact
	for _, c := range s.contacts {
		if c.Live() && email != "" && slices.Contains(c.Emails, email) {
			out = append(out, s.withReminders(c))
		}
	}
	SortContacts(out, SortCreated)
	return out, nil
}

// Apply implements Store.
func (s *MemoryStore) Apply(_ context.Context, ch Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, at := range ch.Expect {
		c, ok := s.contacts[id]
		if !ok {
			return ErrNotFound
		}
		if !c.UpdatedAt.Equal(at) {
			return ErrConflict
		}
	}
	for _, c := range ch.Contacts {
		c.OpenReminders, c.NextReminderAt, c.DueReminders, c.ListingIDs = 0, nil, 0, nil
		s.contacts[c.ID] = clone(c)
	}
	if m := ch.Merge; m != nil {
		for i := range s.notes {
			if slices.Contains(m.From, s.notes[i].ContactID) {
				if s.notes[i].MergedFrom == nil {
					from := s.notes[i].ContactID
					s.notes[i].MergedFrom = &from
				}
				s.notes[i].ContactID = m.Keep
			}
		}
		for i := range s.reminders {
			if slices.Contains(m.From, s.reminders[i].ContactID) {
				s.reminders[i].ContactID = m.Keep
			}
		}
		for i := range s.events {
			if slices.Contains(m.From, s.events[i].ContactID) {
				s.events[i].ContactID = m.Keep
			}
		}
		var kept []Link
		for _, l := range s.links {
			if slices.Contains(m.From, l.ContactID) {
				l.ContactID = m.Keep
			}
			if !slices.ContainsFunc(kept, func(k Link) bool { return k.ContactID == l.ContactID && k.Kind == l.Kind && k.ItemID == l.ItemID }) {
				kept = append(kept, l)
			}
		}
		s.links = kept
	}
	s.notes = append(s.notes, ch.Notes...)
	for _, r := range ch.Reminders {
		if i := slices.IndexFunc(s.reminders, func(x Reminder) bool { return x.ID == r.ID }); i >= 0 {
			s.reminders[i] = r
		} else {
			s.reminders = append(s.reminders, r)
		}
	}
	for _, e := range ch.Events {
		e.Data = roundTrip(e.Data)
		s.events = append(s.events, e)
	}
	for _, l := range ch.Links {
		if !slices.ContainsFunc(s.links, func(k Link) bool { return k.ContactID == l.ContactID && k.Kind == l.Kind && k.ItemID == l.ItemID }) {
			s.links = append(s.links, l)
		}
	}
	for _, u := range ch.Unlinks {
		s.links = slices.DeleteFunc(s.links, func(k Link) bool { return k.ContactID == u.ContactID && k.Kind == u.Kind && k.ItemID == u.ItemID })
	}
	if s.audit != nil {
		for _, e := range ch.Audit {
			s.audit(e)
		}
	}
	return nil
}

// roundTrip gives event data the shape it has after a trip through JSONB.
func roundTrip(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return m
	}
	return out
}

// Notes implements Store (oldest first).
func (s *MemoryStore) Notes(_ context.Context, contactID uuid.UUID) ([]Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Note
	for _, n := range s.notes {
		if n.ContactID == contactID {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Reminders implements Store (soonest first).
func (s *MemoryStore) Reminders(_ context.Context, contactID uuid.UUID) ([]Reminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Reminder
	for _, r := range s.reminders {
		if r.ContactID == contactID {
			out = append(out, r)
		}
	}
	sortReminders(out)
	return out, nil
}

func sortReminders(out []Reminder) {
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].DueAt.Equal(out[j].DueAt) {
			return out[i].DueAt.Before(out[j].DueAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
}

// Events implements Store (oldest first).
func (s *MemoryStore) Events(_ context.Context, contactID uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, e := range s.events {
		if e.ContactID == contactID {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Links implements Store (oldest first).
func (s *MemoryStore) Links(_ context.Context, contactID uuid.UUID) ([]Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Link
	for _, l := range s.links {
		if l.ContactID == contactID {
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LinkedAt.Before(out[j].LinkedAt) })
	return out, nil
}

// DueReminders implements Store.
func (s *MemoryStore) DueReminders(_ context.Context, book Book, before time.Time) ([]DueReminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rems []Reminder
	for _, r := range s.reminders {
		c, ok := s.contacts[r.ContactID]
		if ok && !r.Done && r.DueAt.Before(before) && c.Book.Same(book) && c.Live() {
			rems = append(rems, r)
		}
	}
	sortReminders(rems)
	out := make([]DueReminder, 0, len(rems))
	for _, r := range rems {
		c := s.contacts[r.ContactID]
		out = append(out, DueReminder{Reminder: r, ContactName: c.Name, OwnerUserID: c.OwnerUserID})
	}
	return out, nil
}

// ClaimDueReminders implements Store.
func (s *MemoryStore) ClaimDueReminders(_ context.Context, now time.Time, limit int) ([]Reminder, error) {
	s.mu.Lock()
	var out []Reminder
	var events []outbox.Event
	for i := range s.reminders {
		r := &s.reminders[i]
		c, ok := s.contacts[r.ContactID]
		if len(out) == limit || !ok || !c.Live() || r.Done || r.NotifiedAt != nil || r.DueAt.After(now) {
			continue
		}
		at := now
		r.NotifiedAt = &at
		out = append(out, *r)
		payload, _ := json.Marshal(reminderPayload(*r, c))
		events = append(events, outbox.Event{ID: uuid.New(), Type: EventReminderDue, AggregateType: AggregateContact, AggregateID: c.ID, Payload: payload, CreatedAt: now})
	}
	s.outbox = append(s.outbox, events...)
	sink := s.sink
	s.mu.Unlock()
	if sink != nil {
		for _, ev := range events {
			sink(ev)
		}
	}
	return out, nil
}

// reminderPayload is the CONTACT_REMINDER_DUE body: identifiers only.
func reminderPayload(r Reminder, c Contact) map[string]any {
	p := map[string]any{"reminder_id": r.ID, "contact_id": c.ID, "due_at": r.DueAt}
	if c.Book.OrganisationID != nil {
		p["organisation_id"] = *c.Book.OrganisationID
	}
	if c.Book.UserID != nil {
		p["book_user_id"] = *c.Book.UserID
	}
	if c.OwnerUserID != nil {
		p["owner_user_id"] = *c.OwnerUserID
	}
	if r.CreatedBy != nil {
		p["created_by"] = *r.CreatedBy
	}
	return p
}

// Put stores contacts, notes, reminders, events and links as they are
// (development seed and tests; no checks, no events).
func (s *MemoryStore) Put(ch Change) {
	ch.Expect = nil
	_ = s.Apply(context.Background(), ch)
}
