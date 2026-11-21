package app

import (
	"context"
	"log/slog"
	"time"

	"rightproperty365.com/backend/internal/modules/emaildelivery"
)

// EmailSpikeInterval is how often the worker looks for bounce and
// complaint spikes (API 0.59.0).
const EmailSpikeInterval = 5 * time.Minute

// RunEmailDeliverySweeps records email bounce and complaint spikes: at
// start, then every EmailSpikeInterval. Several workers agree through an
// advisory lock (one alert per kind per window).
func RunEmailDeliverySweeps(ctx context.Context, svc *emaildelivery.Service, logger *slog.Logger) {
	ticker := time.NewTicker(EmailSpikeInterval)
	defer ticker.Stop()
	for {
		if _, err := svc.Sweep(ctx); err != nil {
			logger.ErrorContext(ctx, "email delivery sweep failed", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
