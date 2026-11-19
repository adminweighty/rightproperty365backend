// Package directory serves the public directories of agencies, agents,
// developers, property managers, hosts and service providers.
//
// A profile is public information only. There is no phone number, email
// address or street address in any type of this package, in the table behind
// it, or in any response: the only way to reach a profile is an enquiry
// (POST /v1/business-enquiries).
package directory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Reference sets used by profiles.
const (
	SetCities        = "cities"
	SetServiceTrades = "service_trades"
)

// Kinds that own listings.
const (
	KindAgencies = "agencies"
	KindAgents   = "agents"
)

// SpecialismLabels are the agent specialisms and how they are shown.
var SpecialismLabels = map[string]string{"SALES": "Sales Agent", "LETTINGS": "Lettings Agent"}

// StatusActive is the only profile status that is public.
const StatusActive = "ACTIVE"

// Error codes.
const (
	CodeKindNotFound    = "DIRECTORY_KIND_NOT_FOUND"
	CodeProfileNotFound = "PROFILE_NOT_FOUND"
)

// Errors.
var (
	ErrKindNotFound    = errors.New("directory kind not found")
	ErrProfileNotFound = errors.New("profile not found")
)

// Kind is a directory section.
type Kind struct {
	Code          string `json:"code"`
	Label         string `json:"label"`
	SingularLabel string `json:"singular_label"`
	Description   string `json:"description"`
}

// KindCount is a directory section with the number of public profiles in it.
type KindCount struct {
	Kind
	Count int `json:"count"`
}

