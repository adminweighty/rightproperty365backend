package analytics

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/attribution"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// API 0.58.0: enquiry_submit keeps the property request's campaign.
func TestRecorderKeepsTheEnquiryCampaign(t *testing.T) {
	store := NewMemoryStore()
	r := newRig(t, store)
	r.now = time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	l := r.listing("ZW", true)
	rec := NewRecorder(r.svc, nil)
	for _, c := range []*attribution.Campaign{{Source: "facebook", Medium: "cpc", Campaign: "spring sale"}, nil} {
		body := map[string]any{"listing_id": l, "created_at": r.now}
		if c != nil {
			body["campaign"] = c
		}
		payload, _ := json.Marshal(body)
		if err := rec.Handle(context.Background(), outbox.Event{ID: uuid.New(), Type: propertyrequests.EventPropertyRequestCreated, Payload: payload, CreatedAt: r.now}); err != nil {
			t.Fatal(err)
		}
	}
	events := store.Events()
	if len(events) != 2 || events[0].Type != TypeEnquirySubmit ||
		events[0].Campaign != (attribution.Campaign{Source: "facebook", Medium: "cpc", Campaign: "spring sale"}) || !events[1].Campaign.Empty() {
		t.Fatalf("events %+v", events)
	}
}
