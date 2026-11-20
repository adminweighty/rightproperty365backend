package listings

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/outbox"
)

// memoryMeta is what the memory store keeps about a listing beside its Record.
type memoryMeta struct {
	createdAt    time.Time
	createdBy    *uuid.UUID
	submittedAt  *time.Time
	submittedBy  *uuid.UUID
	reviewReason string
	revision     *Revision
}

// storedFromPublic turns the photos on a record (seed and tests) into stored
// photos: bundled images, approved when the listing has been published.
func storedFromPublic(r Record) []StoredPhoto {
	var out []StoredPhoto
	for i, p := range r.Photos {
		sp := StoredPhoto{ID: uuid.NewSHA1(r.ID, []byte(p.URL+"#"+string(rune('a'+i)))), Position: i, URL: p.URL, Alt: p.Alt,
			Width: p.Width, Height: p.Height, CreatedAt: r.UpdatedAt}
		if !r.PublishedAt.IsZero() {
			at := r.PublishedAt
			sp.ApprovedAt = &at
		}
		out = append(out, sp)
	}
	return out
}

// PutManaged adds or replaces a listing with its management fields and
// photos as they are (seed and test helper).
func (s *MemoryStore) PutManaged(m ManagedListing) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLocked(m)
}

func (s *MemoryStore) putLocked(m ManagedListing) {
	r := m.Record
	r.Photos = nil
	replaced := false
	for i := range s.records {
		if s.records[i].ID == r.ID {
			s.records[i], replaced = r, true
		}
	}
	if !replaced {
		s.records = append(s.records, r)
	}
	s.photos[r.ID] = clonePhotos(m.AllPhotos)
	s.meta[r.ID] = &memoryMeta{createdAt: m.CreatedAt, createdBy: m.CreatedBy, submittedAt: m.SubmittedAt, submittedBy: m.SubmittedBy,
		reviewReason: m.ReviewReason, revision: cloneRevision(m.Revision)}
}

func clonePhotos(in []StoredPhoto) []StoredPhoto {
	out := make([]StoredPhoto, len(in))
	for i, p := range in {
		p.Variants = append([]StoredVariant(nil), p.Variants...)
		out[i] = p
	}
	return out
}

func cloneRevision(r *Revision) *Revision {
	if r == nil {
		return nil
	}
	c := *r
	if r.Content != nil {
		content := *r.Content
		content.Features = append([]string{}, r.Content.Features...)
		c.Content = &content
	}
	if r.PhotoAlts != nil {
		c.PhotoAlts = map[string]string{}
		for k, v := range r.PhotoAlts {
			c.PhotoAlts[k] = v
		}
	}
	return &c
}

// SetOutboxSink registers a receiver for events as they are written (the
// development server sends the review emails from it).
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// Outbox returns a copy of the outbox events (test helper).
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]outbox.Event(nil), s.outbox...)
}

// record writes the change's timeline entry and events. Caller holds the lock.
func (s *MemoryStore) record(id uuid.UUID, ch Change, now time.Time) {
	if ch.Entry != nil {
		s.timeline = append(s.timeline, *ch.Entry)
	}
	for _, e := range ch.Events {
		ev := outbox.Event{ID: uuid.New(), Type: e.Type, AggregateType: AggregateListing, AggregateID: id, Payload: e.Payload, CreatedAt: now}
		s.outbox = append(s.outbox, ev)
		if s.sink != nil {
			s.sink(ev)
		}
	}
}

func (s *MemoryStore) index(id uuid.UUID) int {
	for i := range s.records {
		if s.records[i].ID == id {
			return i
		}
	}
	return -1
}

func (s *MemoryStore) managedLocked(i int) ManagedListing {
	r := s.resolved(s.records[i])
	m := ManagedListing{Record: r, AllPhotos: clonePhotos(sortedPhotos(s.photos[r.ID]))}
	if meta := s.meta[r.ID]; meta != nil {
		m.CreatedAt, m.CreatedBy, m.SubmittedAt, m.SubmittedBy = meta.createdAt, meta.createdBy, meta.submittedAt, meta.submittedBy
		m.ReviewReason, m.Revision = meta.reviewReason, cloneRevision(meta.revision)
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = r.UpdatedAt
	}
	return m
}

// taken reports whether another listing has the slug or reference. Caller holds the lock.
func (s *MemoryStore) taken(m ManagedListing) bool {
	for _, r := range s.records {
		if r.ID != m.ID && (r.Slug == m.Slug || (m.Reference != "" && r.Reference == m.Reference)) {
			return true
		}
	}
	return false
}

// CreateListing implements ManageStore.
func (s *MemoryStore) CreateListing(_ context.Context, m ManagedListing, ch Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index(m.ID) >= 0 || s.taken(m) {
		return ErrDuplicate
	}
	s.putLocked(m)
	s.record(m.ID, ch, m.UpdatedAt)
	return nil
}

// Managed implements ManageStore.
func (s *MemoryStore) Managed(_ context.Context, id uuid.UUID) (ManagedListing, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i := s.index(id)
	if i < 0 {
		return ManagedListing{}, ErrNotFound
	}
	return s.managedLocked(i), nil
}

