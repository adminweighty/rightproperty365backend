package app

import (
	"net/http"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Reassigning a routing exception (API 0.49.0) through the full router:
// the candidates of the request's country, the checks, and the request
// reaching the chosen member's inbox while staying in the resolved list.
func TestRerouteRoutingException(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	org := propertyrequests.SeedOrganisationID
	e.requests.PutListing(propertyrequests.Listing{
		ID: propertyrequests.SeedListingID(32), MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
		OrganisationID: &org, OrganisationEligible: false,
	})
	exception := e.sent(e.propertyRequest(32))
	super := browser(e.admin())
	gb := browser(e.platformAdmin("gb.admin@example.test", auth.PlatformMarketAdmin, "GB"))
	base := "/v1/admin/routing-exceptions/" + exception

	// The candidates: organisations of Zimbabwe with members who hold leads.
	wantError(t, e.get(base+"/candidates", gb), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
	cands := list(decode[obj](t, e.get(base+"/candidates", super)), "organisations")
	var estates obj
	for _, x := range cands {
		if x.(obj)["organisation_id"] == devseed.SampleEstates.ID.String() {
			estates = x.(obj)
		}
		if x.(obj)["kind"] == "SERVICE_PROVIDER" {
			t.Fatalf("a business that does not manage listings is a candidate: %v", x)
		}
	}
	if estates == nil || len(list(estates, "members")) == 0 {
		t.Fatalf("candidates: %v", cands)
	}
	agent := identity.SeedAgentUserID

	wantError(t, e.post(base+"/assign", obj{"organisation_id": "not-a-uuid"}, super), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.post(base+"/assign", obj{"organisation_id": devseed.SampleEstates.ID.String(), "user_id": identity.SeedConsumerUserID.String()}, super),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.post(base+"/assign", obj{"organisation_id": devseed.SampleEstates.ID.String()}, gb), http.StatusNotFound, propertyrequests.CodeRequestNotFound)

	rec := e.post(base+"/assign", obj{"organisation_id": devseed.SampleEstates.ID.String(), "user_id": agent.String(), "note": "Owner confirmed by phone."}, super)
	wantStatus(t, rec, http.StatusOK)
	if r := decode[obj](t, rec)["request"].(obj); r["state"] != "RESOLVED" {
		t.Fatalf("rerouted: %v", r)
	}
	wantError(t, e.post(base+"/assign", obj{"organisation_id": devseed.SampleEstates.ID.String()}, super), http.StatusConflict, propertyrequests.CodeAlreadyResolved)

	// In the agent's inbox now, assigned to them; still listed as resolved.
	got := requestOf(t, e.get("/v1/agent/property-requests/"+exception, c.as(identity.SeedAgentEmail)))
	if got["stage"] != "ASSIGNED" || got["assigned_to"].(obj)["user_id"] != agent.String() {
		t.Fatalf("inbox: %v", got)
	}
	resolved := list(decode[obj](t, e.get("/v1/admin/routing-exceptions?state=RESOLVED", super)), "items")
	found := false
	for _, x := range resolved {
		found = found || x.(obj)["request_id"] == exception
	}
	if !found {
		t.Fatalf("not in the resolved list: %v", resolved)
	}
	rerouted := false
	for _, ev := range e.requests.Outbox() {
		rerouted = rerouted || (ev.AggregateID.String() == exception && ev.EventType == propertyrequests.EventRerouted)
	}
	if !rerouted {
		t.Fatal("no PROPERTY_REQUEST_REROUTED event")
	}
}
