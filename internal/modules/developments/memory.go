package developments

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// EnquiryLinks is what the memory store tells the business enquiries when a
// project or a unit type goes (what Postgres does by ON DELETE SET NULL).
type EnquiryLinks interface {
	ForgetDevelopment(id uuid.UUID)
	ForgetUnitType(id uuid.UUID)
}

// MemoryStore is the in-memory Store (development and tests).
type MemoryStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]Development
	// visible says whether a project's profile and organisation are public
	// (nil: always). Postgres joins the tables instead.
	visible   func(ctx context.Context, d Development) bool
	audit     *adminaudit.MemoryLog
	enquiries EnquiryLinks
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{rows: map[uuid.UUID]Development{}} }

// SetVisibility sets the rule "the project's developer profile and
// organisation are public".
func (s *MemoryStore) SetVisibility(f func(ctx context.Context, d Development) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.visible = f
}

// SetAuditLog makes Save write the review decisions to the administrators' log.
func (s *MemoryStore) SetAuditLog(log *adminaudit.MemoryLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = log
}

// SetEnquiries makes deletions unlink the enquiries that quote them.
func (s *MemoryStore) SetEnquiries(e EnquiryLinks) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enquiries = e
}

// Put stores a project as it is (seed helper; replaces one with the same ID).
func (s *MemoryStore) Put(d Development) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[d.ID] = d.Clone()
}

func (s *MemoryStore) checkUnique(d Development) error {
	for _, o := range s.rows {
		if o.ID == d.ID {
			continue
		}
		if o.MarketCode == d.MarketCode && o.Slug == d.Slug {
			return ErrSlugTaken
		}
		for _, l := range d.Links {
			if slices.ContainsFunc(o.Links, func(x Link) bool { return x.ListingID == l.ListingID }) {
				return ErrListingLinked
			}
		}
	}
	return nil
}

// Create implements Store.
func (s *MemoryStore) Create(_ context.Context, d Development) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[d.ID]; ok {
		return ErrConflict
	}
	if err := s.checkUnique(d); err != nil {
		return err
	}
	s.rows[d.ID] = d.Clone()
	return nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Development, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok {
		return Development{}, ErrNotFound
	}
	return d.Clone(), nil
}

// Save implements Store.
func (s *MemoryStore) Save(_ context.Context, d Development, expect time.Time, audit *adminaudit.Event) ([]listings.StoredPhoto, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.rows[d.ID]
	if !ok {
		return nil, ErrNotFound
	}
	if !old.UpdatedAt.Equal(expect) {
		return nil, ErrConflict
	}
	if err := s.checkUnique(d); err != nil {
		return nil, err
	}
	var removed []listings.StoredPhoto
	for _, ph := range old.Photos {
		if !slices.ContainsFunc(d.Photos, func(x listings.StoredPhoto) bool { return x.ID == ph.ID }) {
			removed = append(removed, ph)
		}
	}
	keep := d.UnitTypeIDs()
	if s.enquiries != nil {
		for _, id := range old.UnitTypeIDs() {
			if !slices.Contains(keep, id) {
				s.enquiries.ForgetUnitType(id)
			}
		}
	}
	// A link to a unit type that went keeps the listing without a unit type.
	saved := d.Clone()
	for i := range saved.Links {
		if u := saved.Links[i].UnitTypeID; u != nil && !slices.Contains(keep, *u) {
			saved.Links[i].UnitTypeID = nil
		}
	}
	s.rows[d.ID] = saved
	if audit != nil && s.audit != nil {
		s.audit.Append(*audit)
	}
	return removed, nil
}

// Delete implements Store.
func (s *MemoryStore) Delete(_ context.Context, id uuid.UUID, expect time.Time) ([]listings.StoredPhoto, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok {
		return nil, ErrNotFound
	}
	if !d.UpdatedAt.Equal(expect) {
		return nil, ErrConflict
	}
	delete(s.rows, id)
	if s.enquiries != nil {
		s.enquiries.ForgetDevelopment(id)
	}
	return d.Photos, nil
}

