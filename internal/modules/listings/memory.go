package listings

import (
	"context"
	"math/big"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/packages"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore is the in-memory Store and Search adapter for development and
// tests. It implements every filter, sort order, facet and suggestion with
// the same semantics as the Postgres adapter.
type MemoryStore struct {
	mu      sync.RWMutex
	records []Record
	labels  Labels
	// advertisers resolves who advertises a record at the moment it is read,
	// as the Postgres adapter does with its joins. nil keeps the advertiser
	// stored on the record (unit tests).
	advertisers func(Record) Advertiser

	// Listing management (manage_memory.go): every photo of a listing, the
	// management fields, the timelines and the outbox events.
	photos   map[uuid.UUID][]StoredPhoto
	meta     map[uuid.UUID]*memoryMeta
	timeline []TimelineEntry
	outbox   []outbox.Event
	sink     outbox.Sink
	// cycles counts the publication cycles of a listing (publication_cycles)
	// and notices are the expiry notices recorded (listing_expiry_notices,
	// expiry.go).
	cycles  map[uuid.UUID]int
	notices map[expiryNoticeKey]bool
	// placements and their audit (placements_memory.go, API 0.53.0).
	placements     []Placement
	placementAudit *adminaudit.MemoryLog
	// reports (reports_memory.go, API 0.54.0).
	reports []ListingReport

	// quota resolves the listing package of an account in a market (the
	// memory twin of packages.EnforceTx; nil: no limit). See quota.go.
	quota func(acct packages.Account, market string, now time.Time) packages.Resolved
}

// SetAdvertisers makes the store resolve the advertiser of every record it
// returns from the current state of the profiles and accounts, instead of
// from the copy on the record: verification is never read from a snapshot.
func (s *MemoryStore) SetAdvertisers(resolve func(Record) Advertiser) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advertisers = resolve
}

// resolved returns the record with its current advertiser. Caller holds the lock.
func (s *MemoryStore) resolved(r Record) Record {
	if s.advertisers != nil {
		r.Advertiser = s.advertisers(r)
	}
	// The public photos are the approved ones, always derived from all of them.
	r.Photos = PublicPhotos(s.photos[r.ID])
	r.Features = append([]string(nil), r.Features...)
	r.Cycles = s.cycles[r.ID]
	return r
}

// NewMemoryStore returns an empty store. labels supplies the suburb, city and
// property-type labels that free-text search matches against.
func NewMemoryStore(labels Labels) *MemoryStore {
	return &MemoryStore{labels: labels, photos: map[uuid.UUID][]StoredPhoto{}, meta: map[uuid.UUID]*memoryMeta{},
		cycles: map[uuid.UUID]int{}, notices: map[expiryNoticeKey]bool{}}
}

// Put adds or replaces a listing (seed and test helper). Its photos become
// the listing's photos, approved when the listing has been published.
func (s *MemoryStore) Put(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.photos[r.ID] = storedFromPublic(r)
	for i := range s.records {
		if s.records[i].ID == r.ID {
			s.records[i] = r
			return
		}
	}
	s.records = append(s.records, r)
}

func (s *MemoryStore) public(now time.Time) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Record
	for _, r := range s.records {
		// Status, expiry AND a verified advertiser, always.
		if r = s.resolved(r); r.Public(now) {
			out = append(out, r)
		}
	}
	return out
}

// PublicCounts implements Store.
func (s *MemoryStore) PublicCounts(_ context.Context, now time.Time) (PublicCounts, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var c PublicCounts
	for _, r := range s.records {
		r = s.resolved(r)
		if r.Status == StatusPublished && r.ExpiresAt.After(now) && r.Advertiser.Verified {
			c.VerifiedAdvertiser++
		}
		if r.Public(now) {
			c.Public++
			if r.ReviewedAt != nil {
				c.Reviewed++
			}
		}
	}
	return c, nil
}

