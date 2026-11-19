package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/inspections"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Inspection emails go to the tenancy's tenants only (never the managers,
// never the person who acted); the link is the tenancy's page; the text is
// the reference, the type and the time only.
func TestInspectionNotifications(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	tenancy, inspection := devseed.TenancyFlat1ID, uuid.New()
	status := inspections.StatusScheduled
	f.handlers.SetInspectionSource(func(_ context.Context, id uuid.UUID) (inspections.NotificationInfo, error) {
		if id != inspection {
			return inspections.NotificationInfo{}, inspections.ErrNotFound
		}
		return inspections.NotificationInfo{InspectionID: inspection, Reference: "RPI-2026-000042", Type: inspections.TypeRoutine,
			Status: status, ScheduledFor: time.Date(2026, 11, 26, 7, 0, 0, 0, time.UTC), Zone: "Africa/Harare", TenancyID: &tenancy,
			Tenants: []inspections.Recipient{{UserID: devseed.TenantID, Name: "Tariro Tenant", Email: devseed.TenantEmail}}}, nil
	})
	event := func(typ string, actor uuid.UUID, rescheduled bool) outbox.Event {
		raw, _ := json.Marshal(inspections.EventPayload{InspectionID: inspection, Reference: "RPI-2026-000042", Type: inspections.TypeRoutine,
			Status: status, TenancyID: &tenancy, Rescheduled: rescheduled, ActorUserID: &actor, OccurredAt: now})
		return outbox.Event{ID: uuid.New(), Type: typ, AggregateType: inspections.AggregateInspection, AggregateID: inspection, Payload: raw,
			CreatedAt: now}
	}

	if err := f.handle(event(inspections.OutboxScheduled, devseed.EstatesAgent2ID, false)); err != nil {
		t.Fatal(err)
	}
	sent := f.sender.byKind("inspection-scheduled")
	if len(sent) != 1 || sent[0].To != devseed.TenantEmail {
		t.Fatalf("scheduled emails: %+v", sent)
	}
	text := sent[0].Text
	for _, want := range []string{"RPI-2026-000042", "Routine", "Thursday 26 November 2026, 09:00 (Central Africa Time)",
		"/account/tenancies/" + tenancy.String()} {
		if !strings.Contains(text, want) {
			t.Fatalf("scheduled email lacks %q:\n%s", want, text)
		}
	}
	for _, banned := range []string{"Avondale", "Flat 1", "Chipo", "Kudzai", "+263"} {
		if strings.Contains(text, banned) {
			t.Fatalf("scheduled email carries %q:\n%s", banned, text)
		}
	}
	// A reschedule says so.
	if err := f.handle(event(inspections.OutboxScheduled, devseed.EstatesAgent2ID, true)); err != nil {
		t.Fatal(err)
	}
	if again := f.sender.byKind("inspection-scheduled"); len(again) != 2 || !strings.Contains(again[1].Text, "rescheduled") {
		t.Fatalf("rescheduled: %+v", again)
	}
	// Completed: the report is ready; the tenant is told, nobody else.
	status = inspections.StatusCompleted
	before := f.sender.count()
	if err := f.handle(event(inspections.OutboxCompleted, devseed.EstatesAgent2ID, false)); err != nil {
		t.Fatal(err)
	}
	done := f.sender.byKind("inspection-completed")
	if len(done) != 1 || f.sender.count() != before+1 || done[0].To != devseed.TenantEmail || !strings.Contains(done[0].Text, "RPI-2026-000042") {
		t.Fatalf("completed emails: %+v", done)
	}
	// A scheduled event handled after completion sends nothing.
	if err := f.handle(event(inspections.OutboxScheduled, devseed.EstatesAgent2ID, false)); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.byKind("inspection-scheduled")) != 2 {
		t.Fatal("a stale scheduled event was sent")
	}
	// The tenant who acted is not told (defensive: tenants do not write).
	if err := f.handle(event(inspections.OutboxCompleted, devseed.TenantID, false)); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.byKind("inspection-completed")) != 1 {
		t.Fatal("the person who acted was told")
	}
	// An inspection that is gone is dead-lettered.
	gone := event(inspections.OutboxCompleted, devseed.EstatesAgent2ID, false)
	gone.AggregateID = uuid.New()
	if err := f.handle(gone); !outbox.IsPermanent(err) {
		t.Fatalf("err = %v, want permanent", err)
	}
}
