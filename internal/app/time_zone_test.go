package app

import (
	"net/http"
	"strings"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/httpx"
)

type timeZoneBody struct {
	Mode       string  `json:"mode"`
	Zone       *string `json:"zone"`
	MarketCode string  `json:"market_code"`
	MarketZone string  `json:"market_zone"`
}

// API 0.62.0: the personal display zone. The default is market time (Harare
// for Zimbabwe), the setting is stored on the account and returned by
// /v1/me and the sign-in answers, and inspections name their place's zone.
func TestPersonalTimeZone(t *testing.T) {
	e := newEnv(t)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	me := func(who reqOpt) timeZoneBody {
		body := decode[struct {
			User struct {
				TimeZone timeZoneBody `json:"time_zone"`
			} `json:"user"`
		}](t, e.get("/v1/me", who))
		return body.User.TimeZone
	}

	// The default: market time, for a member (their organisation's market)
	// and for someone with no membership (the home market).
	for name, who := range map[string]reqOpt{"agent": agent, "consumer": consumer} {
		if got := me(who); got.Mode != "MARKET" || got.Zone != nil || got.MarketCode != "ZW" || got.MarketZone != "Africa/Harare" {
			t.Fatalf("%s default: %+v", name, got)
		}
	}
	if got := decode[timeZoneBody](t, e.get("/v1/me/time-zone", agent)); got.Mode != "MARKET" || got.MarketZone != "Africa/Harare" {
		t.Fatalf("GET /v1/me/time-zone: %+v", got)
	}

	// A named zone: stored, and shown by /v1/me and the next sign-in.
	rec := e.put("/v1/me/time-zone", obj{"mode": "ZONE", "zone": "Europe/London"}, agent)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[timeZoneBody](t, rec); got.Mode != "ZONE" || got.Zone == nil || *got.Zone != "Europe/London" || got.MarketZone != "Africa/Harare" {
		t.Fatalf("PUT ZONE: %+v", got)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control %q", cc)
	}
	if got := me(agent); got.Mode != "ZONE" || *got.Zone != "Europe/London" {
		t.Fatalf("/v1/me after PUT: %+v", got)
	}
	login := e.post("/v1/auth/login", obj{"email": identity.SeedAgentEmail, "password": seedPassword})
	if !strings.Contains(login.Body.String(), `"time_zone":{"mode":"ZONE","zone":"Europe/London","market_code":"ZW","market_zone":"Africa/Harare"}`) {
		t.Fatalf("sign-in answer: %s", login.Body)
	}
	// Someone else's setting is untouched.
	if got := me(consumer); got.Mode != "MARKET" {
		t.Fatalf("consumer changed: %+v", got)
	}

	// DEVICE: the client resolves it; no zone is stored.
	rec = e.put("/v1/me/time-zone", obj{"mode": "DEVICE"}, agent)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[timeZoneBody](t, rec); got.Mode != "DEVICE" || got.Zone != nil {
		t.Fatalf("PUT DEVICE: %+v", got)
	}

	// Refusals.
	for _, c := range []struct {
		body  obj
		field string
	}{
		{obj{}, "mode"},
		{obj{"mode": "SERVER"}, "mode"},
		{obj{"mode": 3}, "mode"},
		{obj{"mode": "ZONE"}, "zone"},
		{obj{"mode": "ZONE", "zone": "Mars/Olympus"}, "zone"},
		{obj{"mode": "ZONE", "zone": "Local"}, "zone"},
		{obj{"mode": "ZONE", "zone": 2}, "zone"},
		{obj{"mode": "MARKET", "zone": "Europe/London"}, "zone"},
	} {
		env := wantError(t, e.put("/v1/me/time-zone", c.body, agent), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields[c.field] == "" {
			t.Errorf("%v: fields %v, want %s", c.body, env.Error.Fields, c.field)
		}
	}
	if got := me(agent); got.Mode != "DEVICE" {
		t.Fatalf("a refused PUT changed the setting: %+v", got)
	}
	wantStatus(t, e.put("/v1/me/time-zone", obj{"mode": "MARKET", "zone": nil}, agent), http.StatusOK)
	if got := me(agent); got.Mode != "MARKET" || got.Zone != nil {
		t.Fatalf("back to market: %+v", got)
	}
	wantError(t, e.get("/v1/me/time-zone"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.put("/v1/me/time-zone", obj{"mode": "MARKET"}), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// Inspections name their property's zone (class B: place-bound times).
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	org := devseed.SampleEstates.ID
	for _, path := range []string{inspectionPath(org, ""), inspectionPath(org, "/"+devseed.InspectionMoveInID.String())} {
		rec := e.get(path, staff)
		wantStatus(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), `"time_zone":"Africa/Harare"`) {
			t.Fatalf("%s: no time_zone: %.300s", path, rec.Body)
		}
	}
}