// PublishedByIDs implements Store.
func (s *MemoryStore) PublishedByIDs(_ context.Context, ids []uuid.UUID, now time.Time) ([]Record, error) {
	var out []Record
	for _, r := range s.public(now) {
		if slices.Contains(ids, r.ID) {
			out = append(out, r)
		}
	}
	return out, nil
}

// ByIDs implements Store.
func (s *MemoryStore) ByIDs(_ context.Context, ids []uuid.UUID) ([]Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Record
	for _, r := range s.records {
		if slices.Contains(ids, r.ID) {
			out = append(out, s.resolved(r))
		}
	}
	return out, nil
}

// AllPublished implements Store.
func (s *MemoryStore) AllPublished(_ context.Context, now time.Time) ([]Record, error) {
	records := s.public(now)
	newestFirst(records)
	return records, nil
}

// BySlug implements Store.
func (s *MemoryStore) BySlug(_ context.Context, slug string) (Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.records {
		if r.Slug == slug {
			return s.resolved(r), nil
		}
	}
	return Record{}, ErrNotFound
}

func newestFirst(records []Record) {
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].PublishedAt.Equal(records[j].PublishedAt) {
			return records[i].PublishedAt.After(records[j].PublishedAt)
		}
		return records[i].ID.String() < records[j].ID.String()
	})
}

// PublishedByOwner implements Store.
func (s *MemoryStore) PublishedByOwner(_ context.Context, organisationID, agentUserID *uuid.UUID, now time.Time) ([]Record, error) {
	var out []Record
	for _, r := range s.public(now) {
		switch {
		case agentUserID != nil:
			if r.AgentUserID != nil && *r.AgentUserID == *agentUserID {
				out = append(out, r)
			}
		case organisationID != nil:
			if r.OrganisationID != nil && *r.OrganisationID == *organisationID {
				out = append(out, r)
			}
		}
	}
	newestFirst(out)
	return out, nil
}

// CountByCountry implements Store.
func (s *MemoryStore) CountByCountry(_ context.Context, now time.Time) (map[string]int, error) {
	out := map[string]int{}
	for _, r := range s.public(now) {
		if r.MarketCode != "" {
			out[r.MarketCode]++
		}
	}
	return out, nil
}

// PublishedSlugs implements Store.
func (s *MemoryStore) PublishedSlugs(_ context.Context, now time.Time) ([]SlugEntry, error) {
	records := s.public(now)
	newestFirst(records)
	out := make([]SlugEntry, 0, len(records))
	for _, r := range records {
		out = append(out, SlugEntry{Slug: r.Slug, UpdatedAt: r.UpdatedAt.UTC()})
	}
	return out, nil
}

// searchLabels loads the labels free-text search matches against.
func (s *MemoryStore) searchLabels(ctx context.Context) (suburbs, cities, types map[string]string, err error) {
	if suburbs, err = s.labels.Labels(ctx, SetSuburbs); err != nil {
		return
	}
	if cities, err = s.labels.Labels(ctx, SetCities); err != nil {
		return
	}
	types, err = s.labels.Labels(ctx, SetPropertyTypes)
	return
}

func terms(q string) []string { return strings.Fields(strings.ToLower(q)) }

// listedSince is the earliest publication time "listed within N days" keeps.
func listedSince(now time.Time, days int) time.Time {
	return now.Add(-time.Duration(days) * 24 * time.Hour)
}

// score returns how well a record matches the search terms; 0 means no
// match. Every term must match somewhere. Title matches weigh most.
func score(r Record, words []string, suburb, city, propertyType, keywords string) int {
	if len(words) == 0 {
		return 1
	}
	// A free-text city or region is searchable like a reference one.
	title, place := strings.ToLower(r.Title), strings.ToLower(suburb+" "+city+" "+r.CityName+" "+r.RegionName)
	// keywords are the category's label and description ("Short stays ...
	// Airbnb-style stays"), so a category can be found by what people call it.
	kind, rest := strings.ToLower(propertyType), strings.ToLower(r.Summary+" "+r.Description+" "+r.Reference+" "+keywords)
	total := 0
	for _, w := range words {
		points := 0
		if strings.Contains(title, w) {
			points += 4
		}
		if strings.Contains(place, w) {
			points += 3
		}
		if strings.Contains(kind, w) {
			points += 2
		}
		if strings.Contains(rest, w) {
			points++
		}
		if points == 0 {
			return 0
		}
		total += points
	}
	return total
}

