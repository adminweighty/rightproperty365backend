// Package reference serves the platform's reference data: every value that
// fills a drop-down, a filter or a list (deal types, property categories and
// types, request types, contact methods, selectable account roles, directory
// kinds, service trades, cities, suburbs and listing features).
//
// The rows live in the database so a market administrator can change them
// without a code release (BR-001). The API reads them on every request and
// uses the same rows to validate input, so deactivating a row takes effect
// immediately, both in what clients are offered and in what the API accepts.
//
// Markets are not a set here: they have their own endpoint, GET /v1/markets.
package reference

import (
	"context"
	"errors"
	"slices"
	"sort"
)

// Set codes, as used in GET /v1/reference/{set}.
const (
	SetDealTypes          = "deal_types"
	SetPropertyCategories = "property_categories"
	SetPropertyTypes      = "property_types"
	SetRequestTypes       = "request_types"
	SetContactMethods     = "contact_methods"
	SetAccountRoles       = "account_roles"
	SetDirectoryKinds     = "directory_kinds"
	SetServiceTrades      = "service_trades"
	// SetRegions: the regions (provinces, states) of a market. A city's
	// parent_code is its region; a suburb's parent_code is its city.
	SetRegions         = "regions"
	SetCities          = "cities"
	SetSuburbs         = "suburbs"
	SetListingFeatures = "listing_features"
	// SetOnboardingDocuments: the kinds of document a professional may have
	// to attach to an onboarding application (code, label, description).
	SetOnboardingDocuments = "onboarding_documents"
	// SetOrganisationKinds: the kinds of organisation and how they are shown
	// ("Real Estate Agency", "Law Firm", ...).
	SetOrganisationKinds = "organisation_kinds"
)

// Sets returns every set code in the order the API lists them.
func Sets() []string {
	return []string{
		SetDealTypes, SetPropertyCategories, SetPropertyTypes, SetRequestTypes, SetContactMethods,
		SetAccountRoles, SetDirectoryKinds, SetServiceTrades, SetRegions, SetCities, SetSuburbs, SetListingFeatures,
		SetOnboardingDocuments, SetOrganisationKinds,
	}
}

// IsSet reports whether code names a reference set.
func IsSet(code string) bool { return slices.Contains(Sets(), code) }

// Errors.
var (
	// ErrUnknownSet: the set code does not exist.
	ErrUnknownSet = errors.New("unknown reference set")
	// ErrUnknownMarket: the market filter names no market.
	ErrUnknownMarket = errors.New("unknown market")
	// ErrNotFound: no such row.
	ErrNotFound = errors.New("reference item not found")
)

// Group is the heading a row is shown under (account roles only).
type Group struct {
	Code      string `json:"code"`
	Label     string `json:"label"`
	SortOrder int    `json:"sort_order"`
}

// Item is one row of a set. Every key is always present in JSON; the ones
// that do not apply to a set are null.
type Item struct {
	// Code is stable and is what clients send back to the API.
	Code string `json:"code"`
	// Label is the text shown to people. It can change at any time.
	Label string `json:"label"`
	// SingularLabel is set for directory kinds ("Estate agency").
	SingularLabel *string `json:"singular_label"`
	// Description is set for directory kinds.
	Description *string `json:"description"`
	SortOrder   int     `json:"sort_order"`
	// MarketCode is null for a row that applies to every market.
	MarketCode *string `json:"market_code"`
	// ParentCode links a property type to its category, a city to its region
	// and a suburb to its city.
	ParentCode *string `json:"parent_code"`
	// Group is set for account roles.
	Group *Group `json:"group"`
	// RequiresVerification is set for account roles: true means choosing the
	// role only records a request (PENDING_VERIFICATION).
	RequiresVerification *bool `json:"requires_verification"`
	// Active rows are the only ones the API returns or accepts.
	Active bool `json:"-"`
}

// Filter narrows a listing of rows.
type Filter struct {
	// Market keeps rows of that market plus rows that apply to every market.
	// Empty keeps everything.
	Market string
	// IncludeInactive also returns inactive rows (never used by the public API).
	IncludeInactive bool
}

// Store holds the reference rows.
type Store interface {
	// List returns the rows of a set ordered by sort_order, then label.
	List(ctx context.Context, set string, f Filter) ([]Item, error)
}

// Order sorts rows the way every store must return them.
func Order(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Group != nil && b.Group != nil && a.Group.SortOrder != b.Group.SortOrder {
			return a.Group.SortOrder < b.Group.SortOrder
		}
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		return a.Label < b.Label
	})
}

// Service is what the rest of the API uses.
type Service struct {
	store   Store
	markets func(ctx context.Context) ([]string, error)
}

// NewService builds a Service. markets returns the known market codes and is
// used to validate the market filter.
func NewService(store Store, markets func(ctx context.Context) ([]string, error)) *Service {
	return &Service{store: store, markets: markets}
}

func (s *Service) checkMarket(ctx context.Context, market string) error {
	if market == "" {
		return nil
	}
	codes, err := s.markets(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(codes, market) {
		return ErrUnknownMarket
	}
	return nil
}

// Set returns the active rows of one set.
func (s *Service) Set(ctx context.Context, set, market string) ([]Item, error) {
	if !IsSet(set) {
		return nil, ErrUnknownSet
	}
	if err := s.checkMarket(ctx, market); err != nil {
		return nil, err
	}
	items, err := s.store.List(ctx, set, Filter{Market: market})
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []Item{}
	}
	return items, nil
}

// All returns the active rows of every set, keyed by set code.
func (s *Service) All(ctx context.Context, market string) (map[string][]Item, error) {
	if err := s.checkMarket(ctx, market); err != nil {
		return nil, err
	}
	out := make(map[string][]Item, len(Sets()))
	for _, set := range Sets() {
		items, err := s.store.List(ctx, set, Filter{Market: market})
		if err != nil {
			return nil, err
		}
		if items == nil {
			items = []Item{}
		}
		out[set] = items
	}
	return out, nil
}

// ActiveCodes returns the codes of the active rows of a set, in order. It is
// what input validation uses.
func (s *Service) ActiveCodes(ctx context.Context, set string) ([]string, error) {
	items, err := s.store.List(ctx, set, Filter{})
	if err != nil {
		return nil, err
	}
	codes := make([]string, 0, len(items))
	for _, it := range items {
		codes = append(codes, it.Code)
	}
	return codes, nil
}

// Labels returns code -> label for the active rows of a set.
func (s *Service) Labels(ctx context.Context, set string) (map[string]string, error) {
	items, err := s.store.List(ctx, set, Filter{})
	if err != nil {
		return nil, err
	}
	labels := make(map[string]string, len(items))
	for _, it := range items {
		labels[it.Code] = it.Label
	}
	return labels, nil
}

// Items returns the active rows of a set without a market filter.
func (s *Service) Items(ctx context.Context, set string) ([]Item, error) {
	return s.store.List(ctx, set, Filter{})
}
