// Package listings serves the public property listings: search with filters,
// sorting, pagination and facets, one listing by slug, and search suggestions.
//
// Two rules shape the package:
//
//   - Only PUBLISHED and unexpired listings are ever returned (FR-006). The
//     Store — the source of truth — enforces status AND expiry in its own
//     queries, and every result of a Search adapter is re-read through the
//     Store before it is returned. A search index that is behind (the
//     Elasticsearch adapter planned for later) therefore cannot resurrect an
//     expired or unpublished listing.
//   - A listing names who advertises it only as PUBLIC profile references
//     (kind, slug, name, verified). No phone number, email address or street
//     address exists in this package's types at all.
package listings

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Listing statuses (listings.status). Only StatusPublished is public.
const StatusPublished = "PUBLISHED"

// Reference sets the listing codes come from.
const (
	SetDealTypes          = "deal_types"
	SetPropertyCategories = "property_categories"
	SetPropertyTypes      = "property_types"
	SetCities             = "cities"
	SetSuburbs            = "suburbs"
	SetListingFeatures    = "listing_features"
)

// Sort orders.
const (
	SortNewest    = "newest"
	SortPriceAsc  = "price_asc"
	SortPriceDesc = "price_desc"
	SortRelevance = "relevance"
	// SortOldest is the reverse of newest; SortAreaDesc puts the largest
	// floor area first (listings without one last).
	SortOldest   = "oldest"
	SortAreaDesc = "area_desc"
)

// Sorts returns every sort order.
func Sorts() []string {
	return []string{SortNewest, SortOldest, SortPriceAsc, SortPriceDesc, SortAreaDesc, SortRelevance}
}

// Values of the advertiser_type filter.
const (
	AdvertiserFilterAgency  = "agency"
	AdvertiserFilterPrivate = "private"
)

// Paging limits.
const (
	DefaultPageSize = 12
	MaxPageSize     = 48
	MaxSuggestions  = 8
)

// CategoryShortStay is the category of listings let by the night (holiday
// homes, serviced apartments, guest houses, rooms). Such a listing is always
// deal "rent" priced per night.
const CategoryShortStay = "short-stay"

// Price periods (the price_periods reference set). "" is a sale price.
const (
	PricePeriodMonth = "month"
	PricePeriodNight = "night"
)

// DealRent is the deal of everything that is let rather than sold.
const DealRent = "rent"

// CheckRecord enforces the rules a stored listing must meet whatever wrote
// it. Today only the sample-data seed writes listings (there are still no
// create or publish endpoints); the database has the same rules as CHECK
// constraints (migration 0013).
func CheckRecord(r Record) error {
	switch r.PricePeriod {
	case "", PricePeriodMonth, PricePeriodNight:
	default:
		return fmt.Errorf("listing %s: price_period %q is not one of month, night", r.Slug, r.PricePeriod)
	}
	if r.Category == CategoryShortStay {
		if r.Deal != DealRent || r.PricePeriod != PricePeriodNight {
			return fmt.Errorf("listing %s: a short stay must be deal %q priced per %q (got %q, %q)", r.Slug, DealRent, PricePeriodNight, r.Deal, r.PricePeriod)
		}
		if (r.MaxGuests != nil && *r.MaxGuests < 1) || (r.MinNights != nil && *r.MinNights < 1) {
			return fmt.Errorf("listing %s: max_guests and min_nights must be at least 1", r.Slug)
		}
		return checkPlace(r)
	}
	if r.PricePeriod == PricePeriodNight {
		return fmt.Errorf("listing %s: only a short stay is priced per night", r.Slug)
	}
	if r.MaxGuests != nil || r.MinNights != nil {
		return fmt.Errorf("listing %s: max_guests and min_nights are for short stays only", r.Slug)
	}
	return checkPlace(r)
}

// MaxPlaceNameLength bounds a free-text city or region name.
const MaxPlaceNameLength = 80

// CheckPlaceName validates a free-text city or region name: plain text of at
// most MaxPlaceNameLength characters, with no markup and no control
// characters. It returns "" when the name is fine.
func CheckPlaceName(name string) string {
	switch {
	case strings.TrimSpace(name) != name:
		return "must not start or end with a space"
	case len([]rune(name)) > MaxPlaceNameLength:
		return fmt.Sprintf("must be at most %d characters", MaxPlaceNameLength)
	case strings.ContainsAny(name, "<>"):
		return "must be plain text, without HTML"
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return "must be plain text on one line"
		}
	}
	return ""
}