// ByOrganisation implements Store.
func (s *MemoryStore) ByOrganisation(_ context.Context, organisationID uuid.UUID) ([]Development, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Development{}
	for _, d := range s.rows {
		if d.OrganisationID == organisationID {
			out = append(out, d.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out, nil
}

// public returns the public projects matching keep, most recently published first.
func (s *MemoryStore) public(ctx context.Context, keep func(Development) bool) []Development {
	s.mu.Lock()
	rows := make([]Development, 0, len(s.rows))
	for _, d := range s.rows {
		if d.Moderation == ModerationPublished && keep(d) {
			rows = append(rows, d.Clone())
		}
	}
	visible := s.visible
	s.mu.Unlock()
	out := rows[:0]
	for _, d := range rows {
		if visible == nil || visible(ctx, d) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].PublishedAt, out[j].PublishedAt
		if a != nil && b != nil && !a.Equal(*b) {
			return a.After(*b)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out
}

func contains(text, q string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(q))
}

// Public implements Store.
func (s *MemoryStore) Public(ctx context.Context, f PublicFilter) ([]Development, int, error) {
	all := s.public(ctx, func(d Development) bool {
		switch {
		case f.Market != "" && d.MarketCode != f.Market,
			f.City != "" && d.CityCode != f.City,
			f.Status != "" && d.Status != f.Status:
			return false
		case f.Q == "":
			return true
		}
		return contains(d.Name, f.Q) || contains(d.Summary, f.Q) || contains(d.Description, f.Q) || contains(d.CityName, f.Q) ||
			(d.CityCode != "" && slices.Contains(f.QPlaces, d.CityCode)) || (d.SuburbCode != "" && slices.Contains(f.QPlaces, d.SuburbCode))
	})
	total := len(all)
	start := min(max(f.Offset, 0), total)
	end := total
	if f.Limit > 0 {
		end = min(start+f.Limit, total)
	}
	return all[start:end], total, nil
}

// PublicBySlug implements Store.
func (s *MemoryStore) PublicBySlug(ctx context.Context, market, slug string) (Development, error) {
	found := s.public(ctx, func(d Development) bool { return d.MarketCode == market && d.Slug == slug })
	if len(found) == 0 {
		return Development{}, ErrNotFound
	}
	return found[0], nil
}

// PublicByProfile implements Store.
func (s *MemoryStore) PublicByProfile(ctx context.Context, profileID uuid.UUID) ([]Development, error) {
	return s.public(ctx, func(d Development) bool { return d.ProfileID == profileID }), nil
}

// PublicEntries implements Store.
func (s *MemoryStore) PublicEntries(ctx context.Context) ([]Entry, error) {
	out := []Entry{}
	for _, d := range s.public(ctx, func(Development) bool { return true }) {
		out = append(out, Entry{Market: d.MarketCode, Slug: d.Slug, UpdatedAt: d.UpdatedAt})
	}
	return out, nil
}

// Queue implements Store.
func (s *MemoryStore) Queue(_ context.Context, f QueueFilter) ([]Development, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := f.Moderation
	if status == "" {
		status = ModerationPending
	}
	all := []Development{}
	for _, d := range s.rows {
		if d.Moderation != status || (f.Markets != nil && !slices.Contains(f.Markets, d.MarketCode)) || (f.Market != "" && d.MarketCode != f.Market) {
			continue
		}
		all = append(all, d.Clone())
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i].SubmittedAt, all[j].SubmittedAt
		switch {
		case a != nil && b != nil && !a.Equal(*b):
			return a.Before(*b)
		case a == nil && b != nil:
			return false
		case a != nil && b == nil:
			return true
		}
		return all[i].ID.String() < all[j].ID.String()
	})
	total := len(all)
	start := min(max(f.Offset, 0), total)
	end := total
	if f.Limit > 0 {
		end = min(start+f.Limit, total)
	}
	return all[start:end], total, nil
}

// PhotoByID implements Store.
func (s *MemoryStore) PhotoByID(_ context.Context, photoID uuid.UUID) (uuid.UUID, listings.StoredPhoto, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.rows {
		for _, ph := range d.Photos {
			if ph.ID == photoID {
				return d.ID, ph, nil
			}
		}
	}
	return uuid.Nil, listings.StoredPhoto{}, listings.ErrPhotoNotFound
}

// ForgetUser unlinks a deleted account from every project (account
// deletion: the users row is anonymised, so ON DELETE SET NULL never fires).
func (s *MemoryStore) ForgetUser(userID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	is := func(p *uuid.UUID) bool { return p != nil && *p == userID }
	for id, d := range s.rows {
		if is(d.CreatedBy) {
			d.CreatedBy = nil
		}
		if is(d.SubmittedBy) {
			d.SubmittedBy = nil
		}
		if is(d.ReviewedBy) {
			d.ReviewedBy = nil
		}
		for i := range d.Photos {
			if is(d.Photos[i].UploadedBy) {
				d.Photos[i].UploadedBy = nil
			}
		}
		s.rows[id] = d
	}
}

// All returns a copy of every project (test helper).
func (s *MemoryStore) All() []Development {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Development, 0, len(s.rows))
	for _, d := range s.rows {
		out = append(out, d.Clone())
	}
	return out
}
