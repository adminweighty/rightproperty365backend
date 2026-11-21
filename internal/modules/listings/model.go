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
//     (kind, slug, name, verified, logo) and, for a direct owner, the owner's
//     name. No phone number, email address or street address exists in this
//     package's types at all.
//   - A listing is public only while its advertiser is verified. The Store
//     reads the advertiser's verification together with the listing, every
//     time: no copy on the listing or in the search index is trusted.
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

// Photo is one image of a listing. URL, Width and Height are the largest
// size (what clients read since the first version). For an uploaded photo
// URL is an API path such as "/v1/listing-photos/{id}/large?v=3f2a9c0d1b7e"
// (relative to the API origin, like a logo URL) and Variants lists every
// stored size, widest first, for a srcset; a bundled sample image
// ("/assets/images/...", served by the web app) has no variants.
type Photo struct {
	URL      string         `json:"url"`
	Alt      string         `json:"alt"`
	Width    int            `json:"width"`
	Height   int            `json:"height"`
	Variants []PhotoVariant `json:"variants"`
}

// PhotoVariant is one stored size of an uploaded photo.
type PhotoVariant struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	URL    string `json:"url"`
}

// Logo is the public logo (or, for an agent, photo) of a directory profile.
type Logo struct {
	// URL is a path RELATIVE TO THE API ORIGIN, for example
	// "/v1/directory/agencies/sample-estates/logo?v=3f2a9c0d1b7e": a client
	// prefixes its API base URL. The v parameter (LogoVersion) changes
	// whenever the image changes, so a cached copy is never stale.
	URL string `json:"url"`
	// Width and Height are the image's size in pixels (null when unknown).
	Width  *int `json:"width"`
	Height *int `json:"height"`
}

// LogoVersionLength is the number of checksum characters in a logo URL's v
// parameter. Twelve hexadecimal characters can never look like a ten-digit
// phone number to a scanner.
const LogoVersionLength = 12

// LogoVersion is the v parameter of the logo URL of an image with this
// checksum: its first LogoVersionLength characters.
func LogoVersion(checksum string) string {
	if len(checksum) > LogoVersionLength {
		return checksum[:LogoVersionLength]
	}
	return checksum
}

// NewLogo builds the public form of a profile's logo from what is stored: the
// profile's address, the image's checksum and its size. It returns nil when
// the profile has no logo (an empty checksum).
func NewLogo(kind, slug, checksum string, width, height int) *Logo {
	if checksum == "" {
		return nil
	}
	l := &Logo{URL: "/v1/directory/" + kind + "/" + slug + "/logo?v=" + LogoVersion(checksum)}
	if width > 0 && height > 0 {
		l.Width, l.Height = &width, &height
	}
	return l
}

