package listings

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Service implements the public listing use cases.
type Service struct {
	store  Store
	search Search
	labels Labels
	// places supplies the location hierarchy; nil means none (no regions).
	places Places
	now    func() time.Time
}

// SetPlaces gives the Service the location hierarchy behind the country and
// region filters, facets, suggestions and the region / country of a listing.
func (s *Service) SetPlaces(p Places) { s.places = p }

// NewService builds a Service.
func NewService(store Store, search Search, labels Labels) *Service {
	return &Service{store: store, search: search, labels: labels, now: time.Now}
}

// SetClock overrides the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Page is one page of search results.
type Page struct {
	Items      []Listing `json:"items"`
	Page       int       `json:"page"`
	PageSize   int       `json:"page_size"`
	Total      int       `json:"total"`
	TotalPages int       `json:"total_pages"`
	Facets     Facets    `json:"facets"`
}

// labelSets loads the labels of every set a listing uses.
func (s *Service) labelSets(ctx context.Context) (map[string]map[string]string, error) {
	return loadLabels(ctx, s.labels, s.places)
}

func coded(labels map[string]string, code string) Coded {
	if label, ok := labels[code]; ok {
		return Coded{Code: code, Label: label}
	}
	return Coded{Code: code, Label: code} // the row was retired: show the code rather than nothing
}

// present turns a stored record into its public form.
func present(r Record, labels map[string]map[string]string) Listing {
	l := Listing{
		ID: r.ID, Slug: r.Slug, Reference: r.Reference, Title: r.Title,
		Deal: coded(labels[SetDealTypes], r.Deal), Category: coded(labels[SetPropertyCategories], r.Category),
		PropertyType: coded(labels[SetPropertyTypes], r.PropertyType),
		Price:        r.Price, Suburb: coded(labels[SetSuburbs], r.Suburb), City: coded(labels[SetCities], r.City),
		MarketCode: r.MarketCode, Bedrooms: r.Bedrooms, Bathrooms: r.Bathrooms, FloorArea: r.FloorArea, LandArea: r.LandArea,
		Summary: r.Summary, Description: r.Description, Featured: r.Featured,
		Features: make([]Coded, 0, len(r.Features)), Photos: append([]Photo{}, r.Photos...), Advertiser: r.Advertiser,
		PublishedAt: r.PublishedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
	if r.PricePeriod != "" {
		period := r.PricePeriod
		l.PricePeriod = &period
	}
	// Short-stay details; a short stay without a minimum is one night.
	if r.Category == CategoryShortStay {
		l.MaxGuests, l.MinNights = r.MaxGuests, r.MinNights
		if l.MinNights == nil {
			one := 1
			l.MinNights = &one
		}
	}
	// The region is the region of the city; the country is the market.
	if region := labels[keyCityRegion][r.City]; region != "" {
		c := coded(labels[SetRegions], region)
		l.Region = &c
	}
	l.Country = Country{Code: r.MarketCode, Name: r.MarketCode}
	if name := labels[keyCountryNames][r.MarketCode]; name != "" {
		l.Country.Name = name
	}
	for _, f := range r.Features {
		l.Features = append(l.Features, coded(labels[SetListingFeatures], f))
	}
	return l
}

// Present converts records for other modules (the directory lists an
// agency's or agent's listings).
func (s *Service) Present(ctx context.Context, records []Record) ([]Listing, error) {
	labels, err := s.labelSets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Listing, 0, len(records))
	for _, r := range records {
		out = append(out, present(r, labels))
	}
	return out, nil
}

