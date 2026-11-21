package app

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/branches"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/httpx"
)

type branchBody struct {
	Branch struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"branch"`
}

// API 0.57.0, FR-002 acceptance: "A staff account from branch A receives
// 403 when requesting branch B private objects."
func TestBranches(t *testing.T) {
	e := newEnv(t)
	owner := browser(e.loginCookie(ownerEmail, seedPassword))
	agent := browser(e.loginCookie(agentEmail, seedPassword))

	// Only OWNER and MANAGER manage branches; names are unique.
	wantError(t, e.post(teamPath("/branches"), obj{"name": "Harare North"}, agent), http.StatusForbidden, httpx.CodeForbidden)
	rec := e.post(teamPath("/branches"), obj{"name": "Harare North"}, owner)
	wantStatus(t, rec, http.StatusCreated)
	a := decode[branchBody](t, rec).Branch
	wantError(t, e.post(teamPath("/branches"), obj{"name": "harare  north"}, owner), http.StatusConflict, branches.CodeNameTaken)
	rec = e.post(teamPath("/branches"), obj{"name": "Bulawayo"}, owner)
	wantStatus(t, rec, http.StatusCreated)
	b := decode[branchBody](t, rec).Branch

	// The agent works in branch A; listing 1 (the agent's) belongs to B.
	agentID := e.userID(agentEmail).String()
	listing := propertyrequests.SeedListingID(1).String()
	wantStatus(t, e.put(teamPath("/members/"+agentID+"/branch"), obj{"branch_id": a.ID}, owner), http.StatusNoContent)
	wantError(t, e.put(teamPath("/listings/"+listing+"/branch"), obj{"branch_id": "not-a-uuid"}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantStatus(t, e.put(teamPath("/listings/"+listing+"/branch"), obj{"branch_id": b.ID}, owner), http.StatusNoContent)
	// The pin applies at once: the same session is now scoped.

	// The listing: 403 for the agent, fine for the owner.
	wantError(t, e.get("/v1/me/listings/"+listing, agent), http.StatusForbidden, "BRANCH_FORBIDDEN")
	if rec := e.get("/v1/me/listings/"+listing, owner); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"branch_id":"`+b.ID+`"`) {
		t.Fatalf("owner's listing view %d: no branch_id", rec.Code)
	}

	// A lead on it: hidden from the agent's inbox, 403 on its detail.
	reqID := e.sent(e.propertyRequest(1))
	if slices.Contains(e.inboxListings(agentEmail), 1) {
		t.Fatal("the agent's inbox shows a lead of branch B")
	}
	if !slices.Contains(e.inboxListings(ownerEmail), 1) {
		t.Fatal("the owner's inbox lost the lead")
	}
	wantError(t, e.get("/v1/agent/property-requests/"+reqID, agent), http.StatusForbidden, "BRANCH_FORBIDDEN")

	// A ledger entry of branch B: 403 for the agent, hidden from their list.
	org := devseed.SampleEstates.ID
	rec = e.post(ledgerPath(org, "/entries"), obj{"type": "INCOME", "category": "commission", "method": "CASH", "amount": "10", "currency": "USD",
		"effective_date": "2026-11-01", "branch": "bulawayo", "description": "Branch B income"}, owner)
	wantStatus(t, rec, http.StatusCreated)
	entry := decode[ledgerPosted](t, rec).Entry
	wantError(t, e.get(ledgerPath(org, "/entries/"+entry.ID), agent), http.StatusForbidden, "BRANCH_FORBIDDEN")
	wantStatus(t, e.get(ledgerPath(org, "/entries/"+entry.ID), owner), http.StatusOK)
	if rec := e.get(ledgerPath(org, "/entries"), agent); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), entry.ID) {
		t.Fatalf("the agent's ledger list: %d, shows branch B's entry: %v", rec.Code, strings.Contains(rec.Body.String(), entry.ID))
	}
	if rec := e.get(ledgerPath(org, "/entries"), owner); !strings.Contains(rec.Body.String(), entry.ID) {
		t.Fatal("the owner's ledger list lost the entry")
	}

	// Unpinned again: everything is back.
	wantStatus(t, e.put(teamPath("/members/"+agentID+"/branch"), obj{"branch_id": nil}, owner), http.StatusNoContent)
	wantStatus(t, e.get("/v1/me/listings/"+listing, agent), http.StatusOK)

	audits := 0
	for _, ev := range e.team.AuditLog() {
		switch ev.Action {
		case branches.ActionCreated, branches.ActionMemberBranch, branches.ActionListingBranch:
			audits++
		}
	}
	if audits != 5 {
		t.Fatalf("%d branch audit events, want 5", audits)
	}
}