var countryCodeRe = regexp.MustCompile(`^[A-Z]{2}$`)

// checkPlace enforces where a listing is: a country always; a city either as
// a reference code or, for a country without reference places, as free text
// (never both); a suburb only under a city code; a free-text region only
// with a free-text city.
func checkPlace(r Record) error {
	if !countryCodeRe.MatchString(r.MarketCode) {
		return fmt.Errorf("listing %s: a country (ISO 3166-1 alpha-2 code) is required, got %q", r.Slug, r.MarketCode)
	}
	switch {
	case r.City == "" && r.CityName == "":
		return fmt.Errorf("listing %s: a city is required, as a reference code or as free text", r.Slug)
	case r.City != "" && (r.CityName != "" || r.RegionName != ""):
		return fmt.Errorf("listing %s: a city code and a free-text city or region cannot be combined", r.Slug)
	case r.City == "" && r.Suburb != "":
		return fmt.Errorf("listing %s: a suburb needs a city code", r.Slug)
	}
	if problem := CheckPlaceName(r.CityName); problem != "" {
		return fmt.Errorf("listing %s: city_name %s", r.Slug, problem)
	}
	if problem := CheckPlaceName(r.RegionName); problem != "" {
		return fmt.Errorf("listing %s: region_name %s", r.Slug, problem)
	}
	return nil
}

// DefaultCurrency is the currency named by an empty price facet: when no
// currency or country was chosen and nothing matched, there is no currency
// to take from the results. It is never used to filter silently any more.
const DefaultCurrency = "USD"

// Errors.
var (
	// ErrNotFound: no public trace of the listing (unknown slug, or never published).
	ErrNotFound = errors.New("listing not found")
	// ErrUnavailable: the listing was public once and no longer is (expired,
	// sold, let, paused or closed).
	ErrUnavailable = errors.New("listing is no longer available")
)

// Coded is a reference-data code with its current label.
type Coded struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// Place is a city, a suburb or a region of a listing or a profile. Code is
// the reference-data code (use it as the filter of the same name) and Label
// its current label. For a place that was typed as free text — a city in a
// country that has no reference places yet — Code is null and Label is the
// text as it was entered; such a place cannot be filtered on, only found
// with q.
type Place struct {
	Code  *string `json:"code"`
	Label string  `json:"label"`
}

// Is reports whether the place is the reference row with that code.
func (p Place) Is(code string) bool { return p.Code != nil && *p.Code == code }

// CodeOr returns the place's code, or "" for a free-text place.
func (p Place) CodeOr() string {
	if p.Code == nil {
		return ""
	}
	return *p.Code
}

// Money is an exact decimal amount with its ISO 4217 currency. Never a float.
type Money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// Photo is one image of a listing.
type Photo struct {
	URL    string `json:"url"`
	Alt    string `json:"alt"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// ProfileRef is a PUBLIC reference to a directory profile. It is everything
// a listing says about a person or business: no contact details.
type ProfileRef struct {
	Kind     string `json:"kind"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Verified bool   `json:"verified"`
}

// Advertiser types.
const (
	AdvertiserAgency        = "agency"
	AdvertiserPrivateSeller = "private_seller"
)

// Advertiser says who advertises a listing.
type Advertiser struct {
	// Type is "agency" or "private_seller".
	Type string `json:"type"`
	// Name is the agency's name, or the private seller's display name.
	Name     string `json:"name"`
	Verified bool   `json:"verified"`
	// Agency and Agent are directory profiles; both are null for a private seller.
	Agency *ProfileRef `json:"agency"`
	Agent  *ProfileRef `json:"agent"`
}