// ProfileRef is a PUBLIC reference to a directory profile. It is everything
// a listing says about a person or business: no contact details.
type ProfileRef struct {
	Kind     string `json:"kind"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Verified bool   `json:"verified"`
	// Logo is the profile's own logo or photo; null when it has none (or the
	// profile is not public). For an agent it is the agent's own photo only:
	// a client falls back to the agency's logo, then to initials.
	Logo *Logo `json:"logo"`

	// Specialism is an agent's specialism ("SALES", "LETTINGS" or ""). It
	// decides the agent's role label and is not serialised.
	Specialism string `json:"-"`
}

// ProfileLink is the address of a public profile page.
type ProfileLink struct {
	Kind string `json:"kind"`
	Slug string `json:"slug"`
}

// Advertiser types.
const (
	AdvertiserAgency        = "agency"
	AdvertiserPrivateSeller = "private_seller"
)

// DefaultSellerName is shown for a direct owner whose account name is not known.
const DefaultSellerName = "Private seller"

// AdvertiserDisplay is everything a listing card needs to show who
// advertises the listing, already resolved (see Advertiser).
type AdvertiserDisplay struct {
	// Name is the business's name; for a direct owner, the owner's name.
	Name string `json:"name"`
	// SecondaryName is the assigned agent's name when the listing has both a
	// business and an agent; null otherwise.
	SecondaryName *string `json:"secondary_name"`
	// RoleLabel says what the advertiser is, in the reference data's words:
	// "Real Estate Agency", "Law Firm", "Host", "Landlord", ...
	RoleLabel string `json:"role_label"`
	// Logo is the best logo there is: the business's, else the assigned
	// agent's photo, else null (show initials).
	Logo *Logo `json:"logo"`
	// Verified is true only when the business (or the direct owner) is verified.
	Verified bool `json:"verified"`
	// Profile is the public page to link to; null when there is none.
	Profile *ProfileLink `json:"profile"`
}

// AgentDisplay is the assigned agent of a listing as a person, for a detail page.
type AgentDisplay struct {
	Name string `json:"name"`
	// RoleLabel is "Estate Agent", or "Sales Agent" / "Lettings Agent" for an
	// agent with that specialism.
	RoleLabel string `json:"role_label"`
	// Verified is true only when the agent's own profile is verified AND the
	// business is.
	Verified bool `json:"verified"`
	// Logo is the agent's own photo; null when they have none.
	Logo    *Logo        `json:"logo"`
	Profile *ProfileLink `json:"profile"`
}

// Advertiser says who advertises a listing.
//
// Type, Agency, Agent and the unserialised fields are what the Store read
// together with the listing (never a copy kept on the listing: a business
// that stops being verified stops showing as verified on its listings at
// once). For a direct owner the Store also sets Name (the owner's account
// name) and Verified. Name, Verified, Display and AgentDisplay are derived
// when the listing is presented.
type Advertiser struct {
	// Type is "agency" (any business: an agency, a law firm, a property
	// manager, a developer, a host) or "private_seller" (a direct owner).
	Type string `json:"type"`
	// Name equals Display.Name.
	Name string `json:"name"`
	// Verified equals Display.Verified: the business's own verification, or
	// the direct owner's approved PRIVATE_SELLER / LANDLORD role.
	Verified bool `json:"verified"`
	// Agency and Agent are directory profiles; both are null for a direct owner.
	Agency *ProfileRef `json:"agency"`
	Agent  *ProfileRef `json:"agent"`
	// Display is what a card shows; AgentDisplay the assigned agent as a
	// person (null when the listing has no assigned agent with a profile).
	Display      AdvertiserDisplay `json:"display"`
	AgentDisplay *AgentDisplay     `json:"agent_display"`

	// OrganisationKind is the kind of the owning organisation ("AGENCY",
	// "LAW_FIRM", ...; "" for a direct owner). Not serialised.
	OrganisationKind string `json:"-"`
	// VerifiedAt is when the business's profile was verified, or the direct
	// owner's role approved; nil when not verified or not recorded. It is
	// shown as Listing.Verification.AdvertiserVerifiedAt.
	VerifiedAt *time.Time `json:"-"`
}

// Verification is what a listing says about the checks behind it.
type Verification struct {
	// AdvertiserVerified is true on every listing the API returns publicly:
	// a listing whose advertiser is not verified is not public.
	AdvertiserVerified bool `json:"advertiser_verified"`
	// AdvertiserVerifiedAt is when the business was verified or the direct
	// owner's role approved; null when that time was not recorded.
	AdvertiserVerifiedAt *time.Time `json:"advertiser_verified_at"`
	// ListingReviewed says a platform administrator or moderator reviewed
	// the listing itself; ListingReviewedAt when.
	ListingReviewed   bool       `json:"listing_reviewed"`
	ListingReviewedAt *time.Time `json:"listing_reviewed_at"`
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
	MaxGuests   *int    `json:"max_guests"`
	MinNights   *int    `json:"min_nights"`
	Summary     string  `json:"summary"`
	Description string  `json:"description"`
	Features    []Coded `json:"features"`
	Featured    bool    `json:"featured"`
	// Placement is the active paid placement (API 0.53.0): {kind, label},
	// null when there is none (a sample listing may be featured without one).
	Placement  *PlacementView `json:"placement"`
	Photos     []Photo        `json:"photos"`
	Advertiser Advertiser     `json:"advertiser"`

	// Video is the listing's YouTube video tour, null when it has none;
	// HasVideo says whether it has one (for the cards of a search).
	Video    *Video `json:"video"`
	HasVideo bool   `json:"has_video"`
	// Location is where the listing is on a map, null when the lister set no
	// coordinates. For an APPROXIMATE listing the coordinates are rounded to
	// two decimals (about 1 km) by the server: the exact ones are never public.
	Location *Location `json:"location"`
	// Verification says which checks stand behind the listing.
	Verification Verification `json:"verification"`
	PublishedAt  time.Time    `json:"published_at"`
	ExpiresAt    time.Time    `json:"expires_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
	// Availability is set for short stays only (null otherwise): when the
	// lister last changed the calendar and the first night that is free.
	// The calendar itself is GET /v1/listings/{slug}/availability.
	Availability *AvailabilitySummary `json:"availability"`
	// Applications says whether the listing takes applications now (API
	// 0.32.0); null when no questionnaire is attached (always for short stays).
	Applications *ApplicationsSummary `json:"applications"`
	// HourlyBooking says whether the listing can be booked by the hour (API
	// 0.33.0); null unless its property type is hourly_bookable and the
	// lister configured hourly settings. The availability is GET
	// /v1/venues/{id}/availability.
	HourlyBooking *HourlyBookingSummary `json:"hourly_booking"`
}

// HourlyBookingSummary is what a listing says about hourly booking.
type HourlyBookingSummary struct {
	Enabled          bool   `json:"enabled"`
	RatePerHour      string `json:"rate_per_hour"`
	Currency         string `json:"currency"`
	IncrementMinutes int    `json:"increment_minutes"`
}

