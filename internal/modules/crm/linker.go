package crm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/outbox"
)

// kindOfEvent maps the outbox events of OutboxEventTypes to the item they announce.
var kindOfEvent = map[string]string{
	"PROPERTY_REQUEST_CREATED": KindPropertyRequest,
	"BUSINESS_ENQUIRY_CREATED": KindBusinessEnquiry,
	"APPOINTMENT_REQUESTED":    KindAppointment,
}

// HandleOutbox links the item an outbox event announces to the contacts of
// its book (idempotent: a link that exists is left alone).
func (s *Service) HandleOutbox(ctx context.Context, ev outbox.Event) error {
	kind, ok := kindOfEvent[ev.Type]
	if !ok {
		return nil
	}
	var body struct {
		RequestID     *uuid.UUID `json:"request_id"`
		EnquiryID     *uuid.UUID `json:"enquiry_id"`
		AppointmentID *uuid.UUID `json:"appointment_id"`
	}
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &body); err != nil {
			return outbox.Permanent(fmt.Errorf("crm: decode %s payload: %w", ev.Type, err))
		}
	}
	id := ev.AggregateID
	for _, p := range []*uuid.UUID{body.RequestID, body.EnquiryID, body.AppointmentID} {
		if p != nil && *p != uuid.Nil {
			id = *p
			break
		}
	}
	if _, err := s.LinkItem(ctx, kind, id); err != nil {
		return fmt.Errorf("crm: link %s %s: %w", kind, id, err)
	}
	return nil
}

// Register adds the linking to the outbox events that announce an item
// (after any handler already registered), and acknowledges
// CONTACT_REMINDER_DUE (nothing is sent for it yet).
func (s *Service) Register(d *outbox.Dispatcher) {
	for _, t := range OutboxEventTypes() {
		d.Also(t, outbox.HandlerFunc(s.HandleOutbox))
	}
	d.Register(EventReminderDue, outbox.HandlerFunc(func(context.Context, outbox.Event) error { return nil }))
}

// ReminderSweepInterval is how often the worker looks for reminders that fell due.
const ReminderSweepInterval = time.Minute

// RunReminderSweeps writes CONTACT_REMINDER_DUE for the reminders that fell
// due, every ReminderSweepInterval, until ctx ends.
func (s *Service) RunReminderSweeps(ctx context.Context) {
	t := time.NewTicker(ReminderSweepInterval)
	defer t.Stop()
	for {
		if n, err := s.SweepReminders(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("contact reminder sweep failed", slog.String("error", err.Error()))
		} else if n > 0 {
			s.logger.Info("contact reminders fell due", slog.Int("count", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
