package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/httpx"
)

type placementBody struct {
	Placement struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Label  string `json:"label"`
		Status string `json:"status"`
	} `json:"placement"`
}

// API 0.53.0: an administrator grants and ends a sponsored placement; the
// public listing shows it; the grant and the end are audited.
func TestListingPlacements(t *testing.T) {
	e := newEnv(t)
	finance := e.asAdmin(devseed.FinanceAdminEmail)
	listing := propertyrequests.SeedListingID(2).String()
	ends := e.now.Add(7 * 24 * time.Hour).Format(time.RFC3339)

	wantError(t, e.post("/v1/admin/placements", obj{"listing_id": listing, "kind": "SPONSORED", "ends_at": ends}, e.asAdmin(devseed.SupportEmail)),
		http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.post("/v1/admin/placements", obj{"listing_id": listing, "kind": "SPONSORED", "ends_at": ends}), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	rec := e.post("/v1/admin/placements", obj{"listing_id": listing, "kind": "SPONSORED", "ends_at": ends, "note": "Spring campaign"}, finance)
	wantStatus(t, rec, http.StatusCreated)
	p := decode[placementBody](t, rec).Placement
	if p.Kind != "SPONSORED" || p.Label != "Sponsored" || p.Status != "ACTIVE" {
		t.Fatalf("placement: %+v", p)
	}
	wantError(t, e.post("/v1/admin/placements", obj{"listing_id": listing, "kind": "FEATURED", "ends_at": ends}, finance),
		http.StatusConflict, listings.CodePlacementOverlap)

	found, err := e.listings.ByIDs(context.Background(), []uuid.UUID{propertyrequests.SeedListingID(2)})
	if err != nil || len(found) != 1 {
		t.Fatalf("seed listing: %v", err)
	}
	slug := found[0].Slug
	public := func() map[string]any {
		t.Helper()
		rec := e.get("/v1/listings/" + slug)
		wantStatus(t, rec, http.StatusOK)
		return decode[obj](t, rec)["listing"].(map[string]any)
	}
	if l := public(); l["featured"] != true || l["placement"] == nil || l["placement"].(map[string]any)["label"] != "Sponsored" {
		t.Fatalf("public: %v %v", l["featured"], l["placement"])
	}

	rec = e.get("/v1/admin/placements?status=ACTIVE", finance)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[obj](t, rec); got["total"] != float64(1) {
		t.Fatalf("list: %v", got)
	}

	wantError(t, e.post("/v1/admin/placements/"+p.ID+"/end", obj{"reason": ""}, finance), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantStatus(t, e.post("/v1/admin/placements/"+p.ID+"/end", obj{"reason": "Campaign cancelled"}, finance), http.StatusOK)
	wantError(t, e.post("/v1/admin/placements/"+p.ID+"/end", obj{"reason": "again"}, finance), http.StatusConflict, listings.CodePlacementEnded)
	if l := public(); l["featured"] != false || l["placement"] != nil {
		t.Fatalf("after the end: %v %v", l["featured"], l["placement"])
	}

	granted, ended := 0, 0
	for _, ev := range e.audit.All() {
		switch ev.Action {
		case listings.ActionPlacementGranted:
			granted++
		case listings.ActionPlacementEnded:
			ended++
		}
	}
	if granted != 1 || ended != 1 {
		t.Fatalf("audit: %d granted, %d ended", granted, ended)
	}
}
