package app

import (
	"context"
	"log/slog"
	"time"

	"rightproperty365.com/backend/internal/modules/uploads"
)

// RunUploadIntentSweeps deletes the upload intents (and their objects) not
// used within uploads.Lifetime (API 0.60.0): at start, then every
// uploads.SweepInterval. Compare-and-set transitions keep several workers safe.
func RunUploadIntentSweeps(ctx context.Context, svc *uploads.Service, logger *slog.Logger) {
	ticker := time.NewTicker(uploads.SweepInterval)
	defer ticker.Stop()
	for {
		start := time.Now()
		n, err := svc.Sweep(ctx)
		sweepDone("upload_intents", start, err)
		if err != nil {
			logger.ErrorContext(ctx, "upload intent sweep failed", slog.String("error", err.Error()))
		}
		if n > 0 {
			logger.InfoContext(ctx, "expired upload intents deleted", slog.Int("expired", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
