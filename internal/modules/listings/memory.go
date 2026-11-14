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
)

// MemoryStore is the in-memory Store and Search adapter for development and
// tests. It implements every filter, sort order, facet and suggestion with
// the same semantics as the Postgres adapter.
type MemoryStore struct {
	mu      sync.RWMutex
	records []Record
	labels  Labels
}

// NewMemoryStore returns an empty store. labels supplies the suburb, city and
// property-type labels that free-text search matches against.
func NewMemoryStore(labels Labels) *MemoryStore { return &MemoryStore{labels: labels} }

// Put adds or replaces a listing (seed and test helper).
func (s *MemoryStore) Put(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
		if r.Public(now) { // status AND expiry, always
			out = append(out, r)
		}
	}
	return out
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
			out = append(out, r)
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
			return r, nil
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
	title, place := strings.ToLower(r.Title), strings.ToLower(suburb+" "+city)
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
		Country: map[string]int{}, Region: map[string]int{},
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
		if matches(c.Record, q, "city") {
			facets.City[c.City]++
		}
		if matches(c.Record, q, "suburb") {
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
		if matches(c.Record, q, "price") && c.Price.Currency == currencyOf(q) {
			a := amount(c.Record)
			if facets.PriceMin == nil || a.Cmp(facets.PriceMin) < 0 {
				facets.PriceMin = a
			}
			if facets.PriceMax == nil || a.Cmp(facets.PriceMax) > 0 {
				facets.PriceMax = a
			}
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
	for _, r := range records {
		add("suburb", suburbs[r.Suburb]+", "+cities[r.City], r.Suburb, suburbs[r.Suburb], 0)
	}
	for _, r := range records {
		add("city", cities[r.City], r.City, cities[r.City], 1)
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