// Find runs a search. Every hit is re-read from the Store, which keeps only
// listings that are PUBLISHED and unexpired right now.
func (s *Service) Find(ctx context.Context, q Query) (Page, error) {
	now := s.now().UTC()
	hits, err := s.search.Search(ctx, q, now)
	if err != nil {
		return Page{}, fmt.Errorf("search listings: %w", err)
	}
	records, err := s.store.PublishedByIDs(ctx, hits.IDs, now)
	if err != nil {
		return Page{}, fmt.Errorf("load listings: %w", err)
	}
	byID := make(map[uuid.UUID]Record, len(records))
	for _, r := range records {
		byID[r.ID] = r
	}
	labels, err := s.labelSets(ctx)
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: []Listing{}, Page: q.Page, PageSize: q.PageSize, Total: hits.Total}
	for _, id := range hits.IDs { // keep the search order; drop what the source of truth refused
		if r, ok := byID[id]; ok && r.Public(now) {
			page.Items = append(page.Items, present(r, labels))
		}
	}
	// A hit the source of truth refused (the index was behind) is not counted either.
	if dropped := len(hits.IDs) - len(page.Items); dropped > 0 && page.Total >= dropped {
		page.Total -= dropped
	}
	if q.PageSize > 0 {
		page.TotalPages = (hits.Total + q.PageSize - 1) / q.PageSize
	}
	page.Facets = s.facets(hits.Facets, q, labels)
	return page, nil
}

// facets attaches labels and a stable order to raw counts.
func (s *Service) facets(raw RawFacets, q Query, labels map[string]map[string]string) Facets {
	build := func(counts map[string]int, set string, byCount bool) []FacetCount {
		out := make([]FacetCount, 0, len(counts))
		for code, n := range counts {
			if n > 0 {
				out = append(out, FacetCount{Code: code, Label: coded(labels[set], code).Label, Count: n})
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if byCount && out[i].Count != out[j].Count {
				return out[i].Count > out[j].Count
			}
			return out[i].Label < out[j].Label
		})
		return out
	}
	f := Facets{
		Deal: build(raw.Deal, SetDealTypes, false), Category: build(raw.Category, SetPropertyCategories, false),
		PropertyType: build(raw.PropertyType, SetPropertyTypes, false), City: build(raw.City, SetCities, false),
		Suburb: build(raw.Suburb, SetSuburbs, false), Features: build(raw.Features, SetListingFeatures, true),
		Bedrooms: []FacetCount{}, Price: PriceRange{Currency: currencyOf(q)},
		Country: build(raw.Country, keyCountryNames, false), Region: build(raw.Region, SetRegions, false),
	}
	// Each suburb names its city, so a client can group them.
	for i := range f.Suburb {
		if city := labels[keySuburbCity][f.Suburb[i].Code]; city != "" {
			f.Suburb[i].City = &city
		}
	}
	beds := make([]int, 0, len(raw.Bedrooms))
	for n := range raw.Bedrooms {
		beds = append(beds, n)
	}
	sort.Ints(beds)
	for _, n := range beds {
		label := strconv.Itoa(n) + " bedrooms"
		if n == 1 {
			label = "1 bedroom"
		}
		f.Bedrooms = append(f.Bedrooms, FacetCount{Code: strconv.Itoa(n), Label: label, Count: raw.Bedrooms[n]})
	}
	if raw.PriceMin != nil {
		lo, hi := FormatAmount(raw.PriceMin), FormatAmount(raw.PriceMax)
		f.Price.Min, f.Price.Max = &lo, &hi
	}
	return f
}

func currencyOf(q Query) string {
	if q.Currency != "" {
		return q.Currency
	}
	return DefaultCurrency
}

// BySlug returns one public listing. ErrUnavailable means it was public once
// and no longer is; ErrNotFound means there is nothing public to say about it.
func (s *Service) BySlug(ctx context.Context, slug string) (Listing, error) {
	now := s.now().UTC()
	r, err := s.store.BySlug(ctx, slug)
	if err != nil {
		return Listing{}, err
	}
	if !r.Public(now) {
		if r.EverPublished() {
			return Listing{}, ErrUnavailable
		}
		return Listing{}, ErrNotFound
	}
	labels, err := s.labelSets(ctx)
	if err != nil {
		return Listing{}, err
	}
	return present(r, labels), nil
}

