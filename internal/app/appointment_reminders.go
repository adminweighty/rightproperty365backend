package app

import (
	"context"
	"log/slog"
	"time"

	"rightproperty365.com/backend/internal/modules/appointments"
)

// RunAppointmentReminderSweeps (API 0.51.0) reminds confirmed appointments
// about a day ahead: at start, then every appointments.ReminderSweepInterval.
// Each start time is reminded once (the mark and the outbox event commit
// together, SKIP LOCKED), so several workers are safe.
func RunAppointmentReminderSweeps(ctx context.Context, store appointments.Store, logger *slog.Logger, now func() time.Time) {
	svc := appointments.NewService(appointments.Deps{Store: store, Now: now})
	ticker := time.NewTicker(appointments.ReminderSweepInterval)
	defer ticker.Stop()
	for {
		n, err := svc.SweepReminders(ctx)
		if err != nil {
			logger.ErrorContext(ctx, "appointment reminder sweep failed", slog.String("error", err.Error()))
		} else if n > 0 {
			logger.InfoContext(ctx, "appointment reminders", slog.Int("reminded", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
