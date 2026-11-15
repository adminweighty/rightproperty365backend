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
	// calendar is the availability calendar of the short stays; nil means
	// none (no availability on listings, no check_in / check_out search).
	calendar Calendar
	now      func() time.Time
}

// SetCalendar gives the Service the availability calendar behind a
// listing's `availability` and the check_in / check_out search.
func (s *Service) SetCalendar(c Calendar) { s.calendar = c }

// withAvailability sets `availability` on the short stays among listings.
// records and listings are parallel.
func (s *Service) withAvailability(ctx context.Context, records []Record, out []Listing) error {
	if s.calendar == nil || len(records) == 0 {
		return nil
	}
	summaries, err := s.calendar.Summaries(ctx, records, s.now().UTC())
	if err != nil {
		return fmt.Errorf("load availability: %w", err)
	}
	for i := range out {
		if summary, ok := summaries[out[i].ID]; ok {
			out[i].Availability = &summary
		}
	}
	return nil
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

// PlaceOf builds the public form of a place: the reference row with that
// code, or — when there is no code — the free text as it was entered.
func PlaceOf(labels map[string]string, code, freeText string) Place {
	if code == "" {
		return Place{Label: freeText}
	}
	return Place{Code: &code, Label: coded(labels, code).Label}
}

// present turns a stored record into its public form.
func present(r Record, labels map[string]map[string]string) Listing {
	l := Listing{
		ID: r.ID, Slug: r.Slug, Reference: r.Reference, Title: r.Title,
		Deal: coded(labels[SetDealTypes], r.Deal), Category: coded(labels[SetPropertyCategories], r.Category),
		PropertyType: coded(labels[SetPropertyTypes], r.PropertyType),
		Price:        r.Price, City: PlaceOf(labels[SetCities], r.City, r.CityName),
		MarketCode: r.MarketCode, Bedrooms: r.Bedrooms, Bathrooms: r.Bathrooms, FloorArea: r.FloorArea, LandArea: r.LandArea,
		Summary: r.Summary, Description: r.Description, Featured: r.Featured,
		Features: make([]Coded, 0, len(r.Features)), Photos: append([]Photo{}, r.Photos...), Advertiser: presentAdvertiser(r, labels),
		PublishedAt: r.PublishedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
	l.Verification = Verification{
		AdvertiserVerified: r.Advertiser.Verified, AdvertiserVerifiedAt: utcTime(r.Advertiser.VerifiedAt),
		ListingReviewed: r.ReviewedAt != nil, ListingReviewedAt: utcTime(r.ReviewedAt),
	}
	for i := range l.Photos {
		if l.Photos[i].Variants == nil {
			l.Photos[i].Variants = []PhotoVariant{}
		}
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
	if r.Suburb != "" {
		suburb := PlaceOf(labels[SetSuburbs], r.Suburb, "")
		l.Suburb = &suburb
	}
	// The region is the region of the city (or the free text that came with
	// a free-text city); the country is the market.
	if region := labels[keyCityRegion][r.City]; r.City != "" && region != "" {
		p := PlaceOf(labels[SetRegions], region, "")
		l.Region = &p
	} else if r.City == "" && r.RegionName != "" {
		l.Region = &Place{Label: r.RegionName}
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

func utcTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// Account roles and agent specialisms behind the role labels.
const (
	roleAgent         = "AGENT"
	roleSalesAgent    = "SALES_AGENT"
	roleLettingsAgent = "LETTINGS_AGENT"
	roleLandlord      = "LANDLORD"
	rolePrivateSeller = "PRIVATE_SELLER"

	specialismSales    = "SALES"
	specialismLettings = "LETTINGS"
)

// agentRole is the account role an agent's label comes from.
func agentRole(specialism string) string {
	switch specialism {
	case specialismSales:
		return roleSalesAgent
	case specialismLettings:
		return roleLettingsAgent
	}
	return roleAgent
}

// presentAdvertiser derives the public advertiser block from what the Store
// read. The precedence rules (also in the OpenAPI description):
//
//   - a business (organisation) listing: display.name is the business's name,
//     display.secondary_name the assigned agent's name, display.role_label
//     the label of the organisation's kind, display.logo the business's
//     logo, else the agent's photo, else null, display.verified the
//     business's verification, display.profile the business's public page;
//   - a listing with an agent and no business: the agent takes the
//     business's place;
//   - a direct owner: display.name is the owner's name, display.role_label
//     "Property Seller (Vendor)" for a sale and "Landlord" for a rental,
//     display.verified the owner's approved role, no logo and no profile.
//
// advertiser.name and advertiser.verified always equal display's.
func presentAdvertiser(r Record, labels map[string]map[string]string) Advertiser {
	a := r.Advertiser
	roles := labels[SetAccountRoles]
	link := func(ref *ProfileRef) *ProfileLink {
		if ref == nil || !ref.Verified { // an unverified profile has no public page
			return nil
		}
		return &ProfileLink{Kind: ref.Kind, Slug: ref.Slug}
	}
	// Copies: the record's own refs are never changed.
	if a.Agency != nil {
		ref := *a.Agency
		ref.Verified = a.Verified
		a.Agency = &ref
	}
	if a.Agent != nil {
		ref := *a.Agent
		// An agent is shown as verified only under a verified business.
		ref.Verified = ref.Verified && (a.Agency == nil || a.Verified)
		a.Agent = &ref
		a.AgentDisplay = &AgentDisplay{
			Name: ref.Name, RoleLabel: coded(roles, agentRole(ref.Specialism)).Label, Verified: ref.Verified,
			Logo: ref.Logo, Profile: link(&ref),
		}
	}
	switch {
	case a.Type == AdvertiserPrivateSeller:
		role := rolePrivateSeller
		if r.Deal == DealRent {
			role = roleLandlord
		}
		name := a.Name
		if name == "" {
			name = DefaultSellerName
		}
		a.Display = AdvertiserDisplay{Name: name, RoleLabel: coded(roles, role).Label, Verified: a.Verified}
		a.Agency, a.Agent, a.AgentDisplay = nil, nil, nil
	case a.Agency == nil && a.Agent != nil && a.Name == "":
		a.Display = AdvertiserDisplay{Name: a.Agent.Name, RoleLabel: a.AgentDisplay.RoleLabel, Logo: a.Agent.Logo, Verified: a.Verified, Profile: link(a.Agent)}
	default:
		a.Display = AdvertiserDisplay{Name: a.Name, RoleLabel: coded(labels[SetOrganisationKinds], a.OrganisationKind).Label, Verified: a.Verified}
		if a.Agency != nil {
			a.Display.Name, a.Display.Logo, a.Display.Profile = a.Agency.Name, a.Agency.Logo, link(a.Agency)
		}
		if a.Agent != nil {
			name := a.Agent.Name
			a.Display.SecondaryName = &name
			if a.Display.Logo == nil {
				a.Display.Logo = a.Agent.Logo
			}
		}
	}
	a.Name, a.Verified = a.Display.Name, a.Display.Verified
	return a
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
	if err := s.withAvailability(ctx, records, out); err != nil {
		return nil, err
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
	var shown []Record
	for _, id := range hits.IDs { // keep the search order; drop what the source of truth refused
		if r, ok := byID[id]; ok && r.Public(now) {
			page.Items = append(page.Items, present(r, labels))
			shown = append(shown, r)
		}
	}
	if err := s.withAvailability(ctx, shown, page.Items); err != nil {
		return Page{}, err
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
			if n > 0 && code != "" { // a free-text place has no code and no facet value
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
		Bedrooms: []FacetCount{}, Currencies: []CurrencyCount{},
		Country: build(raw.Country, keyCountryNames, false), Region: build(raw.Region, SetRegions, false),
	}
	// One currency per search. It is the one asked for, the country's, or —
	// when neither was given — the only currency the results are priced in.
	// Results in several currencies have no single price range: price is
	// null and the client asks the user to pick from `currencies`.
	for code, stat := range raw.Prices {
		if stat.Count > 0 && code != "" {
			f.Currencies = append(f.Currencies, CurrencyCount{Code: code, Count: stat.Count})
		}
	}
	sort.Slice(f.Currencies, func(i, j int) bool {
		if f.Currencies[i].Count != f.Currencies[j].Count {
			return f.Currencies[i].Count > f.Currencies[j].Count
		}
		return f.Currencies[i].Code < f.Currencies[j].Code
	})
	currency := q.Currency
	switch {
	case currency != "":
	case len(f.Currencies) == 1:
		currency = f.Currencies[0].Code
	case len(f.Currencies) == 0:
		currency = DefaultCurrency
	}
	if currency != "" {
		f.Price = &PriceRange{Currency: currency}
		if stat, ok := raw.Prices[currency]; ok && stat.Min != nil && stat.Max != nil {
			lo, hi := FormatAmount(stat.Min), FormatAmount(stat.Max)
			f.Price.Min, f.Price.Max = &lo, &hi
		}
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
	if err := publicError(r, now); err != nil {
		return Listing{}, err
	}
	listings, err := s.Present(ctx, []Record{r})
	if err != nil {
		return Listing{}, err
	}
	return listings[0], nil
}

// PublicRecord returns the stored record behind a public listing, with the
// same answers as BySlug: ErrUnavailable or ErrNotFound when it is not public.
func (s *Service) PublicRecord(ctx context.Context, slug string) (Record, error) {
	r, err := s.store.BySlug(ctx, slug)
	if err != nil {
		return Record{}, err
	}
	if err := publicError(r, s.now().UTC()); err != nil {
		return Record{}, err
	}
	return r, nil
}

// publicError says why a record is not public (nil when it is). A listing
// whose advertiser is not verified is ErrNotFound, whatever its history:
// "no longer available" would reveal that an advertiser lost its
// verification.
func publicError(r Record, now time.Time) error {
	switch {
	case r.Public(now):
		return nil
	case !r.Advertiser.Verified || !r.EverPublished():
		return ErrNotFound
	}
	return ErrUnavailable
}

// AdvertiserVerified reports whether the advertiser of a listing is verified
// right now. known is false when the catalogue has no such listing.
func (s *Service) AdvertiserVerified(ctx context.Context, id uuid.UUID) (verified, known bool, err error) {
	records, err := s.store.ByIDs(ctx, []uuid.UUID{id})
	if err != nil || len(records) == 0 {
		return false, false, err
	}
	return records[0].Advertiser.Verified, true, nil
}

// PublicCounts returns the listing numbers of the trust summary.
func (s *Service) PublicCounts(ctx context.Context) (PublicCounts, error) {
	return s.store.PublicCounts(ctx, s.now().UTC())
}

// Owned returns the listings an organisation's members or a direct owner
// manage, whatever their status. Authorisation is the caller's business.
func (s *Service) Owned(ctx context.Context, organisationIDs []uuid.UUID, sellerUserID *uuid.UUID) ([]Record, error) {
	return s.store.Owned(ctx, organisationIDs, sellerUserID)
}

// Record returns one listing whatever its status, or ErrNotFound.
func (s *Service) Record(ctx context.Context, id uuid.UUID) (Record, error) {
	records, err := s.store.ByIDs(ctx, []uuid.UUID{id})
	if err != nil {
		return Record{}, err
	}
	if len(records) == 0 {
		return Record{}, ErrNotFound
	}
	return records[0], nil
}

// SetStayRules changes a short stay's shortest stay and / or guest count.
func (s *Service) SetStayRules(ctx context.Context, id uuid.UUID, minNights, maxGuests *int) (Record, error) {
	return s.store.SetStayRules(ctx, id, minNights, maxGuests, s.now().UTC().Truncate(time.Microsecond))
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

// CountByCountry returns the number of public listings per country code.
func (s *Service) CountByCountry(ctx context.Context) (map[string]int, error) {
	return s.store.CountByCountry(ctx, s.now().UTC())
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
	if err := s.resolveStay(ctx, &q, fields); err != nil {
		return q, nil, err
	}
	if err := s.chooseCurrency(ctx, &q, fields); err != nil {
		return q, nil, err
	}
	return q, fields, nil
}

// resolveStay turns check_in / check_out into what the adapters apply: the
// short stays that are free for every night of the stay. Which listings are
// not free is asked of the availability calendar (the database), so every
// adapter excludes exactly the same listings and the search index needs no
// availability data. A stay implies the short-stay category.
func (s *Service) resolveStay(ctx context.Context, q *Query, fields map[string]string) error {
	if q.CheckIn == "" && q.CheckOut == "" {
		return nil
	}
	switch {
	case q.Category == "":
		q.Category = CategoryShortStay
	case q.Category != CategoryShortStay:
		fields["category"] = "must be " + CategoryShortStay + " when check_in and check_out are given"
	}
	if s.calendar == nil {
		fields["check_in"] = "availability search is not available"
		return nil
	}
	stay, problems, err := s.calendar.Stay(ctx, q.CheckIn, q.CheckOut, s.now().UTC())
	if err != nil {
		return fmt.Errorf("resolve the stay: %w", err)
	}
	for field, message := range problems {
		fields[field] = message
	}
	if len(problems) == 0 {
		q.Stay = &stay
	}
	return nil
}

// CurrencyNeededMessage is the message of the 400 on `currency` when a price
// filter has no single currency to apply to.
const CurrencyNeededMessage = "Choose a currency or a country to filter by price."

// chooseCurrency settles the one currency of a priced search. A currency
// that was asked for, or the country's, is already on the query. Otherwise
// the results (without the price filter) decide: one currency is used, and
// several are refused, because a price range across currencies means nothing
// and nothing is ever converted.
func (s *Service) chooseCurrency(ctx context.Context, q *Query, fields map[string]string) error {
	if !q.priced() || q.Currency != "" || len(fields) > 0 {
		return nil
	}
	probe := *q
	probe.MinPrice, probe.MaxPrice, probe.Page, probe.PageSize = nil, nil, 1, 1
	hits, err := s.search.Search(ctx, probe, s.now().UTC())
	if err != nil {
		return fmt.Errorf("search listings: %w", err)
	}
	var currencies []string
	for code, stat := range hits.Facets.Prices {
		if stat.Count > 0 && code != "" {
			currencies = append(currencies, code)
		}
	}
	switch len(currencies) {
	case 0:
		q.Currency = DefaultCurrency // nothing matches anyway
	case 1:
		q.Currency = currencies[0]
	default:
		fields["currency"] = CurrencyNeededMessage
	}
	return nil
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
