package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/subscriptions"
)

// placementQuota ties a placement to a subscription (API 0.53.0): it must be
// ACTIVE and the listing's advertiser's (its organisation, or the direct
// seller), and its plan's MAX_FEATURED_LISTINGS is the limit.
func placementQuota(svc *subscriptions.Service) listings.PlacementQuota {
	return func(ctx context.Context, id uuid.UUID, r listings.Record) (*int, string, error) {
		d, err := svc.Get(ctx, id)
		if errors.Is(err, subscriptions.ErrNotFound) {
			return nil, "is not a subscription", nil
		}
		if err != nil {
			return nil, "", err
		}
		sub := d.Subscription
		owner := (sub.SubscriberKind == subscriptions.KindOrganisation && r.OrganisationID != nil && *r.OrganisationID == sub.SubscriberID) ||
			(sub.SubscriberKind == subscriptions.KindIndividual && r.OrganisationID == nil && r.SellerUserID != nil && *r.SellerUserID == sub.SubscriberID)
		switch {
		case !owner:
			return nil, "is not the subscription of this listing's advertiser", nil
		case sub.Status != subscriptions.StatusActive:
			return nil, "is not active", nil
		}
		plan, err := svc.Plan(ctx, sub.PlanID)
		if err != nil {
			return nil, "", err
		}
		limit, listed := plan.Limit(subscriptions.FeatureMaxFeaturedListings)
		if !listed {
			return nil, "has a plan without featured listings", nil
		}
		return limit, "", nil
	}
}

// RunPlacementSync keeps the listings' placements in step with their
// periods (API 0.53.0): at start, then every listings.PlacementSyncInterval.
func RunPlacementSync(ctx context.Context, svc *listings.Placements, logger *slog.Logger) {
	ticker := time.NewTicker(listings.PlacementSyncInterval)
	defer ticker.Stop()
	for {
		n, err := svc.Sync(ctx)
		if err != nil {
			logger.ErrorContext(ctx, "placement sync failed", slog.String("error", err.Error()))
		} else if n > 0 {
			logger.InfoContext(ctx, "placements applied", slog.Int("listings", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
