package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Where the maintenance emails send the managers and the tenant.
const (
	PathAgentMaintenance = "/agent/maintenance/"
	// PathMyTenancies is followed by the tenancy's ID: the tenant's page
	// that shows its maintenance requests.
	PathMyTenancies = "/account/tenancies/"
)

// SetMaintenanceSource supplies the data behind maintenance events (the
// maintenance module's Notices). Without it those events are dead-lettered.
func (h *Handlers) SetMaintenanceSource(source func(ctx context.Context, id uuid.UUID) (maintenance.NotificationInfo, error)) {
	h.maintenance = source
}

// maintenanceNotes builds the emails of one maintenance event: a new
// request (maintenance-request-new) or a status change
// (maintenance-status-changed), to the book's owners and managers (or the
// direct owner) and to the tenancy's tenants with a verified account,
// never to the person who acted. Safe text only: the reference, the status
// and category labels.
func (h *Handlers) maintenanceNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	if h.maintenance == nil {
		return outbox.Permanent(fmt.Errorf("notifications: no maintenance source for event type %q", ev.Type))
	}
	var payload maintenance.EventPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload: %w", ev.Type, err))
	}
	n, err := h.maintenance(ctx, ev.AggregateID)
	if errors.Is(err, maintenance.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	template := "maintenance-request-new"
	status := payload.ToStatus
	if ev.Type == maintenance.OutboxStatusChanged {
		template = "maintenance-status-changed"
	}
	if status == "" {
		status = n.Status
	}
	d := mail.Data{Reference: n.Reference, StatusText: maintenance.StatusLabel(status), Service: n.CategoryLabel, When: occurredAt(ev)}
	actor := func(id *uuid.UUID) bool {
		return id != nil && payload.ActorUserID != nil && *id == *payload.ActorUserID
	}
	var managers []Person
	switch {
	case n.OrganisationID != nil:
		managers, err = h.source.Recipients(ctx, nil, n.OrganisationID)
	case n.OwnerUserID != nil:
		managers, err = h.source.Recipients(ctx, n.OwnerUserID, nil)
	}
	if err != nil {
		return err
	}
	seen := map[uuid.UUID]bool{}
	for _, m := range managers {
		if actor(m.UserID) || (m.UserID != nil && seen[*m.UserID]) {
			continue
		}
		if m.UserID != nil {
			seen[*m.UserID] = true
		}
		md := d
		md.Name, md.Path = m.Name, PathAgentMaintenance+n.RequestID.String()
		if err := add(template, recipientKey(m), m, md); err != nil {
			return err
		}
	}
	if n.TenancyID == nil {
		return nil // no tenancy: no tenant to tell
	}
	for _, t := range n.Tenants {
		id := t.UserID
		if actor(&id) || seen[id] {
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
