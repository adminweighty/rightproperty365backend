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
	"math/big"
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
)

// Paging limits.
const (
	DefaultPageSize = 12
	MaxPageSize     = 48
	MaxSuggestions  = 8
)

// DefaultCurrency is assumed for min_price / max_price when no currency is given.
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
	ID           uuid.UUID  `json:"id"`
	Slug         string     `json:"slug"`
	Reference    string     `json:"reference"`
	Title        string     `json:"title"`
	Deal         Coded      `json:"deal"`
	Category     Coded      `json:"category"`
	PropertyType Coded      `json:"property_type"`
	Price        Money      `json:"price"`
	PricePeriod  *string    `json:"price_period"`
	Suburb       Coded      `json:"suburb"`
	City         Coded      `json:"city"`
	MarketCode   string     `json:"market_code"`
	Bedrooms     *int       `json:"bedrooms"`
	Bathrooms    *int       `json:"bathrooms"`
	FloorArea    *int       `json:"floor_area"`
	LandArea     *int       `json:"land_area"`
	Summary      string     `json:"summary"`
	Description  string     `json:"description"`
	Features     []Coded    `json:"features"`
	Featured     bool       `json:"featured"`
	Photos       []Photo    `json:"photos"`
	Advertiser   Advertiser `json:"advertiser"`
	PublishedAt  time.Time  `json:"published_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
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
	PricePeriod  string // "" or "month"
	Suburb       string
	City         string
	MarketCode   string
	Bedrooms     *int
	Bathrooms    *int
	FloorArea    *int
	LandArea     *int
	Summary      string
	Description  string
	Features     []string
	Featured     bool
	Photos       []Photo
	Advertiser   Advertiser

	Status      string
	PublishedAt time.Time
	ExpiresAt   time.Time
	UpdatedAt   time.Time

	// Ownership, used to list an agency's or an agent's listings.
	OrganisationID *uuid.UUID
	AgentUserID    *uuid.UUID
	SellerUserID   *uuid.UUID
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
	Price        PriceRange   `json:"price"`
}

// RawFacets are facet counts by code, before labels and ordering are applied.
type RawFacets struct {
	Deal, Category, PropertyType, City, Suburb, Features map[string]int
	Bedrooms                                             map[int]int
	PriceMin, PriceMax                                   *big.Rat
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
	// Type is "suburb", "city" or "listing".
	Type  string `json:"type"`
	Label string `json:"label"`
	// Value is the suburb or city code (use it as the `suburb` / `city`
	// filter), or the listing's slug.
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
	// PublishedByOwner returns the public listings of an organisation
	// (organisationID) or of one agent (agentUserID), newest first.
	PublishedByOwner(ctx context.Context, organisationID, agentUserID *uuid.UUID, now time.Time) ([]Record, error)
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