func amount(r Record) *big.Rat {
	v, ok := new(big.Rat).SetString(r.Price.Amount)
	if !ok {
		return new(big.Rat)
	}
	return v
}

// matches applies every filter except the one named by skip (used to compute
// that filter's facet).
func matches(r Record, q Query, skip string) bool {
	switch {
	case skip != "deal" && q.Deal != "" && r.Deal != q.Deal,
		skip != "category" && q.Category != "" && r.Category != q.Category,
		skip != "property_type" && q.PropertyType != "" && r.PropertyType != q.PropertyType,
		skip != "city" && q.City != "" && r.City != q.City,
		skip != "suburb" && q.Suburb != "" && r.Suburb != q.Suburb,
		q.Featured != nil && r.Featured != *q.Featured,
		q.BathroomsMin != nil && (r.Bathrooms == nil || *r.Bathrooms < *q.BathroomsMin),
		skip != "bedrooms" && q.BedroomsMin != nil && (r.Bedrooms == nil || *r.Bedrooms < *q.BedroomsMin),
		skip != "country" && q.Country != "" && r.MarketCode != q.Country,
		skip != "region" && q.Region != "" && q.CityRegion[r.City] != q.Region,
		skip != "bedrooms" && q.BedroomsMax != nil && (r.Bedrooms == nil || *r.Bedrooms > *q.BedroomsMax),
		q.BathroomsMax != nil && (r.Bathrooms == nil || *r.Bathrooms > *q.BathroomsMax),
		q.FloorAreaMin != nil && (r.FloorArea == nil || *r.FloorArea < *q.FloorAreaMin),
		q.FloorAreaMax != nil && (r.FloorArea == nil || *r.FloorArea > *q.FloorAreaMax),
		q.LandAreaMin != nil && (r.LandArea == nil || *r.LandArea < *q.LandAreaMin),
		q.LandAreaMax != nil && (r.LandArea == nil || *r.LandArea > *q.LandAreaMax),
		q.GuestsMin != nil && (r.MaxGuests == nil || *r.MaxGuests < *q.GuestsMin),
		q.Reference != "" && !strings.EqualFold(r.Reference, q.Reference),
		q.HasPhotos && len(r.Photos) == 0,
		q.AdvertiserType == AdvertiserFilterAgency && r.Advertiser.Type != AdvertiserAgency,
		q.AdvertiserType == AdvertiserFilterPrivate && r.Advertiser.Type != AdvertiserPrivateSeller:
		return false
	}
	for _, f := range q.Features {
		if !slices.Contains(r.Features, f) {
			return false
		}
	}
	// A stay: never skipped, so every facet is counted with it applied.
	if q.Stay != nil && !stayMatches(r, *q.Stay) {
		return false
	}
	// The map box and has_location: never skipped either.
	if !locationMatches(r, q) {
		return false
	}
	if skip != "price" && q.priced() {
		if r.Price.Currency != currencyOf(q) {
			return false
		}
		a := amount(r)
		if (q.MinPrice != nil && a.Cmp(q.MinPrice) < 0) || (q.MaxPrice != nil && a.Cmp(q.MaxPrice) > 0) {
			return false
		}
	}
	return true
}

// stayMatches reports whether a listing can take a stay: it is not among the
// excluded listings or countries, and the stay is long enough for it.
func stayMatches(r Record, stay Stay) bool {
	if slices.Contains(stay.ExcludeIDs, r.ID) || slices.Contains(stay.ExcludeCountries, r.MarketCode) {
		return false
	}
	return r.MinNights == nil || *r.MinNights <= stay.Nights
}

