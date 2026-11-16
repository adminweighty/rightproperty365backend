package app

import (
	"context"
	"log/slog"
	"time"

	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/subscriptions"
)

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
		expired, reminded, err := svc.Sweep(ctx)
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
		expired, err := svc.Sweep(ctx)
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
