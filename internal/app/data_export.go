package app

import (
	"context"
	"log/slog"
	"time"

	"rightproperty365.com/backend/internal/modules/dataexport"
)

// RunDataExportSweeps builds the requested personal data exports and
// deletes the expired ones (API 0.55.0): at start, then every
// dataexport.SweepInterval. SKIP LOCKED: several workers are safe.
func RunDataExportSweeps(ctx context.Context, svc *dataexport.Service, logger *slog.Logger) {
	ticker := time.NewTicker(dataexport.SweepInterval)
	defer ticker.Stop()
	for {
		n, err := svc.Sweep(ctx)
		if err != nil {
			logger.ErrorContext(ctx, "data export sweep failed", slog.String("error", err.Error()))
		}
		if n > 0 {
			logger.InfoContext(ctx, "data exports built", slog.Int("built", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
