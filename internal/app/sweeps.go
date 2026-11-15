package app

import (
	"context"
	"log/slog"
	"time"

	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/leases"
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
