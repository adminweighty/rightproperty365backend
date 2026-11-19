package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/inspections"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// SetInspectionSource supplies the data behind inspection events (the
// inspections module's Notices). Without it those events are dead-lettered.
func (h *Handlers) SetInspectionSource(source func(ctx context.Context, id uuid.UUID) (inspections.NotificationInfo, error)) {
	h.inspection = source
}

// inspectionNotes builds the emails of one inspection event: scheduled
// (inspection-scheduled) or completed (inspection-completed), to the
// inspection's tenancy's tenants with a verified account only, never to
// the person who acted. Safe text only: the reference, the type label and
// the time in the market's time zone.
func (h *Handlers) inspectionNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	if h.inspection == nil {
		return outbox.Permanent(fmt.Errorf("notifications: no inspection source for event type %q", ev.Type))
	}
	var payload inspections.EventPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload: %w", ev.Type, err))
	}
	n, err := h.inspection(ctx, ev.AggregateID)
	if errors.Is(err, inspections.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if n.TenancyID == nil {
		return nil // no tenancy: nobody to tell
	}
	template := "inspection-completed"
	d := mail.Data{Reference: n.Reference, RequestType: inspections.TypeLabel(n.Type), When: occurredAt(ev)}
	if ev.Type == inspections.OutboxScheduled {
		if n.Status != inspections.StatusScheduled {
			return nil // started, completed or cancelled since: the time no longer matters
		}
		template = "inspection-scheduled"
		d.AppointmentTime = mail.LocalDateTime(n.ScheduledFor, n.Zone)
		if payload.Rescheduled {
			d.StatusText = "Rescheduled"
		}
	}
	seen := map[uuid.UUID]bool{}
	for _, t := range n.Tenants {
		id := t.UserID
		if (payload.ActorUserID != nil && *payload.ActorUserID == id) || seen[id] {
			continue
		}
		seen[id] = true
		td := d
		td.Name, td.Path = t.Name, PathMyTenancies+n.TenancyID.String()
		p := Person{UserID: &id, Name: t.Name, Email: t.Email}
		if err := add(template, recipientKey(p), p, td); err != nil {
			return err
		}
	}
	return nil
}
