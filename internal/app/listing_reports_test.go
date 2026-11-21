package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/httpx"
)

type reportList struct {
	Items []struct {
		ID       string `json:"id"`
		Reason   string `json:"reason"`
		Status   string `json:"status"`
		Reporter string `json:"reporter"`
		Listing  struct {
			ID          string `json:"id"`
			OpenReports int    `json:"open_reports"`
		} `json:"listing"`
	} `json:"items"`
	Total int `json:"total"`
}

// API 0.54.0: anyone reports a listing; a moderator warns, then takes it
// down; the reports close; everything is audited.
func TestListingReports(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	id := propertyrequests.SeedListingID(3)
	found, err := e.listings.ByIDs(context.Background(), []uuid.UUID{id})
	if err != nil || len(found) != 1 {
		t.Fatal(err)
	}
	slug := found[0].Slug
	path := "/v1/listings/" + slug + "/reports"

	wantError(t, e.post(path, obj{"reason": "NOPE"}), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.post(path, obj{"reason": "OTHER"}), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.post("/v1/listings/no-such-listing/reports", obj{"reason": "FRAUD"}), http.StatusNotFound, listings.CodeListingNotFound)
	wantStatus(t, e.post(path, obj{"reason": "WRONG_PRICE", "note": "The price is per week, not per month"}), http.StatusAccepted)
	consumer := c.as(identity.SeedConsumerEmail)
	for range 2 { // the same reporter twice: one report
		wantStatus(t, e.post(path, obj{"reason": "FRAUD", "note": "Asked for a deposit by WhatsApp"}, consumer, withIP("198.18.7.7")), http.StatusAccepted)
	}

	mod := e.asAdmin(devseed.ModeratorEmail)
	wantError(t, e.get("/v1/admin/listing-reports", e.asAdmin(devseed.FinanceAdminEmail)), http.StatusForbidden, httpx.CodeForbidden)
	rec := e.get("/v1/admin/listing-reports?status=OPEN", mod)
	wantStatus(t, rec, http.StatusOK)
	q := decode[reportList](t, rec)
	if q.Total != 2 || q.Items[0].Listing.OpenReports != 2 {
		t.Fatalf("queue: %+v", q)
	}
	var signedIn, guest string
	for _, it := range q.Items {
		if it.Reporter == "guest" {
			guest = it.ID
		} else {
			signedIn = it.ID
		}
	}

	// The flags on the moderator's view of the listing.
	rec = e.get("/v1/admin/listings/"+id.String(), mod)
	wantStatus(t, rec, http.StatusOK)
	if _, ok := decode[obj](t, rec)["listing"].(map[string]any)["flags"]; !ok {
		t.Fatal("no flags on the admin view")
	}

	// Dismiss the guest's; warn on the other; then the listing is still public.
	wantError(t, e.post("/v1/admin/listing-reports/"+guest+"/dismiss", obj{"note": ""}, mod), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantStatus(t, e.post("/v1/admin/listing-reports/"+guest+"/dismiss", obj{"note": "The price is right."}, mod), http.StatusOK)
	wantError(t, e.post("/v1/admin/listing-reports/"+guest+"/warn", obj{"message": "x"}, mod), http.StatusConflict, listings.CodeReportDecided)
	wantStatus(t, e.post("/v1/admin/listing-reports/"+signedIn+"/warn", obj{"message": "Customers report a deposit request outside the platform."}, mod), http.StatusOK)
	wantStatus(t, e.get("/v1/listings/"+slug), http.StatusOK)
	warned := 0
	for _, ev := range e.listings.Outbox() {
		if ev.AggregateID == id && ev.Type == listings.EventListingWarned {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("%d warnings", warned)
	}

	// A new report, then the take-down: the listing leaves the public views.
	wantStatus(t, e.post(path, obj{"reason": "FRAUD", "note": "Again"}, consumer, withIP("198.18.7.8")), http.StatusAccepted)
	rec = e.get("/v1/admin/listing-reports?status=OPEN", mod)
	open := decode[reportList](t, rec)
	if open.Total != 1 {
		t.Fatalf("open: %+v", open)
	}
	wantStatus(t, e.post("/v1/admin/listing-reports/"+open.Items[0].ID+"/unpublish", obj{"reason": "Reported as a scam: a deposit was asked for outside the platform."}, mod), http.StatusOK)
	if rec := e.get("/v1/listings/" + slug); rec.Code == http.StatusOK {
		t.Fatal("still public after the take-down")
	}
	if got := decode[reportList](t, e.get("/v1/admin/listing-reports?status=OPEN", mod)); got.Total != 0 {
		t.Fatalf("still open: %+v", got)
	}

	actions := map[string]int{}
	for _, ev := range e.audit.All() {
		actions[ev.Action]++
	}
	if actions[listings.ActionReportDismissed] != 1 || actions[listings.ActionWarnedOnReport] != 1 || actions[listings.ActionUnpublishedOnReport] != 1 {
		t.Fatalf("audit: %v", actions)
	}
}