// Offering is one service described in detail.
type Offering struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Profile is the public representation of a profile, as it appears in lists.
type Profile struct {
	Kind        string `json:"kind"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Tagline     string `json:"tagline"`
	Description string `json:"description"`
	// City is a reference city (code set) or, in a country without reference
	// places, a free-text one (code null, label as entered).
	City listings.Place `json:"city"`
	// Region is the region (province) of the city, or the free text that came
	// with a free-text city; null when there is none. Country is the
	// profile's own country (every profile has one).
	Region    *listings.Place   `json:"region"`
	Country   *listings.Country `json:"country"`
	Areas     []string          `json:"areas"`
	Services  []string          `json:"services"`
	Offerings []Offering        `json:"offerings"`
	Trade     *listings.Coded   `json:"trade"`
	// Specialism is set for an agent who specialises in sales or lettings.
	Specialism *listings.Coded `json:"specialism"`
	Verified   bool            `json:"verified"`
	Sponsored  bool            `json:"sponsored"`
	Since      *int            `json:"since"`
	// Logo is the profile's logo (an agent's photo); null when it has none.
	// Its url is a path relative to the API origin.
	Logo         *listings.Logo       `json:"logo"`
	Agency       *listings.ProfileRef `json:"agency"`
	ListingCount int                  `json:"listing_count"`
	// Rating is the profile's published reviews (API 0.36.4): their mean
	// with one decimal (null when none) and their count. Only service
	// providers are reviewed; every other profile has {null, 0}.
	Rating    Rating    `json:"rating"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Rating is a profile's published reviews: the average ("4.5", one
// decimal, null when there is none) and the count.
type Rating struct {
	Average *string `json:"average"`
	Count   int     `json:"count"`
}

// RatingSource returns the rating of each profile (by ID) that has
// published reviews.
type RatingSource func(ctx context.Context, profileIDs []uuid.UUID) (map[uuid.UUID]Rating, error)

// Detail is one profile with its agents and its public listings.
type Detail struct {
	Profile
	// Agents are an agency's agent profiles; empty for every other kind.
	Agents []listings.ProfileRef `json:"agents"`
	// Listings are the profile's public listings, newest first.
	Listings []listings.Listing `json:"listings"`
}

// Record is a profile as stored.
type Record struct {
	ID          uuid.UUID
	Kind        string
	Slug        string
	Name        string
	Role        string
	Tagline     string
	Description string
	CityCode    string
	// CityName and RegionName are free text, for a profile in a country that
	// has no reference places: set instead of CityCode (never with it).
	CityName   string
	RegionName string
	// MarketCode is the profile's country (ISO 3166-1 alpha-2).
	MarketCode string
	Areas      []string
	Services   []string
	Offerings  []Offering
	TradeCode  string
	// Specialism is "SALES", "LETTINGS" or "" (agent profiles only).
	Specialism string
	Verified   bool
	// VerifiedAt is when the profile was verified (nil when it is not, or
	// when that time was never recorded).
	VerifiedAt *time.Time
	Sponsored  bool
	Since      *int
	Status     string
	UpdatedAt  time.Time
	// Logo is the profile's logo; nil when it has none.
	Logo *Logo

	OrganisationID uuid.UUID
	// AgentUserID is set for agent profiles.
	AgentUserID *uuid.UUID
}

// Filter narrows a directory listing.
type Filter struct {
	Q     string
	City  string
	Trade string
	// Country (any enabled ISO country code) keeps the profiles of that
	// country, whether their city is a reference city or free text. Region
	// (a regions code) narrows by the profile's city: the Service resolves it
	// into Cities.
	Country string
	Region  string
	// Cities, when not nil, is the set of city codes a profile's city must
	// be in (possibly empty: nothing matches). Stores filter on it; clients
	// cannot set it.
	Cities []string
}

// Entry is one sitemap entry.
type Entry struct {
	Kind      string    `json:"kind"`
	Slug      string    `json:"slug"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store holds the profiles. Every method returns only profiles that are
// published (ACTIVE) AND verified: nothing else is ever public.
type Store interface {
	// List returns one page of a kind, sponsored profiles first, then by name,
	// and the total number of matches.
	List(ctx context.Context, kind string, f Filter, limit, offset int) ([]Record, int, error)
	// Get returns one profile or ErrProfileNotFound.
	Get(ctx context.Context, kind, slug string) (Record, error)
	// ByID returns one public profile by its ID, or ErrProfileNotFound
	// (maintenance assigns a request to a provider by ID, API 0.36.2).
	ByID(ctx context.Context, id uuid.UUID) (Record, error)
	// AgencyOf returns the agency profile of an organisation, if it has one.
	AgencyOf(ctx context.Context, organisationID uuid.UUID) (*Record, error)
	// AgentsOf returns the agent profiles of an organisation, by name.
	AgentsOf(ctx context.Context, organisationID uuid.UUID) ([]Record, error)
	// Counts returns the number of profiles per kind.
	Counts(ctx context.Context) (map[string]int, error)
	// Entries returns every profile's address and updated_at (sitemap).
	Entries(ctx context.Context) ([]Entry, error)
	// VerifiedOrganisations returns the number of organisations with a public
	// profile of their own (never an agent's).
	VerifiedOrganisations(ctx context.Context) (int, error)

	// The three methods below are for a profile's OWN managers and return
	// profiles WHATEVER THEIR STATUS. They never answer a public request.

	// Find returns one profile or ErrProfileNotFound.
	Find(ctx context.Context, kind, slug string) (Record, error)
	// Managed returns the business profiles of the organisations and the
	// agent profile of the user.
	Managed(ctx context.Context, organisationIDs []uuid.UUID, agentUserID uuid.UUID) ([]Record, error)
	// SetLogo replaces the logo of a profile (nil removes it), sets its
	// updated_at, and returns the object key of the logo it replaced ("" when
	// there was none). ErrProfileNotFound when the profile does not exist.
	SetLogo(ctx context.Context, id uuid.UUID, logo *Logo, now time.Time) (previousKey string, err error)
}

// Reference supplies the directory kinds and reference labels.
type Reference interface {
	DirectoryKindList(ctx context.Context) ([]Kind, error)
	Labels(ctx context.Context, set string) (map[string]string, error)
}

// ListingSource supplies the public listings of a profile.
type ListingSource interface {
	ByOwner(ctx context.Context, organisationID, agentUserID *uuid.UUID) ([]listings.Listing, error)
	Slugs(ctx context.Context) ([]listings.SlugEntry, error)
}

// Service implements the public directory use cases.
type Service struct {
	store    Store
	ref      Reference
	listings ListingSource
	// places supplies the location hierarchy; nil means none.
	places listings.Places
	// files is the object storage logos are kept in; nil means none.
	files  storage.Storage
	logger *slog.Logger
	now    func() time.Time
	// ratings supplies the reviews' ratings; nil means none.
	ratings RatingSource
}

// SetRatings gives the Service the ratings of the reviews (API 0.36.4).
func (s *Service) SetRatings(src RatingSource) { s.ratings = src }

// rate sets the rating of each profile (records[i] is profiles[i]).
func (s *Service) rate(ctx context.Context, records []Record, profiles []*Profile) error {
	if s.ratings == nil || len(records) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	got, err := s.ratings(ctx, ids)
	if err != nil {
		return fmt.Errorf("load ratings: %w", err)
	}
	for i, r := range records {
		if rt, ok := got[r.ID]; ok {
			profiles[i].Rating = rt
		}
	}
	return nil
}

// SetPlaces gives the Service the location hierarchy behind the country and
// region filters and the region / country of a profile.
func (s *Service) SetPlaces(p listings.Places) { s.places = p }

// placeNames is the hierarchy with the labels needed to show it.
type placeNames struct {
	geo     listings.Geography
	regions map[string]string
}

func (s *Service) placeNames(ctx context.Context) (placeNames, error) {
	if s.places == nil {
		return placeNames{}, nil
	}
	geo, err := s.places.Geography(ctx)
	if err != nil {
		return placeNames{}, fmt.Errorf("load the location hierarchy: %w", err)
	}
	regions, err := s.ref.Labels(ctx, listings.SetRegions)
	if err != nil {
		return placeNames{}, err
	}
	return placeNames{geo: geo, regions: regions}, nil
}

func (n placeNames) countryOf(city string) string {
	if c := n.geo.CityCountry[city]; c != "" {
		return c
	}
	return n.geo.RegionCountry[n.geo.CityRegion[city]]
}

// locate sets a profile's region (from its city, or the free text that came
// with a free-text city) and its country (its own; a row from before
// profiles carried one falls back to the country of its city).
func (n placeNames) locate(p *Profile, r Record) {
	city := r.CityCode
	if region := n.geo.CityRegion[city]; city != "" && region != "" {
		p.Region = &listings.Place{Code: &region, Label: labelOr(n.regions, region)}
	} else if city == "" && r.RegionName != "" {
		p.Region = &listings.Place{Label: r.RegionName}
	}
	code := r.MarketCode
	if code == "" {
		code = n.countryOf(city)
	}
	if code != "" {
		p.Country = &listings.Country{Code: code, Name: code}
		if info, ok := n.geo.Countries[code]; ok {
			p.Country.Name = info.Name
		}
	}
}

// NewService builds a Service.
func NewService(store Store, ref Reference, source ListingSource) *Service {
	return &Service{store: store, ref: ref, listings: source}
}

func (s *Service) kind(ctx context.Context, code string) (Kind, error) {
	kinds, err := s.ref.DirectoryKindList(ctx)
	if err != nil {
		return Kind{}, err
	}
	for _, k := range kinds {
		if k.Code == code {
			return k, nil
		}
	}
	return Kind{}, ErrKindNotFound
}

// Kinds returns the directory sections with their profile counts.
func (s *Service) Kinds(ctx context.Context) ([]KindCount, error) {
	kinds, err := s.ref.DirectoryKindList(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := s.store.Counts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]KindCount, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, KindCount{Kind: k, Count: counts[k.Code]})
	}
	return out, nil
}

// RefOf is the public reference to a profile. Verified means the profile is
// PUBLIC (published and verified); only a public profile has a logo here,
// because only a public profile's logo is served.
func RefOf(r Record) listings.ProfileRef {
	out := listings.ProfileRef{Kind: r.Kind, Slug: r.Slug, Name: r.Name, Verified: r.Verified && r.Status == StatusActive, Specialism: r.Specialism}
	if out.Verified {
		out.Logo = publicLogo(r)
	}
	return out
}

// owner returns the listing owner a profile stands for: an agency's
// organisation, or one agent. Other kinds own no listings.
func owner(r Record) (organisationID, agentUserID *uuid.UUID) {
	switch r.Kind {
	case KindAgencies:
		return &r.OrganisationID, nil
	case KindAgents:
		return nil, r.AgentUserID
	}
	return nil, nil
}

func (s *Service) present(ctx context.Context, r Record, cities, trades map[string]string) (Profile, []listings.Listing, error) {
	p := Profile{
		Kind: r.Kind, Slug: r.Slug, Name: r.Name, Role: r.Role, Tagline: r.Tagline, Description: r.Description,
		City:  listings.PlaceOf(cities, r.CityCode, r.CityName),
		Areas: append([]string{}, r.Areas...), Services: append([]string{}, r.Services...), Offerings: append([]Offering{}, r.Offerings...),
		Verified: r.Verified, Sponsored: r.Sponsored, Since: r.Since, UpdatedAt: r.UpdatedAt.UTC(), Logo: publicLogo(r),
	}
	if r.TradeCode != "" {
		p.Trade = &listings.Coded{Code: r.TradeCode, Label: labelOr(trades, r.TradeCode)}
	}
	if label, ok := SpecialismLabels[r.Specialism]; ok {
		p.Specialism = &listings.Coded{Code: r.Specialism, Label: label}
	}
	if r.Kind == KindAgents {
		agency, err := s.store.AgencyOf(ctx, r.OrganisationID)
		if err != nil {
			return Profile{}, nil, err
		}
		if agency != nil {
			a := RefOf(*agency)
			p.Agency = &a
		}
	}
	var owned []listings.Listing
	if org, agent := owner(r); org != nil || agent != nil {
		var err error
		if owned, err = s.listings.ByOwner(ctx, org, agent); err != nil {
			return Profile{}, nil, err
		}
	}
	p.ListingCount = len(owned)
	return p, owned, nil
}

func labelOr(labels map[string]string, code string) string {
	if label, ok := labels[code]; ok {
		return label
	}
	return code
}

// Page is one page of a directory.
type Page struct {
	Kind       Kind      `json:"kind"`
	Items      []Profile `json:"items"`
	Page       int       `json:"page"`
	PageSize   int       `json:"page_size"`
	Total      int       `json:"total"`
	TotalPages int       `json:"total_pages"`
}

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// List returns one page of a directory.
func (s *Service) List(ctx context.Context, kindCode string, f Filter, page, size int) (Page, error) {
	kind, err := s.kind(ctx, kindCode)
	if err != nil {
		return Page{}, err
	}
	cities, err := s.ref.Labels(ctx, SetCities)
	if err != nil {
		return Page{}, err
	}
	trades, err := s.ref.Labels(ctx, SetServiceTrades)
	if err != nil {
		return Page{}, err
	}
	fields := map[string]string{}
	if _, ok := cities[f.City]; f.City != "" && !ok {
		fields["city"] = "is not a known value; see GET /v1/reference/cities"
	}
	if _, ok := trades[f.Trade]; f.Trade != "" && !ok {
		fields["trade"] = "is not a known value; see GET /v1/reference/service_trades"
	}
	places, err := s.placeNames(ctx)
	if err != nil {
		return Page{}, err
	}
	// Country and region narrow by the profile's city. The chain is checked
	// like the listings search: a mismatch is reported on the narrower one.
	if f.Country != "" || f.Region != "" {
		if _, ok := places.geo.Countries[f.Country]; f.Country != "" && !ok {
			fields["country"] = "is not a known country; see GET /v1/countries"
		}
		if f.Region != "" {
			switch _, ok := places.regions[f.Region]; {
			case !ok:
				fields["region"] = "is not a known value; see GET /v1/reference/regions"
			case f.Country != "" && places.geo.RegionCountry[f.Region] != f.Country:
				fields["region"] = "is not a region of that country"
			}
		}
		if _, bad := fields["city"]; f.City != "" && !bad {
			switch {
			case f.Region != "" && places.geo.CityRegion[f.City] != f.Region:
				fields["city"] = "is not a city of that region"
			case f.Country != "" && places.countryOf(f.City) != f.Country:
				fields["city"] = "is not a city of that country"
			}
		}
		// A region narrows by city; a country is the profile's own country
		// and is filtered by the store, so it also finds profiles whose city
		// is free text.
		if f.Region != "" {
			f.Cities = []string{}
			for city := range cities {
				if places.geo.CityRegion[city] == f.Region {
					f.Cities = append(f.Cities, city)
				}
			}
			sort.Strings(f.Cities)
		}
	}
	if len(fields) > 0 {
		return Page{}, &ValidationError{Fields: fields}
	}

	records, total, err := s.store.List(ctx, kindCode, f, size, (page-1)*size)
	if err != nil {
		return Page{}, fmt.Errorf("list profiles: %w", err)
	}
	out := Page{Kind: kind, Items: make([]Profile, 0, len(records)), Page: page, PageSize: size, Total: total, TotalPages: (total + size - 1) / size}
	for _, r := range records {
		p, _, err := s.present(ctx, r, cities, trades)
		if err != nil {
			return Page{}, err
		}
		places.locate(&p, r)
		out.Items = append(out.Items, p)
	}
	ptrs := make([]*Profile, len(out.Items))
	for i := range out.Items {
		ptrs[i] = &out.Items[i]
	}
	if err := s.rate(ctx, records, ptrs); err != nil {
		return Page{}, err
	}
	return out, nil
}

// Get returns one profile with its agents and listings.
func (s *Service) Get(ctx context.Context, kindCode, slug string) (Detail, error) {
	if _, err := s.kind(ctx, kindCode); err != nil {
		return Detail{}, err
	}
	r, err := s.store.Get(ctx, kindCode, slug)
	if err != nil {
		return Detail{}, err
	}
	cities, err := s.ref.Labels(ctx, SetCities)
	if err != nil {
		return Detail{}, err
	}
	trades, err := s.ref.Labels(ctx, SetServiceTrades)
	if err != nil {
		return Detail{}, err
	}
	places, err := s.placeNames(ctx)
	if err != nil {
		return Detail{}, err
	}
	p, owned, err := s.present(ctx, r, cities, trades)
	places.locate(&p, r)
	if err != nil {
		return Detail{}, err
	}
	if err := s.rate(ctx, []Record{r}, []*Profile{&p}); err != nil {
		return Detail{}, err
	}
	d := Detail{Profile: p, Agents: []listings.ProfileRef{}, Listings: owned}
	if d.Listings == nil {
		d.Listings = []listings.Listing{}
	}
	if r.Kind == KindAgencies {
		agents, err := s.store.AgentsOf(ctx, r.OrganisationID)
		if err != nil {
			return Detail{}, err
		}
		for _, a := range agents {
			d.Agents = append(d.Agents, RefOf(a))
		}
	}
	return d, nil
}

// Sitemap is everything a sitemap needs.
type Sitemap struct {
	Listings []listings.SlugEntry `json:"listings"`
	Profiles []Entry              `json:"profiles"`
}

// Sitemap returns every public listing slug and profile address with updated_at.
func (s *Service) Sitemap(ctx context.Context) (Sitemap, error) {
	slugs, err := s.listings.Slugs(ctx)
	if err != nil {
		return Sitemap{}, err
	}
	entries, err := s.store.Entries(ctx)
	if err != nil {
		return Sitemap{}, err
	}
	kinds, err := s.ref.DirectoryKindList(ctx)
	if err != nil {
		return Sitemap{}, err
	}
	out := Sitemap{Listings: slugs, Profiles: []Entry{}}
	for _, e := range entries {
		if slices.ContainsFunc(kinds, func(k Kind) bool { return k.Code == e.Kind }) {
			out.Profiles = append(out.Profiles, e)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Handler serves GET /v1/directory, /v1/directory/{kind},
// /v1/directory/{kind}/{slug}, its logo, /v1/sitemap and the profile
// managers' routes under /v1/me/profiles (logo_handler.go).
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrKindNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeKindNotFound, "This directory does not exist.", nil)
	case errors.Is(err, ErrProfileNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeProfileNotFound, "This profile does not exist.", nil)
	case errors.Is(err, ErrLogoNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeLogoNotFound, "This profile has no logo.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You do not have permission to do this.", nil)
	case errors.Is(err, ErrLogoTooLarge), errors.As(err, &tooLarge):
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, CodeLogoTooLarge, "A logo must be at most 2 MB.", nil)
	case errors.Is(err, ErrLogoUnsupported):
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, CodeLogoUnsupported, "A logo must be a PNG or JPEG image.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

type kindsResponse struct {
	Kinds []KindCount `json:"kinds"`
}

// Kinds handles GET /v1/directory.
func (h *Handler) Kinds(w http.ResponseWriter, r *http.Request) {
	kinds, err := h.svc.Kinds(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, kindsResponse{Kinds: kinds}, listings.CacheMaxAge, listings.CacheStale)
}

// List handles GET /v1/directory/{kind}.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	fields := map[string]string{}
	page, size := listings.ParsePaging(v, fields)
	f := Filter{Q: strings.TrimSpace(v.Get("q")), City: v.Get("city"), Trade: v.Get("trade")}
	// country: any enabled ISO country code, in any case (`market` is an alias); region: a regions code.
	f.Country, f.Region = strings.ToUpper(strings.TrimSpace(v.Get("country"))), strings.TrimSpace(v.Get("region"))
	if market := strings.ToUpper(strings.TrimSpace(v.Get("market"))); market != "" {
		if f.Country != "" && f.Country != market {
			fields["market"] = "is an alias of country; send one of them, or the same value in both"
		}
		f.Country = market
	}
	if len([]rune(f.Q)) > 100 {
		fields["q"] = "must be at most 100 characters"
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	out, err := h.svc.List(r.Context(), chi.URLParam(r, "kind"), f, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, out, listings.CacheMaxAge, listings.CacheStale)
}

type profileResponse struct {
	Profile Detail `json:"profile"`
}

// Get handles GET /v1/directory/{kind}/{slug}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if !slugRe.MatchString(slug) || len(slug) > 80 {
		h.fail(w, r, ErrProfileNotFound)
		return
	}
	detail, err := h.svc.Get(r.Context(), chi.URLParam(r, "kind"), slug)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, profileResponse{Profile: detail}, listings.CacheMaxAge, listings.CacheStale)
}

// Sitemap handles GET /v1/sitemap.
func (h *Handler) Sitemap(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.Sitemap(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, out, listings.CacheMaxAge, listings.CacheStale)
}