// SaveListing implements ManageStore.
func (s *MemoryStore) SaveListing(_ context.Context, m ManagedListing, expect time.Time, ch Change) ([]StoredPhoto, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(m.ID)
	if i < 0 {
		return nil, ErrNotFound
	}
	if !s.records[i].UpdatedAt.Equal(expect) {
		return nil, ErrConflict
	}
	if s.taken(m) {
		return nil, ErrDuplicate
	}
	if err := s.enforceLocked(ch.Quota, m.ID); err != nil {
		return nil, err
	}
	// Photos: the ones kept get their alt, position and approval; the others go.
	keep := map[uuid.UUID]StoredPhoto{}
	for _, p := range m.AllPhotos {
		keep[p.ID] = p
	}
	var stored, removed []StoredPhoto
	for _, p := range s.photos[m.ID] {
		if k, ok := keep[p.ID]; ok {
			p.Alt, p.Position, p.ApprovedAt = k.Alt, k.Position, k.ApprovedAt
			stored = append(stored, p)
		} else {
			removed = append(removed, p)
		}
	}
	m.AllPhotos = stored
	s.putLocked(m)
	if ch.NewCycle {
		s.cycles[m.ID] = max(s.cycles[m.ID], ch.MinRelistNo) + 1
	}
	s.record(m.ID, ch, m.UpdatedAt)
	return removed, nil
}

// DeleteListing implements ManageStore.
func (s *MemoryStore) DeleteListing(_ context.Context, id uuid.UUID, expect time.Time) ([]StoredPhoto, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return nil, ErrNotFound
	}
	if !s.records[i].UpdatedAt.Equal(expect) {
		return nil, ErrConflict
	}
	removed := s.photos[id]
	s.records = slices.Delete(s.records, i, i+1)
	delete(s.photos, id)
	delete(s.meta, id)
	delete(s.cycles, id)
	for k := range s.notices {
		if k.listing == id {
			delete(s.notices, k)
		}
	}
	s.timeline = slices.DeleteFunc(s.timeline, func(e TimelineEntry) bool { return e.ListingID == id })
	return removed, nil
}

// AddPhoto implements ManageStore.
func (s *MemoryStore) AddPhoto(_ context.Context, listingID uuid.UUID, p StoredPhoto, limit int, expect, now time.Time, ch Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(listingID)
	if i < 0 {
		return ErrNotFound
	}
	if !s.records[i].UpdatedAt.Equal(expect) {
		return ErrConflict
	}
	photos := s.photos[listingID]
	if len(photos) >= limit {
		return ErrPhotoLimit
	}
	p.Position = 0
	for _, existing := range photos {
		p.Position = max(p.Position, existing.Position+1)
	}
	s.photos[listingID] = append(photos, p)
	s.records[i].UpdatedAt = now
	s.record(listingID, ch, now)
	return nil
}

// PhotoByID implements ManageStore.
func (s *MemoryStore) PhotoByID(_ context.Context, photoID uuid.UUID) (uuid.UUID, StoredPhoto, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for listingID, photos := range s.photos {
		for _, p := range photos {
			if p.ID == photoID {
				return listingID, clonePhotos([]StoredPhoto{p})[0], nil
			}
		}
	}
	return uuid.Nil, StoredPhoto{}, ErrPhotoNotFound
}

// matchesReview reports whether a listing is in the review queue of f.
func matchesReview(m ManagedListing, f ReviewFilter) bool {
	status := f.Status
	if status == "" {
		status = StatusPendingReview
	}
	revision := ""
	if m.Revision != nil {
		revision = m.Revision.Status
	}
	switch {
	case m.Status != status && revision != status:
		return false
	case f.Markets != nil && !slices.Contains(f.Markets, m.MarketCode):
		return false
	case f.Country != "" && m.MarketCode != f.Country:
		return false
	case f.Kind == ReviewKindNew && m.Status != status:
		return false
	case f.Kind == ReviewKindEdit && revision != status:
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Q)); q != "" {
		return strings.Contains(strings.ToLower(m.Title), q) || strings.EqualFold(m.Reference, q) ||
			(m.Revision != nil && m.Revision.Content != nil && strings.Contains(strings.ToLower(m.Revision.Content.Title), q))
	}
	return true
}

// submittedOf is the time a listing joined the queue, for the order.
func submittedOf(m ManagedListing) time.Time {
	if m.SubmittedAt != nil {
		return *m.SubmittedAt
	}
	return m.UpdatedAt
}

// ReviewQueue implements ManageStore.
func (s *MemoryStore) ReviewQueue(_ context.Context, f ReviewFilter) ([]ManagedListing, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var all []ManagedListing
	for i := range s.records {
		if m := s.managedLocked(i); matchesReview(m, f) {
			all = append(all, m)
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := submittedOf(all[i]), submittedOf(all[j])
		if !a.Equal(b) {
			return a.Before(b)
		}
		return all[i].ID.String() < all[j].ID.String()
	})
	start := min((f.Page-1)*f.PageSize, len(all))
	end := min(start+f.PageSize, len(all))
	return all[start:end], len(all), nil
}

// Timeline implements ManageStore.
func (s *MemoryStore) Timeline(_ context.Context, listingID uuid.UUID) ([]TimelineEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []TimelineEntry
	for _, e := range s.timeline {
		if e.ListingID == listingID {
			out = append(out, e)
		}
	}
	return out, nil
}