// Owned implements Store.
func (s *MemoryStore) Owned(_ context.Context, organisationIDs []uuid.UUID, sellerUserID *uuid.UUID) ([]Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Record
	for _, r := range s.records {
		if (r.OrganisationID != nil && slices.Contains(organisationIDs, *r.OrganisationID)) ||
			(sellerUserID != nil && r.SellerUserID != nil && *r.SellerUserID == *sellerUserID) {
			out = append(out, s.resolved(r))
		}
	}
	newestFirst(out)
	return out, nil
}

// SetStayRules implements Store.
func (s *MemoryStore) SetStayRules(_ context.Context, id uuid.UUID, minNights, maxGuests *int, now time.Time) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		if s.records[i].ID != id {
			continue
		}
		if minNights != nil {
			n := *minNights
			s.records[i].MinNights = &n
		}
		if maxGuests != nil {
			n := *maxGuests
			s.records[i].MaxGuests = &n
		}
		s.records[i].UpdatedAt = now
		return s.resolved(s.records[i]), nil
	}
	return Record{}, ErrNotFound
}

// Search implements Search.
func (s *MemoryStore) Search(ctx context.Context, q Query, now time.Time) (Hits, error) {
	suburbs, cities, types, err := s.searchLabels(ctx)
	if err != nil {
		return Hits{}, err
	}
	keywords, err := categoryKeywords(ctx, s.labels)
	if err != nil {
		return Hits{}, err
	}
	words := terms(q.Q)
	type scored struct {
		Record
		score int
	}
	var candidates []scored // public and matching the text
	// "Listed within N days" is not a facet: it narrows everything.
	var since time.Time
	if q.ListedWithinDays != nil {
		since = listedSince(now, *q.ListedWithinDays)
	}
	for _, r := range s.public(now) {
		if q.ListedWithinDays != nil && r.PublishedAt.Before(since) {
			continue
		}
		if sc := score(r, words, suburbs[r.Suburb], cities[r.City], types[r.PropertyType], keywords[r.Category]); sc > 0 {
			candidates = append(candidates, scored{r, sc})
		}
	}
	facets := RawFacets{
		Deal: map[string]int{}, Category: map[string]int{}, PropertyType: map[string]int{}, City: map[string]int{},
		Suburb: map[string]int{}, Features: map[string]int{}, Bedrooms: map[int]int{},
		Country: map[string]int{}, Region: map[string]int{}, Prices: map[string]PriceStat{},
	}
	var matched []scored
	for _, c := range candidates {
		if matches(c.Record, q, "deal") {
			facets.Deal[c.Deal]++
		}
		if matches(c.Record, q, "category") {
			facets.Category[c.Category]++
		}
		if matches(c.Record, q, "property_type") {
			facets.PropertyType[c.PropertyType]++
		}
		if c.City != "" && matches(c.Record, q, "city") {
			facets.City[c.City]++
		}
		if c.Suburb != "" && matches(c.Record, q, "suburb") {
			facets.Suburb[c.Suburb]++
		}
		if matches(c.Record, q, "country") && c.MarketCode != "" {
			facets.Country[c.MarketCode]++
		}
		if region := q.CityRegion[c.City]; region != "" && matches(c.Record, q, "region") {
			facets.Region[region]++
		}
		if matches(c.Record, q, "bedrooms") && c.Bedrooms != nil {
			facets.Bedrooms[*c.Bedrooms]++
		}
		if matches(c.Record, q, "price") { // per currency: nothing is converted
			a, stat := amount(c.Record), facets.Prices[c.Price.Currency]
			stat.Count++
			if stat.Min == nil || a.Cmp(stat.Min) < 0 {
				stat.Min = a
			}
			if stat.Max == nil || a.Cmp(stat.Max) > 0 {
				stat.Max = a
			}
			facets.Prices[c.Price.Currency] = stat
		}
		if matches(c.Record, q, "") {
			matched = append(matched, c)
			for _, f := range c.Features {
				facets.Features[f]++
			}
		}
	}

	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		switch q.Sort {
		case SortPriceAsc, SortPriceDesc:
			if c := amount(a.Record).Cmp(amount(b.Record)); c != 0 {
				return (c < 0) == (q.Sort == SortPriceAsc)
			}
		case SortRelevance:
			if a.score != b.score {
				return a.score > b.score
			}
			if a.Featured != b.Featured {
				return a.Featured
			}
		case SortOldest:
			if !a.PublishedAt.Equal(b.PublishedAt) {
				return a.PublishedAt.Before(b.PublishedAt)
			}
		case SortAreaDesc: // largest floor area first; listings without one last
			switch {
			case a.FloorArea == nil && b.FloorArea == nil:
			case a.FloorArea == nil:
				return false
			case b.FloorArea == nil:
				return true
			case *a.FloorArea != *b.FloorArea:
				return *a.FloorArea > *b.FloorArea
			}
		}
		if !a.PublishedAt.Equal(b.PublishedAt) {
			return a.PublishedAt.After(b.PublishedAt)
		}
		return a.ID.String() < b.ID.String()
	})

	hits := Hits{Total: len(matched), Facets: facets, IDs: []uuid.UUID{}}
	for _, m := range matched {
		if m.PublicLocation() != nil {
			hits.Mappable++
		}
	}
	start := (q.Page - 1) * q.PageSize
	for i := start; i < len(matched) && i < start+q.PageSize; i++ {
		hits.IDs = append(hits.IDs, matched[i].ID)
	}
	return hits, nil
}