// Suggest returns up to MaxSuggestions suggestions for a search term.
func (s *Service) Suggest(ctx context.Context, term string) ([]Suggestion, error) {
	term = strings.TrimSpace(term)
	if len([]rune(term)) < 2 {
		return []Suggestion{}, nil
	}
	found, err := s.search.Suggest(ctx, term, MaxSuggestions, s.now().UTC())
	if err != nil {
		return nil, fmt.Errorf("suggest: %w", err)
	}
	// Regions and countries come from the reference data, after the suburbs
	// and cities and before the listings.
	places, err := s.placeSuggestions(ctx, term)
	if err != nil {
		return nil, fmt.Errorf("suggest places: %w", err)
	}
	out := []Suggestion{}
	for _, sg := range found {
		if sg.Type != "listing" {
			out = append(out, sg)
		}
	}
	out = append(out, places...)
	for _, sg := range found {
		if sg.Type == "listing" {
			out = append(out, sg)
		}
	}
	if len(out) > MaxSuggestions {
		out = out[:MaxSuggestions]
	}
	return out, nil
}

// ByOwner returns the public listings of an organisation or an agent.
func (s *Service) ByOwner(ctx context.Context, organisationID, agentUserID *uuid.UUID) ([]Listing, error) {
	records, err := s.store.PublishedByOwner(ctx, organisationID, agentUserID, s.now().UTC())
	if err != nil {
		return nil, err
	}
	return s.Present(ctx, records)
}

// Summaries returns the slug, title and reference of the given listings,
// whatever their status (an advertiser's inbox still names an expired listing).
func (s *Service) Summaries(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Summary, error) {
	out := map[uuid.UUID]Summary{}
	if len(ids) == 0 {
		return out, nil
	}
	records, err := s.store.ByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		if r.Slug != "" {
			out[r.ID] = Summary{Slug: r.Slug, Title: r.Title, Reference: r.Reference}
		}
	}
	return out, nil
}

// Slugs returns every public listing's slug and updated_at.
func (s *Service) Slugs(ctx context.Context) ([]SlugEntry, error) {
	out, err := s.store.PublishedSlugs(ctx, s.now().UTC())
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []SlugEntry{}
	}
	return out, nil
}

// Resolve validates a query against the reference data — the codes, then the
// location hierarchy (a suburb must belong to the city, a city to the region,
// a region to the country) — and returns it completed with what the search
// adapters need: the city -> region map and, when a country is selected and
// no currency was asked for, that country's currency.
func (s *Service) Resolve(ctx context.Context, q Query) (Query, map[string]string, error) {
	fields, err := s.CheckCodes(ctx, q)
	if err != nil {
		return q, nil, err
	}
	if err := s.checkPlaces(ctx, &q, fields); err != nil {
		return q, nil, err
	}
	return q, fields, nil
}

// CheckCodes validates the reference codes of a query against the active
// rows and returns per-field messages.
func (s *Service) CheckCodes(ctx context.Context, q Query) (map[string]string, error) {
	fields := map[string]string{}
	check := func(field, set string, values ...string) error {
		var active []string
		for _, v := range values {
			if v == "" {
				continue
			}
			if active == nil {
				var err error
				if active, err = s.labels.ActiveCodes(ctx, set); err != nil {
					return err
				}
			}
			if !slices.Contains(active, v) {
				fields[field] = "is not a known value; see GET /v1/reference/" + set
			}
		}
		return nil
	}
	for _, c := range []struct {
		field, set string
		values     []string
	}{
		{"deal", SetDealTypes, []string{q.Deal}}, {"category", SetPropertyCategories, []string{q.Category}},
		{"property_type", SetPropertyTypes, []string{q.PropertyType}}, {"city", SetCities, []string{q.City}},
		{"suburb", SetSuburbs, []string{q.Suburb}}, {"features", SetListingFeatures, q.Features},
	} {
		if err := check(c.field, c.set, c.values...); err != nil {
			return nil, err
		}
	}
	return fields, nil
}

// ParseAmount parses a non-negative decimal amount such as "385000" or "950.50".
func ParseAmount(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "eE+-/ ") {
		return nil, errors.New("not a decimal amount")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return nil, errors.New("not a decimal amount")
	}
	return r, nil
}

// FormatAmount renders an amount with two decimal places.
func FormatAmount(r *big.Rat) string { return r.FloatString(2) }
