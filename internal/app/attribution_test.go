package app

import (
	"net/http"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/analytics"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/attribution"
)

// API 0.58.0, FR-013: the campaign of a visit is stored with its property
// request and its analytics events, shown on the lead and in the funnel, and
// dropped under Do Not Track or Global Privacy Control.
func TestCampaignAttribution(t *testing.T) {
	e := newEnv(t)
	owner := withBearer(e.loginBearer(devseed.EstatesOwnerEmail, seedPassword))
	agent := withBearer(e.loginBearer(identity.SeedAgentEmail, seedPassword))

	body := e.propertyRequest(1)
	body["campaign"] = obj{"source": " Facebook ", "medium": "CPC", "campaign": "Spring <Sale>"}
	tracked := e.sent(body)
	body = e.propertyRequest(1)
	body["campaign"] = obj{"source": "facebook", "medium": "cpc", "campaign": "spring sale"}
	untracked := e.sent(body, withHeader("Sec-GPC", "1"))
	plain := e.sent(e.propertyRequest(1))

	type lead struct {
		Request struct {
			Campaign *attribution.Campaign `json:"campaign"`
		} `json:"request"`
	}
	want := attribution.Campaign{Source: "facebook", Medium: "cpc", Campaign: "spring sale"}
	for id, expect := range map[string]*attribution.Campaign{tracked: &want, untracked: nil, plain: nil} {
		rec := e.get("/v1/agent/property-requests/"+id, agent)
		wantStatus(t, rec, http.StatusOK)
		got := decode[lead](t, rec).Request.Campaign
		if (got == nil) != (expect == nil) || (got != nil && *got != *expect) {
			t.Fatalf("request %s: campaign %+v, want %+v", id, got, expect)
		}
	}

	// The funnel: the campaign's row, then the requests without one.
	type funnel struct {
		ByCampaign []struct {
			Campaign *attribution.Campaign `json:"campaign"`
			Received int                   `json:"received"`
		} `json:"by_campaign"`
	}
	rec := e.get("/v1/agent/organisations/"+devseed.SampleEstates.ID.String()+"/reports/lead-funnel", owner)
	wantStatus(t, rec, http.StatusOK)
	rows := decode[funnel](t, rec).ByCampaign
	if len(rows) < 2 || rows[0].Campaign == nil || *rows[0].Campaign != want || rows[0].Received != 1 || rows[len(rows)-1].Campaign != nil {
		t.Fatalf("by campaign: %+v", rows)
	}

	// Analytics events keep the campaign, except under Do Not Track.
	tendai, _, _ := analyticsSamples(t, e.now)
	ev := analyticsEvent(analytics.TypeDetailView, tendai.ID, e.now, "sessionCAMPAIGN000000001")
	ev["campaign"] = obj{"source": "Newsletter", "medium": "email"}
	wantStatus(t, e.sendAnalytics([]obj{ev}), http.StatusAccepted)
	ev2 := analyticsEvent(analytics.TypeDetailView, tendai.ID, e.now, "sessionCAMPAIGN000000002")
	ev2["campaign"] = obj{"source": "newsletter"}
	wantStatus(t, e.sendAnalytics([]obj{ev2}, withHeader("DNT", "1")), http.StatusAccepted)
	got := map[string]attribution.Campaign{}
	for _, s := range e.analytics.Events() {
		got[s.Session] = s.Campaign
	}
	if got["sessionCAMPAIGN000000001"] != (attribution.Campaign{Source: "newsletter", Medium: "email"}) || !got["sessionCAMPAIGN000000002"].Empty() {
		t.Fatalf("analytics campaigns: %+v", got)
	}
}
