package app

import (
	"net/http"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// TestMarketDateAfterLocalMidnight: at 22:30 UTC on 1 November it is already
// 2 November in Harare (Zimbabwe, UTC+2). A rent receipt and a repair cost
// dated 2 November are today, not "in the future"; 3 November still is; and
// the ledger and arrears reports default to the market's date.
func TestMarketDateAfterLocalMidnight(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	e.now = time.Date(2026, 11, 1, 22, 30, 0, 0, time.UTC)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	const local, tomorrow = "2026-11-02", "2026-11-03"

	receipts := tenanciesPath(org, "/"+devseed.TenancyCottageID.String()+"/receipts")
	receipt := func(on string) obj {
		return obj{"amount": "10.00", "currency": "USD", "method": "CASH", "received_on": on}
	}
	wantStatus(t, e.post(receipts, receipt(local), agent), http.StatusCreated)
	env := wantError(t, e.post(receipts, receipt(tomorrow), agent), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["received_on"] != "may not be in the future" {
		t.Fatalf("receipt fields: %+v", env.Error.Fields)
	}

	costs := maintenancePath(org, "/"+devseed.MaintenancePowerID.String()+"/costs")
	cost := func(on string) obj {
		return obj{"amount": "12.50", "currency": "USD", "description": "Washer", "method": "CASH", "incurred_on": on}
	}
	wantStatus(t, e.post(costs, cost(local), agent), http.StatusCreated)
	env = wantError(t, e.post(costs, cost(tomorrow), agent), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["incurred_on"] != "may not be in the future" {
		t.Fatalf("cost fields: %+v", env.Error.Fields)
	}

	cashbook := decode[struct {
		From string `json:"from"`
		To   string `json:"to"`
	}](t, e.get(ledgerPath(org, "/reports/cashbook"), staff))
	if cashbook.To != local || cashbook.From != "2026-11-01" {
		t.Fatalf("cashbook range: %+v", cashbook)
	}
	receivables := decode[struct {
		AsOf string `json:"as_of"`
	}](t, e.get(ledgerPath(org, "/reports/receivables"), staff))
	if receivables.AsOf != local {
		t.Fatalf("receivables as_of: %s", receivables.AsOf)
	}
	arrears := decode[struct {
		AsOf string `json:"as_of"`
	}](t, e.get(bookPath(org.String(), "/arrears"), staff))
	if arrears.AsOf != local {
		t.Fatalf("arrears as_of: %s", arrears.AsOf)
	}
}
