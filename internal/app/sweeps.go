package app

import (
	"context"
	"log/slog"
	"time"

	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/modules/tenancies"
)

// ListingExpirySweepInterval is how often listings are expired and their
// reminders sent (API 0.45.0). Hourly rather than daily, so an expiry and
// the 1-day reminder are never up to a day late; every run is idempotent.
const ListingExpirySweepInterval = time.Hour

// RunListingExpirySweeps runs the listing expiry sweep (listings.SweepExpiry)
// at start and then every ListingExpirySweepInterval until ctx ends: PUBLISHED
// listings past their period are stored EXPIRED (LISTING_EXPIRED) and those
// ending within 14, 7 or 1 days get their reminder (LISTING_EXPIRING), once
// per publication cycle. now is the clock (nil: time.Now).
func RunListingExpirySweeps(ctx context.Context, store listings.ExpiryStore, logger *slog.Logger, now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	ticker := time.NewTicker(ListingExpirySweepInterval)
	defer ticker.Stop()
	for {
		r, err := listings.SweepExpiry(ctx, store, now())
		if err != nil {
			logger.ErrorContext(ctx, "listing expiry sweep failed", slog.String("error", err.Error()))
		} else if r.Claimed > 0 {
			logger.InfoContext(ctx, "listing expiry sweep", slog.Int("expired", r.Expired), slog.Int("reminded", r.Reminded),
				slog.Int("reminders_skipped", r.Skipped))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// AgreementSweepInterval is how often expiries and reminders are checked.
const AgreementSweepInterval = 5 * time.Minute

// RunAgreementSweeps expires agreements and sends the reminders that are
// due, every AgreementSweepInterval until ctx ends. With leaseSvc set, a
// lease follows its agreement when it expires (the API's own service is
// already connected).
func RunAgreementSweeps(ctx context.Context, svc *agreements.Service, leaseSvc *leases.Service, logger *slog.Logger) {
	if leaseSvc != nil {
		svc.SetOnFinished(FollowLeases(leaseSvc, logger))
	}
	ticker := time.NewTicker(AgreementSweepInterval)
	defer ticker.Stop()
	for {
		start := time.Now()
		expired, reminded, err := svc.Sweep(ctx)
		sweepDone("agreements", start, err)
		if err != nil {
			logger.ErrorContext(ctx, "agreement sweep failed", slog.String("error", err.Error()))
		} else if expired > 0 || reminded > 0 {
			logger.InfoContext(ctx, "agreement sweep", slog.Int("expired", expired), slog.Int("reminded", reminded))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SubscriptionSweepInterval is how often ended subscriptions are expired.
const SubscriptionSweepInterval = 5 * time.Minute

// RunSubscriptionSweeps records the expiry of every ACTIVE subscription whose
// end has passed (with its email), every SubscriptionSweepInterval until ctx
// ends. Reads already treat a past end as EXPIRED; the sweep makes it durable
// and sends the notice.
func RunSubscriptionSweeps(ctx context.Context, svc *subscriptions.Service, logger *slog.Logger) {
	ticker := time.NewTicker(SubscriptionSweepInterval)
	defer ticker.Stop()
	for {
		start := time.Now()
		expired, err := svc.Sweep(ctx)
		sweepDone("subscriptions", start, err)
		if err != nil {
			logger.ErrorContext(ctx, "subscription sweep failed", slog.String("error", err.Error()))
		} else if expired > 0 {
			logger.InfoContext(ctx, "subscription sweep", slog.Int("expired", expired))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RentScheduleSweepInterval is how often the rent schedules are re-run.
const RentScheduleSweepInterval = 24 * time.Hour

// RunRentScheduleSweeps re-runs the rent rule of every ACTIVE tenancy at
// start and then every RentScheduleSweepInterval until ctx ends: a rolling
// schedule gains the next month's due when the month turns (the horizon is
// the end of the month 12 months ahead), and a tenancy activated before
// API 0.36.1 gets its schedule. Idempotent.
func RunRentScheduleSweeps(ctx context.Context, svc *tenancies.Service, logger *slog.Logger) {
	ticker := time.NewTicker(RentScheduleSweepInterval)
	defer ticker.Stop()
	for {
		added, err := svc.ExtendSchedules(ctx)
		if err != nil {
			logger.ErrorContext(ctx, "rent schedule sweep failed", slog.String("error", err.Error()))
		} else if added > 0 {
			logger.InfoContext(ctx, "rent schedule sweep", slog.Int("dues_added", added))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
