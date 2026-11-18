package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/venues"
)

// VenuesService builds the hourly rental service (the API, the worker's
// notifications and the in-memory development process share it).
func VenuesService(store venues.Store, listingsSvc *listings.Service, listingStore listings.Store, marketStore markets.Store,
	referenceStore reference.Store, logger *slog.Logger, now func() time.Time) *venues.Service {
	return venues.NewService(venues.Deps{
		Store: store, Listings: venueListings{svc: listingsSvc, store: listingStore}, Markets: venueMarkets{markets: marketStore},
		Hourly: HourlyTypes(reference.NewService(referenceStore, nil)), Logger: logger, Now: now,
	})
}

// HourlyTypes reads the property types flagged hourly_bookable (active
// rows of the reference data, read on every call so an administrator's
// change applies at once).
func HourlyTypes(ref *reference.Service) venues.HourlyTypes {
	return func(ctx context.Context) (map[string]bool, error) {
		items, err := ref.Set(ctx, reference.SetPropertyTypes, "")
		if err != nil {
			return nil, err
		}
		out := map[string]bool{}
		for _, it := range items {
			if it.HourlyBookable != nil && *it.HourlyBookable {
				out[it.Code] = true
			}
		}
		return out, nil
	}
}

// venueListings reads listings for the venues module.
type venueListings struct {
	svc   *listings.Service
	store listings.Store
}

// Listing implements venues.Listings.
func (l venueListings) Listing(ctx context.Context, id uuid.UUID) (venues.ListingInfo, error) {
	records, err := l.store.ByIDs(ctx, []uuid.UUID{id})
	if err != nil {
		return venues.ListingInfo{}, err
	}
	if len(records) == 0 {
		return venues.ListingInfo{}, venues.ErrListingNotFound
	}
	r := records[0]
	info := venues.ListingInfo{ID: r.ID, Slug: r.Slug, Reference: r.Reference, Title: r.Title, PropertyType: r.PropertyType, MarketCode: r.MarketCode,
		OrganisationID: r.OrganisationID, AgentUserID: r.AgentUserID, SellerUserID: r.SellerUserID}
	if l.svc != nil {
		states, err := l.svc.CardStates(ctx, []uuid.UUID{id})
		if err != nil {
			return venues.ListingInfo{}, err
		}
		info.Public = states[id].Public
	}
	return info, nil
}

// AgentListingIDs implements venues.Listings.
func (l venueListings) AgentListingIDs(ctx context.Context, organisationID, agentUserID uuid.UUID) ([]uuid.UUID, error) {
	return applicationListings{svc: l.svc, store: l.store}.AgentListingIDs(ctx, organisationID, agentUserID)
}

// venueMarkets answers a country's zone and currency.
type venueMarkets struct{ markets markets.Store }

// Market implements venues.Markets.
func (m venueMarkets) Market(ctx context.Context, code string) (string, string, error) {
	all, err := m.markets.List(ctx)
	if err != nil {
		return "", "", err
	}
	for _, c := range all {
		if c.Code == code {
			return c.Timezone, c.CurrencyCode, nil
		}
	}
	return "", "", nil
}

// venueRates gives the listings their `hourly_booking`.
type venueRates struct{ svc *venues.Service }

// HourlySummaries implements listings.HourlyRates.
func (v venueRates) HourlySummaries(ctx context.Context, records []listings.Record) (map[uuid.UUID]listings.HourlyBookingSummary, error) {
	types := make(map[uuid.UUID]string, len(records))
	for _, r := range records {
		types[r.ID] = r.PropertyType
	}
	got, err := v.svc.Summaries(ctx, types)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]listings.HourlyBookingSummary, len(got))
	for id, s := range got {
		out[id] = listings.HourlyBookingSummary{Enabled: s.Enabled, RatePerHour: s.RatePerHour, Currency: s.Currency, IncrementMinutes: s.IncrementMinutes}
	}
	return out, nil
}

// VenueNotices loads the data behind a venue booking email (the
// notification handlers' venue source).
func VenueNotices(store venues.Store, listingStore listings.Store, marketStore markets.Store, referenceStore reference.Store,
	logger *slog.Logger) func(ctx context.Context, id uuid.UUID) (venues.BookingNotice, error) {
	return VenuesService(store, nil, listingStore, marketStore, referenceStore, logger, nil).Notice
}
