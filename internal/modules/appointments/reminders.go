package appointments

import (
	"context"
	"errors"
	"time"
)

// Reminders (API 0.51.0, FR-021): one reminder per confirmed start time,
// about a day before. The sweep marks the appointment (reminded_for_start =
// start) and writes the APPOINTMENT_REMINDER outbox event in ONE
// transaction, so however often it runs, each start time is reminded once;
// a rescheduled appointment is reminded again for its new time. The
// notification handlers send the email and the in-app notice to the
// customer and the professional, consulting their preferences.

// Reminder timing.
const (
	ReminderAhead         = 24 * time.Hour
	ReminderSweepInterval = 5 * time.Minute
	reminderBatch         = 200
)

// ReminderStore is implemented by both stores.
type ReminderStore interface {
	// SendReminders marks up to limit CONFIRMED appointments that start in
	// (now, now+ahead] and were not reminded for that start, writing one
	// EventReminder each. It returns how many were marked.
	SendReminders(ctx context.Context, now time.Time, ahead time.Duration, limit int) (int, error)
}

// SweepReminders runs one reminder pass.
func (s *Service) SweepReminders(ctx context.Context) (int, error) {
	store, ok := s.d.Store.(ReminderStore)
	if !ok {
		return 0, errors.New("appointments: the store cannot send reminders")
	}
	return store.SendReminders(ctx, s.now(), ReminderAhead, reminderBatch)
}

// reminderEntry is the (unstored) entry a reminder event is built from.
func reminderEntry(a Appointment, now time.Time) TimelineEntry {
	start := a.StartUTC
	return TimelineEntry{AppointmentID: a.ID, Event: "REMINDER", ActorType: PartySystem, FromStatus: a.Status, ToStatus: a.Status, StartUTC: &start, At: now}
}
