package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/analytics"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
)

// analyticsListings adapts the listing store to analytics.Listings. Whether
// a listing is public is the listings module's own rule (Record.Public).
type analyticsListings struct {
	store listings.Store
	now   func() time.Time
}

func infoOf(r listings.Record, now time.Time) analytics.ListingInfo {
	return analytics.ListingInfo{ID: r.ID, Title: r.Title, Reference: r.Reference, Slug: r.Slug, MarketCode: r.MarketCode,
		OrganisationID: r.OrganisationID, AgentUserID: r.AgentUserID, SellerUserID: r.SellerUserID, Public: r.Public(now)}
}

// Listings implements analytics.Listings.
func (a analyticsListings) Listings(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]analytics.ListingInfo, error) {
	out := map[uuid.UUID]analytics.ListingInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	records, err := a.store.ByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	now := a.now().UTC()
	for _, r := range records {
		out[r.ID] = infoOf(r, now)
	}
	return out, nil
}

// OrganisationListings implements analytics.Listings.
func (a analyticsListings) OrganisationListings(ctx context.Context, organisationID uuid.UUID) ([]analytics.ListingInfo, error) {
	records, err := a.store.Owned(ctx, []uuid.UUID{organisationID}, nil)
	if err != nil {
		return nil, err
	}
	now := a.now().UTC()
	out := make([]analytics.ListingInfo, 0, len(records))
	for _, r := range records {
		if r.OrganisationID != nil && *r.OrganisationID == organisationID {
			out = append(out, infoOf(r, now))
		}
	}
	return out, nil
}

// AnalyticsService builds the analytics service on the listing store and
// the markets' time zones (the same in the API, the worker and the
// development in-process recorder). now nil means time.Now.
func AnalyticsService(store analytics.Store, listingStore listings.Store, marketStore markets.Store, retentionDays int,
	logger *slog.Logger, now func() time.Time) *analytics.Service {
	if now == nil {
		now = time.Now
	}
	return analytics.NewService(analytics.Deps{Store: store, Listings: analyticsListings{store: listingStore, now: now},
		Zones: countryZones{markets: marketStore}, Logger: logger, Now: now, RawRetentionDays: retentionDays})
}

// AppointmentListing returns the listing an appointment is about, for the
// analytics recorder (nil for a consultation or an unknown appointment).
func AppointmentListing(store appointments.Store) analytics.AppointmentListing {
	return func(ctx context.Context, id uuid.UUID) (*uuid.UUID, error) {
		a, err := store.Get(ctx, id)
		if errors.Is(err, appointments.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return a.ListingID, nil
	}
}

// AnalyticsJobInterval is how often the worker recomputes the rollups (the
// last 3 market-local days) and deletes expired raw events.
const AnalyticsJobInterval = 15 * time.Minute

// RunAnalyticsJobs runs the rollup and the raw-event retention every
// AnalyticsJobInterval until ctx ends.
func RunAnalyticsJobs(ctx context.Context, svc *analytics.Service, logger *slog.Logger) {
	ticker := time.NewTicker(AnalyticsJobInterval)
	defer ticker.Stop()
	for {
		if res, err := svc.Rollup(ctx); err != nil {
			logger.ErrorContext(ctx, "analytics rollup failed", slog.String("error", err.Error()))
		} else {
			logger.InfoContext(ctx, "analytics rollup", slog.Int("markets", res.Markets), slog.Int("rows", res.Rows))
		}
		if events, late, err := svc.PurgeRaw(ctx); err != nil {
			logger.ErrorContext(ctx, "analytics retention failed", slog.String("error", err.Error()))
		} else if events > 0 || late > 0 {
			logger.InfoContext(ctx, "analytics retention", slog.Int64("events_deleted", events), slog.Int64("late_deleted", late))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
