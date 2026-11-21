package app

import (
	"net/http"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

// The owner's lifecycle actions (API 0.48.0) through the full router: pause
// keeps the package place and resume brings the listing back; sold frees the
// place; the type decides sold or let; relist from a final state.
func TestListingLifecycle(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	tendai, blake := c.as(identity.SeedAgentEmail), c.as(identity.SeedAgentBEmail)
	id := propertyrequests.SeedListingID(1).String()
	path := "/v1/me/listings/" + id
	usage := func() int {
		t.Helper()
		b := decode[accountUsageBody](t, e.get("/v1/agent/organisations/"+devseed.SampleEstates.ID.String()+"/package", tendai))
		return b.market(t, "ZW").Used
	}

	l := managedOf(t, e.get(path, tendai), http.StatusOK)
	if l.Status != "PUBLISHED" || !contains(l.Actions, "pause") || !contains(l.Actions, "mark_sold") || contains(l.Actions, "mark_let") ||
		!contains(l.Actions, "close") || contains(l.Actions, "resume") {
		t.Fatalf("published actions: %v", l.Actions)
	}
	used := usage()
	wantError(t, e.post(path+"/pause", nil, blake), http.StatusNotFound, listings.CodeListingNotFound)
	wantError(t, e.post(path+"/mark-let", nil, tendai), http.StatusConflict, listings.CodeTypeConflict)
	wantError(t, e.post(path+"/resume", nil, tendai), http.StatusConflict, listings.CodeStateConflict)

	p := managedOf(t, e.post(path+"/pause", nil, tendai), http.StatusOK)
	if p.Status != "PAUSED" || p.Public || !contains(p.Actions, "resume") || contains(p.Actions, "pause") {
		t.Fatalf("paused: %+v", p)
	}
	if usage() != used {
		t.Fatalf("pausing changed the package usage: %d -> %d", used, usage())
	}
	if events := e.outboxTypes(id); !contains(events, listings.EventListingUnpublished) {
		t.Fatalf("pause events: %v", events)
	}
	wantError(t, e.get("/v1/listings/"+l.Slug), http.StatusGone, listings.CodeListingUnavailable)

	r := managedOf(t, e.post(path+"/resume", nil, tendai), http.StatusOK)
	if r.Status != "PUBLISHED" || !r.Public {
		t.Fatalf("resumed: %+v", r)
	}
	wantStatus(t, e.get("/v1/listings/"+l.Slug), http.StatusOK)

	s := managedOf(t, e.post(path+"/mark-sold", nil, tendai), http.StatusOK)
	if s.Status != "SOLD" || s.Public || !contains(s.Actions, "relist") || contains(s.Actions, "pause") {
		t.Fatalf("sold: %+v", s)
	}
	if usage() != used-1 {
		t.Fatalf("selling did not free the place: %d -> %d", used, usage())
	}
	wantError(t, e.post(path+"/close", nil, tendai), http.StatusConflict, listings.CodeStateConflict)
	back := managedOf(t, e.post(path+"/relist", nil, tendai), http.StatusOK)
	if back.Status != "PENDING_REVIEW" {
		t.Fatalf("relisted: %+v", back)
	}
}