// Suggest implements Search: suburbs and cities that have public listings,
// then listing titles. Names that start with the term come first.
func (s *MemoryStore) Suggest(ctx context.Context, term string, limit int, now time.Time) ([]Suggestion, error) {
	suburbs, cities, _, err := s.searchLabels(ctx)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(strings.TrimSpace(term))
	records := s.public(now)
	newestFirst(records)

	type candidate struct {
		Suggestion
		prefix bool
		rank   int
	}
	var found []candidate
	seen := map[string]bool{}
	add := func(kind, label, value, haystack string, rank int) {
		h := strings.ToLower(haystack)
		if key := kind + "/" + value; !seen[key] && strings.Contains(h, needle) {
			seen[key] = true
			found = append(found, candidate{Suggestion{Type: kind, Label: label, Value: value}, strings.HasPrefix(h, needle), rank})
		}
	}
	// Only reference places are suggested: a suggestion's value is a filter
	// code, and a free-text city has none.
	for _, r := range records {
		if r.Suburb != "" {
			add("suburb", suburbs[r.Suburb]+", "+cities[r.City], r.Suburb, suburbs[r.Suburb], 0)
		}
	}
	for _, r := range records {
		if r.City != "" {
			add("city", cities[r.City], r.City, cities[r.City], 1)
		}
	}
	for _, r := range records {
		add("listing", r.Title, r.Slug, r.Title, 2)
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].rank != found[j].rank {
			return found[i].rank < found[j].rank
		}
		if found[i].prefix != found[j].prefix {
			return found[i].prefix
		}
		if found[i].rank < 2 {
			return found[i].Label < found[j].Label
		}
		return false // listings stay newest first
	})
	out := []Suggestion{}
	for _, c := range found {
		if len(out) == limit {
			break
		}
		out = append(out, c.Suggestion)
	}
	return out, nil
}

// SetBranch ties a listing to a branch (API 0.57.0; the branches store
// owns the change).
func (s *MemoryStore) SetBranch(id uuid.UUID, branch *uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		if s.records[i].ID == id {
			if branch == nil {
				s.records[i].BranchID = nil
			} else {
				b := *branch
				s.records[i].BranchID = &b
			}
		}
	}
}
