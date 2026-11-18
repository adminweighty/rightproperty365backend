package app

import (
	"net/http"
	"strings"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Listing coordinates and map search (API 0.29.0).

type locationContent struct {
	Latitude          *float64 `json:"latitude"`
	Longitude         *float64 `json:"longitude"`
	LocationPrecision string   `json:"location_precision"`
}

type locationView struct {
	ID             string `json:"id"`
	Slug           string `json:"slug"`
	Status         string `json:"status"`
	PendingChanges *struct {
		Status string `json:"status"`
	} `json:"pending_changes"`
	Content      locationContent `json:"content"`
	Completeness struct {
		Ready bool `json:"ready"`
	} `json:"completeness"`
	ReviewKind *string          `json:"review_kind"`
	Live       *locationContent `json:"live"`
	Changes    []struct {
		Field  string `json:"field"`
		Before any    `json:"before"`
		After  any    `json:"after"`
	} `json:"changes"`
}

func locationViewOf(t *testing.T, e *env, status int, path string, body obj, as reqOpt) locationView {
	t.Helper()
	var rec = e.get(path, as)
	if body != nil {
		rec = e.patch(path, body, as)
	}
	wantStatus(t, rec, status)
	return decode[struct {
		Listing locationView `json:"listing"`
	}](t, rec).Listing
}

func wantCoords(t *testing.T, got locationContent, lat, lon float64, precision string) {
	t.Helper()
	if got.Latitude == nil || got.Longitude == nil || *got.Latitude != lat || *got.Longitude != lon || got.LocationPrecision != precision {
		t.Fatalf("coordinates = %v %v %q, want %v %v %s", got.Latitude, got.Longitude, got.LocationPrecision, lat, lon, precision)
	}
}

func TestListingLocationIsSetValidatedAndCleared(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	l := e.readyListing(tendai, "House with a pin on the map")
	path := "/v1/me/listings/" + l.ID

	v := locationViewOf(t, e, http.StatusOK, path, nil, tendai)
	if !v.Completeness.Ready || v.Content.Latitude != nil || v.Content.Longitude != nil || v.Content.LocationPrecision != "EXACT" {
		t.Fatalf("a listing without coordinates is ready, EXACT by default: %+v", v)
	}

	v = locationViewOf(t, e, http.StatusOK, path, obj{"latitude": -17.745211, "longitude": 31.091842}, tendai)
	wantCoords(t, v.Content, -17.745211, 31.091842, "EXACT")
	v = locationViewOf(t, e, http.StatusOK, path, obj{"location_precision": "APPROXIMATE"}, tendai)
	wantCoords(t, v.Content, -17.745211, 31.091842, "APPROXIMATE")

	for _, bad := range []struct {
		body  obj
		field string
		want  string
	}{
		{obj{"longitude": nil}, "longitude", listings.MsgCoordinatePair},
		{obj{"latitude": 0, "longitude": 0}, "latitude", listings.MsgNullIsland},
		{obj{"latitude": 95, "longitude": 31}, "latitude", listings.MsgLatitudeRange},
		{obj{"latitude": -17.7, "longitude": 200}, "longitude", listings.MsgLongitudeRange},
		{obj{"latitude": -17.7452111, "longitude": 31}, "latitude", listings.MsgCoordinateDigits},
		{obj{"latitude": "-17.7", "longitude": 31}, "latitude", listings.MsgCoordinateNumber},
		{obj{"location_precision": "CITY"}, "location_precision", listings.MsgPrecision},
	} {
		env := wantError(t, e.patch(path, bad.body, tendai), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields[bad.field] != bad.want {
			t.Fatalf("%v: fields = %v", bad.body, env.Error.Fields)
		}
	}
	v = locationViewOf(t, e, http.StatusOK, path, nil, tendai)
	wantCoords(t, v.Content, -17.745211, 31.091842, "APPROXIMATE")

	v = locationViewOf(t, e, http.StatusOK, path, obj{"latitude": nil, "longitude": nil}, tendai)
	if v.Content.Latitude != nil || v.Content.Longitude != nil {
		t.Fatalf("cleared: %+v", v.Content)
	}
}

// A location change on a published listing is a content change: it waits
// for review, and the public keeps the old position until the approval.
func TestPublishedListingLocationGoesThroughReview(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	admin := e.admin()
	id := propertyrequests.SeedListingID(1).String()
	path := "/v1/me/listings/" + id

	before := decode[struct{ Listing listings.Listing }](t, e.get("/v1/listings/four-bedroom-family-home-borrowdale")).Listing.Location
	if before == nil || before.Precision != "EXACT" {
		t.Fatalf("the Borrowdale sample has an exact location: %+v", before)
	}
	v := locationViewOf(t, e, http.StatusOK, path, obj{"latitude": -17.741234, "longitude": 31.095678, "location_precision": "APPROXIMATE"}, tendai)
	if v.Status != "PUBLISHED" || v.PendingChanges == nil || v.PendingChanges.Status != "EDITING" {
		t.Fatalf("a new location on a published listing must open its pending changes: %+v", v)
	}
	public := decode[struct{ Listing listings.Listing }](t, e.get("/v1/listings/"+v.Slug)).Listing.Location
	if public == nil || *public != *before {
		t.Fatalf("an unreviewed location is public: %+v", public)
	}

	locationViewOf(t, e, http.StatusOK, path, nil, tendai)
	wantStatus(t, e.post(path+"/submit", nil, tendai), http.StatusOK)
	r := locationViewOf(t, e, http.StatusOK, "/v1/admin/listings/"+id, nil, browser(admin))
	if r.ReviewKind == nil || *r.ReviewKind != "EDIT" || r.Live == nil {
		t.Fatalf("review: %+v", r)
	}
	wantCoords(t, r.Content, -17.741234, 31.095678, "APPROXIMATE")
	changed := map[string]bool{}
	for _, c := range r.Changes {
		changed[c.Field] = true
	}
	if !changed["latitude"] || !changed["longitude"] || !changed["location_precision"] {
		t.Fatalf("the diff must show the location: %+v", r.Changes)
	}

	wantStatus(t, e.post("/v1/admin/listings/"+id+"/approve", nil, browser(admin)), http.StatusOK)
	rec := e.get("/v1/listings/" + v.Slug)
	public = decode[struct{ Listing listings.Listing }](t, rec).Listing.Location
	if public == nil || *public != (listings.Location{Latitude: -17.74, Longitude: 31.1, Precision: "APPROXIMATE"}) {
		t.Fatalf("approved, approximate: %+v", public)
	}
	if body := rec.Body.String(); strings.Contains(body, "741234") || strings.Contains(body, "095678") {
		t.Fatal("the exact coordinates of an APPROXIMATE listing are public")
	}
}

// The seed: the APPROXIMATE sample is rounded everywhere public; the one
// without coordinates has location null and is not mappable.
func TestPublicLocationsAndMapSearch(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/v1/listings/" + devseed.ApproximateListingSlug)
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"location":{"latitude":-18.95,"longitude":32.66,"precision":"APPROXIMATE"}`) ||
		strings.Contains(body, "951234") || strings.Contains(body, "657891") {
		t.Fatalf("approximate sample: %.400s", body)
	}
	if body := e.get("/v1/listings/" + devseed.NoLocationListingSlug).Body.String(); !strings.Contains(body, `"location":null`) {
		t.Fatal("a listing without coordinates must have location: null")
	}

	all := decode[listings.Page](t, e.get("/v1/listings?country=ZW&page_size=48"))
	if all.MappableTotal != all.Total-1 || all.Total < 10 {
		t.Fatalf("ZW: total %d, mappable %d", all.Total, all.MappableTotal)
	}
	search := e.get("/v1/listings?country=ZW&page_size=48")
	if body := search.Body.String(); strings.Contains(body, "951234") || strings.Contains(body, "657891") || !strings.Contains(body, `"mappable_total":`) {
		t.Fatal("the search leaks the exact coordinates of an APPROXIMATE listing, or lacks mappable_total")
	}

	mappable := decode[listings.Page](t, e.get("/v1/listings?country=ZW&has_location=true&page_size=48"))
	if mappable.Total != all.Total-1 || mappable.MappableTotal != mappable.Total {
		t.Fatalf("has_location: total %d, mappable %d", mappable.Total, mappable.MappableTotal)
	}
	for _, l := range mappable.Items {
		if l.Location == nil || l.Slug == devseed.NoLocationListingSlug {
			t.Fatalf("has_location returned %s without coordinates", l.Slug)
		}
	}

	// Bulawayo's box: only Bulawayo, with facets for the box only.
	box := decode[listings.Page](t, e.get("/v1/listings?bbox=28.4,-20.3,28.8,-20.0&page_size=48"))
	if box.Total != 3 || box.MappableTotal != 3 {
		t.Fatalf("bulawayo box: %d (%s)", box.Total, slugs(box.Items))
	}
	for _, l := range box.Items {
		if l.City.Label != "Bulawayo" {
			t.Fatalf("bulawayo box returned %s", l.Slug)
		}
	}
	cities := map[string]int{}
	for _, f := range box.Facets.City {
		cities[f.Label] = f.Count
	}
	if len(cities) != 1 || cities["Bulawayo"] != 3 {
		t.Fatalf("city facet in the box: %v", cities)
	}
	// London's box, with another filter.
	london := decode[listings.Page](t, e.get("/v1/listings?bbox=-0.3,51.4,0.1,51.6&category=short-stay"))
	if london.Total != 1 || london.Items[0].Slug != "serviced-apartment-shoreditch" {
		t.Fatalf("london short stays: %s", slugs(london.Items))
	}

	for _, bad := range []string{"1,2,3", "x,1,2,3", "31,-17,30,-16", "30,-16,31,-17", "170,0,-170,10", "0,-95,1,1"} {
		env := wantError(t, e.get("/v1/listings?bbox="+bad), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields["bbox"] == "" {
			t.Fatalf("bbox=%s: fields %v", bad, env.Error.Fields)
		}
	}
	env := wantError(t, e.get("/v1/listings?has_location=1"), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["has_location"] == "" {
		t.Fatalf("has_location=1: %v", env.Error.Fields)
	}
}
