package app

import (
	"net/http"
	"strings"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// API 0.56.0: provision a country, try to activate it unready, give it a
// notice and extra currencies; the public country list shows it only when
// active.
func TestMarketAdministration(t *testing.T) {
	e := newEnv(t)
	root := e.asAdmin(devseed.PlatformAdminEmail)

	wantError(t, e.get("/v1/admin/markets", e.asAdmin(devseed.FinanceAdminEmail)), http.StatusForbidden, httpx.CodeForbidden)
	wantStatus(t, e.post("/v1/admin/markets", obj{"code": "QZ", "name": "Test Land", "currency_code": "USD", "timezone": "Africa/Harare"}, root), http.StatusCreated)
	wantError(t, e.post("/v1/admin/markets", obj{"code": "QZ", "name": "Again", "currency_code": "USD", "timezone": "Africa/Harare"}, root),
		http.StatusConflict, markets.CodeMarketExists)

	countries := func() string {
		t.Helper()
		rec := e.get("/v1/countries?q=Test")
		wantStatus(t, rec, http.StatusOK)
		return rec.Body.String()
	}
	if strings.Contains(countries(), `"QZ"`) {
		t.Fatal("a planned country is public")
	}

	rec := e.get("/v1/admin/markets/QZ", root)
	wantStatus(t, rec, http.StatusOK)
	if r := decode[obj](t, rec)["readiness"].(map[string]any); r["ready"] != false {
		t.Fatalf("readiness: %v", r)
	}
	wantError(t, e.patch("/v1/admin/markets/QZ", obj{"status": "ACTIVE"}, root), http.StatusConflict, markets.CodeMarketNotReady)

	rec = e.patch("/v1/admin/markets/QZ", obj{"notice": "Launching soon", "extra_currencies": []string{"ZAR"}}, root)
	wantStatus(t, rec, http.StatusOK)
	if m := decode[obj](t, rec)["market"].(map[string]any); m["notice"] != "Launching soon" {
		t.Fatalf("market: %v", m)
	}
	audits := 0
	for _, ev := range e.audit.All() {
		if ev.TargetType == markets.TargetMarket {
			audits++
		}
	}
	if audits != 2 {
		t.Fatalf("%d market audit events, want 2", audits)
	}
}
