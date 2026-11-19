package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Maintenance emails go to the book's owners and managers and to the
// tenancy's tenants, never to the person who acted; a tenant's link is
// their tenancy's page; the text is the reference and the labels only.
func TestMaintenanceNotifications(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	org, tenancy, request := devseed.SampleEstates.ID, devseed.TenancyFlat1ID, uuid.New()
	f.handlers.SetMaintenanceSource(func(_ context.Context, id uuid.UUID) (maintenance.NotificationInfo, error) {
		if id != request {
			return maintenance.NotificationInfo{}, maintenance.ErrNotFound
		}
		return maintenance.NotificationInfo{RequestID: request, Reference: "RPM-2026-000042", Status: maintenance.StatusTriaged,
			CategoryLabel: "Plumbing", TenancyID: &tenancy, OrganisationID: &org,
			Tenants: []maintenance.Recipient{{UserID: devseed.TenantID, Name: "Tariro Tenant", Email: devseed.TenantEmail}}}, nil
	})
	event := func(typ string, actor uuid.UUID, from, to string) outbox.Event {
		raw, _ := json.Marshal(maintenance.EventPayload{RequestID: request, Reference: "RPM-2026-000042", FromStatus: from, ToStatus: to,
			ActorKind: maintenance.ActorManager, ActorUserID: &actor, OccurredAt: now})
		return outbox.Event{ID: uuid.New(), Type: typ, AggregateType: maintenance.AggregateMaintenance, AggregateID: request, Payload: raw,
			CreatedAt: now}
	}

	// A manager (the owner) changes the status: the tenant is told, the owner is not.
	before := f.sender.count()
	if err := f.handle(event(maintenance.OutboxStatusChanged, devseed.EstatesOwnerID, maintenance.StatusNew, maintenance.StatusTriaged)); err != nil {
		t.Fatal(err)
	}
	sent := f.sender.byKind("maintenance-status-changed")
	if len(sent) == 0 || f.sender.count() == before {
		t.Fatal("no status email")
	}
	tenantSeen := false
	for _, m := range sent {
		if m.To == devseed.EstatesOwnerEmail {
			t.Fatal("the person who acted was told")
		}
		if !strings.Contains(m.Text, "RPM-2026-000042") || !strings.Contains(m.Text, "Triaged") {
			t.Fatalf("status email:\n%s", m.Text)
		}
		if m.To == devseed.TenantEmail {
			tenantSeen = true
			if !strings.Contains(m.Text, "/account/tenancies/"+tenancy.String()) {
				t.Fatalf("the tenant's link:\n%s", m.Text)
			}
		} else if !strings.Contains(m.Text, "/agent/maintenance/"+request.String()) {
			t.Fatalf("a manager's link:\n%s", m.Text)
		}
	}
	if !tenantSeen {
		t.Fatal("the tenant was not told")
	}

	// The tenant raises one: the managers are told, the tenant is not.
	if err := f.handle(event(maintenance.OutboxRequestCreated, devseed.TenantID, "", maintenance.StatusNew)); err != nil {
		t.Fatal(err)
	}
	for _, m := range f.sender.byKind("maintenance-request-new") {
		if m.To == devseed.TenantEmail {
			t.Fatal("the tenant who raised it was told")
		}
	}
	if len(f.sender.byKind("maintenance-request-new")) == 0 {
		t.Fatal("the managers were not told")
	}
	// A request that is gone is dead-lettered.
	gone := event(maintenance.OutboxStatusChanged, devseed.EstatesOwnerID, "", maintenance.StatusClosed)
	gone.AggregateID = uuid.New()
	if err := f.handle(gone); !outbox.IsPermanent(err) {
		t.Fatalf("err = %v, want permanent", err)
	}
}