// Listing is the public representation of a listing.
type Listing struct {
	ID           uuid.UUID `json:"id"`
	Slug         string    `json:"slug"`
	Reference    string    `json:"reference"`
	Title        string    `json:"title"`
	Deal         Coded     `json:"deal"`
	Category     Coded     `json:"category"`
	PropertyType Coded     `json:"property_type"`
	Price        Money     `json:"price"`
	PricePeriod  *string   `json:"price_period"`
	// Suburb is null when the listing names none (always the case for a
	// free-text city).
	Suburb *Place `json:"suburb"`
	// City is always present: a reference city (code set) or a free-text one
	// (code null).
	City Place `json:"city"`
	// Region is the region (province) of the city, or the free-text region
	// that came with a free-text city; null when there is none.
	Region *Place `json:"region"`
	// Country is the listing's market as a country; MarketCode is its code.
	Country    Country `json:"country"`
	MarketCode string  `json:"market_code"`
	Bedrooms   *int    `json:"bedrooms"`
	Bathrooms  *int    `json:"bathrooms"`
	FloorArea  *int    `json:"floor_area"`
	LandArea   *int    `json:"land_area"`
	// MaxGuests and MinNights are set for short stays only (null otherwise):
	// how many guests the place sleeps and the shortest stay, in nights.
	MaxGuests   *int       `json:"max_guests"`
	MinNights   *int       `json:"min_nights"`
	Summary     string     `json:"summary"`
	Description string     `json:"description"`
	Features    []Coded    `json:"features"`
	Featured    bool       `json:"featured"`
	Photos      []Photo    `json:"photos"`
	Advertiser  Advertiser `json:"advertiser"`
	PublishedAt time.Time  `json:"published_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// Availability is set for short stays only (null otherwise): when the
	// lister last changed the calendar and the first night that is free.
	// The calendar itself is GET /v1/listings/{slug}/availability.
	Availability *AvailabilitySummary `json:"availability"`
}

// AvailabilitySummary is what a listing says about its availability
// calendar. It carries no reason, note or author: those are the lister's.
type AvailabilitySummary struct {
	// UpdatedAt is when the lister last changed the calendar; null when they
	// never have (every night in the window is then available).
	UpdatedAt *time.Time `json:"updated_at"`
	// NextAvailable is the first available night from today on, in the
	// listing's time zone (YYYY-MM-DD); null when no night in the booking
	// window is free.
	NextAvailable *string `json:"next_available"`
}

// Stay is what a search with check_in and check_out is narrowed by.
type Stay struct {
	// Nights is the length of the stay.
	Nights int
	// ExcludeIDs are the short stays that are not available for every night
	// of the stay, resolved from the database (the source of truth), so the
	// search engine holds no availability data.
	ExcludeIDs []uuid.UUID
	// ExcludeCountries are the countries where the stay starts in the past or
	// ends beyond the booking window (today differs by time zone).
	ExcludeCountries []string
}

// Calendar is the availability calendar of the short stays, as the listings
// module needs it. The availability module implements it.
type Calendar interface {
	// Summaries returns the availability summary of each short stay among records.
	Summaries(ctx context.Context, records []Record, now time.Time) (map[uuid.UUID]AvailabilitySummary, error)
	// Stay validates check_in and check_out (YYYY-MM-DD) and resolves what a
	// search for that stay must leave out. Problems are per-field messages.
	Stay(ctx context.Context, checkIn, checkOut string, now time.Time) (Stay, map[string]string, error)
}

// Record is a listing as stored: codes instead of labels, plus the fields
// that decide whether it is public and who it belongs to.
type Record struct {
	ID           uuid.UUID
	Slug         string
	Reference    string
	Title        string
	Deal         string
	Category     string
	PropertyType string
	Price        Money
	PricePeriod  string // "", "month" or "night"
	// Short stays only.
	MaxGuests *int
	MinNights *int
	Suburb    string
	City      string
	// CityName and RegionName are free text, for a listing in a country that
	// has no reference places: set instead of City (never with it).
	CityName    string
	RegionName  string
	MarketCode  string
	Bedrooms    *int
	Bathrooms   *int
	FloorArea   *int
	LandArea    *int
	Summary     string
	Description string
	Features    []string
	Featured    bool
	Photos      []Photo
	Advertiser  Advertiser

	Status      string
	PublishedAt time.Time
	ExpiresAt   time.Time
	UpdatedAt   time.Time

	// Ownership, used to list an agency's or an agent's listings.
	OrganisationID *uuid.UUID
	AgentUserID    *uuid.UUID
	SellerUserID   *uuid.UUID
}

// Version is a number that grows with every change of the record: its
// updated_at in microseconds. The search index uses it as an external
// version, so an older write can never overwrite a newer one.
func (r Record) Version() int64 { return r.UpdatedAt.UnixMicro() }

// Summary is the public name of a listing: enough to show which listing a
// request is about.
type Summary struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Reference string `json:"reference"`
}

// Public reports whether the record may be shown at the given instant.
func (r Record) Public(now time.Time) bool {
	return r.Status == StatusPublished && r.ExpiresAt.After(now)
}

// EverPublished reports whether the listing has been public at some point, so
// that saying "no longer available" about it reveals nothing new.
func (r Record) EverPublished() bool {
	switch r.Status {
	case StatusPublished, "EXPIRED", "PAUSED", "SOLD", "LET", "CLOSED":
		return !r.PublishedAt.IsZero()
	}
	return false
}

// Query is a search. Codes are reference-data codes; empty means "any".
type Query struct {
	Q            string
	Deal         string
	Category     string
	PropertyType string
	City         string
	Suburb       string
	// MinPrice and MaxPrice compare only listings priced in Currency.
	MinPrice     *big.Rat
	MaxPrice     *big.Rat
	Currency     string
	BedroomsMin  *int
	BathroomsMin *int

	// Location above the city. Country is a market code (upper case); Region
	// a regions code. A listing's region is the region of its city.
	Country string
	Region  string
	// Advanced filters. Areas are square metres.
	BedroomsMax  *int
	BathroomsMax *int
	FloorAreaMin *int
	FloorAreaMax *int
	LandAreaMin  *int
	LandAreaMax  *int
	// ListedWithinDays keeps listings published in the last N days.
	ListedWithinDays *int
	// AdvertiserType is "agency" or "private" ("" for any).
	AdvertiserType string
	// Reference is an exact listing reference, compared without case.
	Reference string
	// HasPhotos keeps listings with at least one photo.
	HasPhotos bool
	// GuestsMin keeps short stays that sleep at least this many guests.
	GuestsMin *int
	// CheckIn and CheckOut (YYYY-MM-DD) ask for short stays that are free for
	// every night of that stay. The Service resolves them into Stay, which is
	// what the adapters apply; clients cannot set Stay.
	CheckIn  string
	CheckOut string
	Stay     *Stay
	// CityRegion (city code -> region code) is filled by the Service from the
	// reference data; the memory adapter evaluates regions with it. Clients
	// cannot set it.
	CityRegion map[string]string
	// Features must ALL be present.
	Features []string
	Featured *bool
	Sort     string
	Page     int
	PageSize int
}

// priced reports whether the query filters on price.
func (q Query) priced() bool { return q.MinPrice != nil || q.MaxPrice != nil }

// FacetCount is one value of a facet with the number of matching listings.
type FacetCount struct {
	Code  string `json:"code"`
	Label string `json:"label"`
	Count int    `json:"count"`
	// City is the code of the city a suburb belongs to. It is set on the
	// values of the suburb facet only, so a client can group them.
	City *string `json:"city,omitempty"`
}

// PriceRange is the lowest and highest price among the matching listings in
// one currency. Both are null when nothing matches.
type PriceRange struct {
	Currency string  `json:"currency"`
	Min      *string `json:"min"`
	Max      *string `json:"max"`
}

// Facets are the counts behind the filter controls. Each facet counts the
// listings that match every OTHER filter, so choosing a value in one filter
// does not empty the alternatives in that same filter.
type Facets struct {
	Deal         []FacetCount `json:"deal"`
	Category     []FacetCount `json:"category"`
	PropertyType []FacetCount `json:"property_type"`
	City         []FacetCount `json:"city"`
	Suburb       []FacetCount `json:"suburb"`
	Bedrooms     []FacetCount `json:"bedrooms"`
	Features     []FacetCount `json:"features"`
	// Price is the price range in the search's one currency. It is NULL when
	// no currency or country was chosen and the results span more than one
	// currency: there is no single range to show, and Currencies says which
	// currencies a client can ask the user to pick from.
	Price *PriceRange `json:"price"`
	// Currencies counts the results by the currency they are priced in
	// (ignoring the price filter, like Price), most listings first.
	Currencies []CurrencyCount `json:"currencies"`
	// Country and Region count by market and by the region of the city.
	Country []FacetCount `json:"country"`
	Region  []FacetCount `json:"region"`
}

// CurrencyCount is one currency with the number of matching listings priced in it.
type CurrencyCount struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// PriceStat is the number of listings priced in one currency and their
// lowest and highest price.
type PriceStat struct {
	Count    int
	Min, Max *big.Rat
}

// RawFacets are facet counts by code, before labels and ordering are applied.
type RawFacets struct {
	Deal, Category, PropertyType, City, Suburb, Features map[string]int
	Bedrooms                                             map[int]int
	// Prices is keyed by currency and holds the listings that match every
	// filter except the price filter.
	Prices map[string]PriceStat
	// Country is keyed by market code, Region by region code.
	Country, Region map[string]int
}

// Hits is what a Search adapter returns: the IDs of one page in order, the
// total number of matches, and the facet counts.
type Hits struct {
	IDs    []uuid.UUID
	Total  int
	Facets RawFacets
}

// Suggestion is one search suggestion.
type Suggestion struct {
	// Type is "suburb", "city", "region", "country" or "listing".
	Type  string `json:"type"`
	Label string `json:"label"`
	// Value is the suburb, city, region or country code (use it as the filter
	// of the same name), or the listing's slug.
	Value string `json:"value"`
}

// Search is the port a search engine implements. The Postgres and memory
// adapters implement it today; an Elasticsearch adapter can be added behind
// it later. An adapter only proposes IDs: the Service re-reads every hit from
// the Store, which re-checks status and expiry.
type Search interface {
	Search(ctx context.Context, q Query, now time.Time) (Hits, error)
	Suggest(ctx context.Context, term string, limit int, now time.Time) ([]Suggestion, error)
}

// Store is the source of truth.
type Store interface {
	// PublishedByIDs returns the records among ids that are PUBLISHED and not
	// expired at now. Status and expiry are checked in the query itself.
	PublishedByIDs(ctx context.Context, ids []uuid.UUID, now time.Time) ([]Record, error)
	// BySlug returns the record whatever its status, or ErrNotFound.
	BySlug(ctx context.Context, slug string) (Record, error)
	// ByIDs returns the records among ids whatever their status. It is used
	// to keep the search index in step and to name a listing to its own
	// advertiser; it is never used to answer a public search.
	ByIDs(ctx context.Context, ids []uuid.UUID) ([]Record, error)
	// AllPublished returns every public listing (rebuilding the search index).
	AllPublished(ctx context.Context, now time.Time) ([]Record, error)
	// PublishedByOwner returns the public listings of an organisation
	// (organisationID) or of one agent (agentUserID), newest first.
	PublishedByOwner(ctx context.Context, organisationID, agentUserID *uuid.UUID, now time.Time) ([]Record, error)
	// CountByCountry returns the number of public listings per country code
	// (GET /v1/countries: which countries have listings right now).
	CountByCountry(ctx context.Context, now time.Time) (map[string]int, error)
	// Owned returns the listings, WHATEVER THEIR STATUS, that belong to one of
	// the organisations or are sold directly by the user, newest first. It is
	// for the lister's own pages, never for the public.
	Owned(ctx context.Context, organisationIDs []uuid.UUID, sellerUserID *uuid.UUID) ([]Record, error)
	// SetStayRules changes the shortest stay and / or the number of guests of
	// a short stay (nil leaves a value as it is) and returns the new record.
	SetStayRules(ctx context.Context, id uuid.UUID, minNights, maxGuests *int, now time.Time) (Record, error)
	// PublishedSlugs returns the slug and updated_at of every public listing (sitemap).
	PublishedSlugs(ctx context.Context, now time.Time) ([]SlugEntry, error)
}

// SlugEntry is one sitemap entry.
type SlugEntry struct {
	Slug      string    `json:"slug"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Labels supplies reference-data labels and the codes that are active now.
type Labels interface {
	Labels(ctx context.Context, set string) (map[string]string, error)
	ActiveCodes(ctx context.Context, set string) ([]string, error)
}