// HourlyRates supplies the HourlyBookingSummary of listings (the venues module).
type HourlyRates interface {
	HourlySummaries(ctx context.Context, records []Record) (map[uuid.UUID]HourlyBookingSummary, error)
}

// ApplicationsSummary is what a public listing says about its application
// intake: whether it is open, when it closes and the questionnaire's name.
type ApplicationsSummary struct {
	Open     bool       `json:"open"`
	ClosesAt *time.Time `json:"closes_at"`
	FormName *string    `json:"form_name"`
}

// Intake supplies the ApplicationsSummary of listings (the applications module).
type Intake interface {
	ApplicationSummaries(ctx context.Context, records []Record) (map[uuid.UUID]ApplicationsSummary, error)
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
	// PlacementKind is the active placement's kind ("" for none, API 0.53.0).
	PlacementKind string
	Photos        []Photo
	Advertiser    Advertiser
	// YouTubeVideoID is the id of the listing's YouTube video tour ("" for
	// none); YouTubeStartSeconds where it starts (nil: at the beginning).
	YouTubeVideoID      string
	YouTubeStartSeconds *int
	// Latitude and Longitude are the exact coordinates the lister set (both
	// or neither; at most six decimals). LocationPrecision is EXACT or
	// APPROXIMATE ("" reads as EXACT). The public sees PublicLocation.
	Latitude          *float64
	Longitude         *float64
	LocationPrecision string

	Status      string
	PublishedAt time.Time
	ExpiresAt   time.Time
	UpdatedAt   time.Time
	// Cycles is the number of publication cycles recorded for the listing
	// (publication_cycles: max(relist_no) + 1; 0 without any, such as the
	// sample data). See CycleNo.
	Cycles int

	// Ownership, used to list an agency's or an agent's listings.
	OrganisationID *uuid.UUID
	AgentUserID    *uuid.UUID
	SellerUserID   *uuid.UUID

	// ReviewedAt and ReviewedBy record the last approval of the listing
	// by a platform reviewer (listing management), or the sample-data seed.
	ReviewedAt *time.Time
	ReviewedBy *uuid.UUID
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

// Public reports whether the record may be shown at the given instant: it is
// PUBLISHED, not expired, AND its advertiser is verified (the owning business
// is verified, or the direct owner's role is approved). An assigned agent's
// own verification does not gate the listing.
func (r Record) Public(now time.Time) bool {
	return r.Status == StatusPublished && r.ExpiresAt.After(now) && r.Advertiser.Verified
}

// EffectiveStatus is the status as the advertiser sees it: a PUBLISHED
// listing whose publication period is over is EXPIRED. (ManagedListing
// embeds Record and uses the same rule.)
func (r Record) EffectiveStatus(now time.Time) string {
	if r.Status == StatusPublished && !r.ExpiresAt.After(now) {
		return StatusExpired
	}
	// A paused listing's period keeps running (API 0.48.0).
	if r.Status == StatusPaused && !r.ExpiresAt.IsZero() && !r.ExpiresAt.After(now) {
		return StatusExpired
	}
	return r.Status
}

// HoldsSlot reports whether the listing takes a place in its account's
// package at now: live, or paused within its period (API 0.48.0).
func (r Record) HoldsSlot(now time.Time) bool {
	return (r.Status == StatusPublished || r.Status == StatusPaused) && r.ExpiresAt.After(now)
}

// EverPublished reports whether the listing has been public at some point, so
// that saying "no longer available" about it reveals nothing new.
func (r Record) EverPublished() bool {
	switch r.Status {
	// A listing that waits for review, or was archived, after it was public
	// (a relist) is "no longer available" too; one that never was public
	// has no publication time and stays unknown.
	case StatusPublished, "EXPIRED", "PAUSED", "SOLD", "LET", "CLOSED", StatusArchived, StatusPendingReview, StatusChangesRequested:
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
	// BBox keeps listings whose public coordinates are inside the box;
	// HasLocation keeps listings that have coordinates at all (the map view
	// before it has a box). Neither is a facet: both narrow every count.
	BBox        *BBox
	HasLocation bool
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
	// Mappable is how many of the Total matches have coordinates.
	Mappable int
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
	// PublicCounts returns the listing numbers of GET /v1/trust/summary.
	PublicCounts(ctx context.Context, now time.Time) (PublicCounts, error)
	// CountByStatus returns the number of listings per status in the
	// countries (nil: every country), for the administrators' overview. A
	// PUBLISHED listing whose publication period is over counts as EXPIRED.
	CountByStatus(ctx context.Context, marketCodes []string, now time.Time) (map[string]int, error)
}

// PublicCounts are the listing numbers of the trust summary.
type PublicCounts struct {
	// Public is every listing that is public right now.
	Public int
	// VerifiedAdvertiser counts the PUBLISHED, unexpired listings whose
	// advertiser is verified. It is counted on its own, without the public
	// rule, so it equals Public exactly when that rule holds.
	VerifiedAdvertiser int
	// Reviewed is the number of public listings that were reviewed.
	Reviewed int
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
