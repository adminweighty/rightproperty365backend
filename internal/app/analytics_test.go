package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/analytics"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/outbox"
)

type analyticsTotals struct {
	Views         int `json:"views"`
	UniqueViews   int `json:"unique_views"`
	Impressions   int `json:"impressions"`
	ContactClicks int `json:"contact_clicks"`
	Saves         int `json:"saves"`
	Enquiries     int `json:"enquiries"`
}

type analyticsReportBody struct {
	Listing *struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Reference string `json:"reference"`
	} `json:"listing"`
	From         string          `json:"from"`
	To           string          `json:"to"`
	NearRealTime bool            `json:"near_real_time"`
	Totals       analyticsTotals `json:"totals"`
	Daily        []struct {
		Date  string `json:"date"`
		Views int    `json:"views"`
	} `json:"daily"`
	Sources []struct {
		Key   string `json:"key"`
		Count int    `json:"count"`
	} `json:"sources"`
	Devices           []struct{ Key string } `json:"devices"`
	Conversion        *float64               `json:"conversion"`
	CTR               *float64               `json:"ctr"`
	RefreshedAt       *time.Time             `json:"refreshed_at"`
	FilteredBotEvents int                    `json:"filtered_bot_events"`
	LateEventsDropped int                    `json:"late_events_dropped"`
	ListingsCount     int                    `json:"listings_count"`
	TopListings       []struct {
		ListingID  string  `json:"listing_id"`
		Title      string  `json:"title"`
		Reference  string  `json:"reference"`
		Slug       *string `json:"slug"`
		MarketCode string  `json:"market_code"`
		Views      int     `json:"views"`
	} `json:"top_listings"`
	Markets []struct {
		MarketCode string          `json:"market_code"`
		Totals     analyticsTotals `json:"totals"`
	} `json:"markets"`
}

type ingestBody struct {
	Accepted   int `json:"accepted"`
	Duplicates int `json:"duplicates"`
	Rejected   int `json:"rejected"`
}

func analyticsEvent(eventType string, listing uuid.UUID, at time.Time, session string) obj {
	o := obj{"event_id": uuid.NewString(), "type": eventType, "occurred_at": at.UTC().Format(time.RFC3339),
		"listing_id": listing.String(), "source": "direct", "device": "mobile", "consent": obj{"analytics": true}}
	if session != "" {
		o["session"] = session
	}
	return o
}

const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Version/18.0 Safari/605.1.15"

func (e *env) sendAnalytics(events []obj, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.post("/v1/analytics/events", obj{"schema": 1, "events": events}, append([]reqOpt{withHeader("User-Agent", browserUA)}, opts...)...)
}

// analyticsSamples returns public Sample Estates listings of Tendai (the seed
// agent) and of Chipo, and a public listing of another organisation.
func analyticsSamples(t *testing.T, now time.Time) (tendai, chipo, foreign listings.Record) {
	t.Helper()
	for _, r := range devseed.ListingRecords(now) {
		if !r.Public(now) || r.OrganisationID == nil {
			continue
		}
		switch {
		case *r.OrganisationID == devseed.SampleEstates.ID && r.AgentUserID != nil && *r.AgentUserID == identity.SeedAgentUserID && tendai.ID == uuid.Nil:
			tendai = r
		case *r.OrganisationID == devseed.SampleEstates.ID && r.AgentUserID != nil && *r.AgentUserID == devseed.EstatesAgent2ID && chipo.ID == uuid.Nil:
			chipo = r
		case *r.OrganisationID == devseed.SampleCommercial.ID && foreign.ID == uuid.Nil:
			foreign = r
		}
	}
	if tendai.ID == uuid.Nil || chipo.ID == uuid.Nil || foreign.ID == uuid.Nil {
		t.Fatal("sample listings missing")
	}
	return tendai, chipo, foreign
}

func TestAnalyticsIngestionRoute(t *testing.T) {
	e := newEnv(t)
	tendai, _, _ := analyticsSamples(t, e.now)
	ev := analyticsEvent(analytics.TypeDetailView, tendai.ID, e.now, "sessionAAAAAAAAAAAAAAAA1")

	// Public, Do Not Track not read server-side (the browser does not send under it).
	rec := e.sendAnalytics([]obj{ev, ev, analyticsEvent(analytics.TypeListingSave, tendai.ID, e.now, "")}, withHeader("DNT", "1"), withIP("203.0.113.77"))
	wantStatus(t, rec, http.StatusAccepted)
	if got := decode[ingestBody](t, rec); got != (ingestBody{Accepted: 1, Duplicates: 1, Rejected: 1}) {
		t.Fatalf("ingest: %+v", got)
	}
	stored := e.analytics.Events()
	if len(stored) != 1 || stored[0].Network != "203.0.113.0/24" || stored[0].MarketCode != tendai.MarketCode {
		t.Fatalf("stored: %+v", stored)
	}

	// sendBeacon's text/plain body.
	raw, _ := json.Marshal(obj{"schema": 1, "events": []obj{analyticsEvent(analytics.TypeContactClick, tendai.ID, e.now, "")}})
	req := httptest.NewRequest(http.MethodPost, "/v1/analytics/events", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("User-Agent", browserUA)
	res := httptest.NewRecorder()
	e.handler.ServeHTTP(res, req)
	wantStatus(t, res, http.StatusAccepted)

	// A signed-in customer may send too (cookie + allowed Origin).
	wantStatus(t, e.sendAnalytics([]obj{analyticsEvent(analytics.TypeCardImpression, tendai.ID, e.now, "")}, e.as(identity.SeedConsumerEmail)), http.StatusAccepted)

	// Request-level errors.
	wantError(t, e.post("/v1/analytics/events", obj{"schema": 2, "events": []obj{ev}}), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.post("/v1/analytics/events", obj{"schema": 1, "events": []obj{}}), http.StatusBadRequest, httpx.CodeValidationFailed)
	big := make([]obj, analytics.MaxEventsPerBatch+1)
	for i := range big {
		big[i] = ev
	}
	wantError(t, e.post("/v1/analytics/events", obj{"schema": 1, "events": big}), http.StatusBadRequest, httpx.CodeValidationFailed)
	huge := obj{"schema": 1, "events": []obj{ev}, "padding": strings.Repeat("x", analytics.MaxBodyBytes)}
	wantError(t, e.post("/v1/analytics/events", huge), http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge)
	wantError(t, e.do(http.MethodPost, "/v1/analytics/events", nil, withHeader("Content-Type", "text/html")), http.StatusUnsupportedMediaType, httpx.CodeUnsupportedMediaType)

	// Its own rate limit: a burst of 60 per IP.
	limited := false
	for i := 0; i < 70 && !limited; i++ {
		limited = e.sendAnalytics([]obj{ev}, withIP("198.51.100.20")).Code == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("no rate limit on POST /v1/analytics/events")
	}
}

func TestAnalyticsReportRoutes(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	tendai, chipo, foreign := analyticsSamples(t, e.now)
	at := e.now.Add(-time.Hour)
	var events []obj
	for i := range 3 {
		events = append(events, analyticsEvent(analytics.TypeDetailView, tendai.ID, at, fmt.Sprintf("tendaiSession%011d", i)))
	}
	events = append(events, analyticsEvent(analytics.TypeDetailView, chipo.ID, at, "chipoSession0000000001"),
		analyticsEvent(analytics.TypeContactClick, tendai.ID, at, ""), analyticsEvent(analytics.TypeDetailView, foreign.ID, at, ""))
	wantStatus(t, e.sendAnalytics(events), http.StatusAccepted)
	// A bot is stored, not counted.
	wantStatus(t, e.post("/v1/analytics/events", obj{"schema": 1, "events": []obj{analyticsEvent(analytics.TypeDetailView, tendai.ID, at, "")}},
		withHeader("User-Agent", "Googlebot/2.1")), http.StatusAccepted)

	// The worker's rollup (run here on the same store).
	svc := AnalyticsService(e.analytics, e.listings, e.marketStore, 90, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return e.now })
	if _, err := svc.Rollup(context.Background()); err != nil {
		t.Fatal(err)
	}

	listingPath := "/v1/analytics/listings/" + tendai.ID.String()
	orgPath := "/v1/analytics/organisations/" + devseed.SampleEstates.ID.String()

	// Signed out: 401.
	wantError(t, e.get(listingPath), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.get(orgPath), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// The assigned agent (Tendai) reads his listing: rollups for 7d.
	tendaiAs := c.as(identity.SeedAgentEmail)
	rep := decode[analyticsReportBody](t, e.get(listingPath+"?period=7d", tendaiAs))
	if rep.Listing == nil || rep.Listing.ID != tendai.ID.String() || rep.Totals.Views != 3 || rep.Totals.UniqueViews != 3 ||
		rep.Totals.ContactClicks != 1 || rep.FilteredBotEvents != 1 || rep.NearRealTime || rep.RefreshedAt == nil || len(rep.Daily) != 7 ||
		rep.CTR == nil || *rep.CTR != 0.3333 || rep.Conversion == nil || *rep.Conversion != 0 || len(rep.Sources) == 0 {
		t.Fatalf("listing report: %+v", rep)
	}
	if rec := e.get(listingPath, tendaiAs); rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("Cache-Control %q", rec.Header().Get("Cache-Control"))
	}
	if live := decode[analyticsReportBody](t, e.get(listingPath+"?period=24h", tendaiAs)); !live.NearRealTime || live.Totals.Views != 3 {
		t.Fatalf("24h: %+v", live)
	}
	wantError(t, e.get(listingPath+"?period=1y", tendaiAs), http.StatusBadRequest, httpx.CodeValidationFailed)

	// Another agent of the agency (Chipo) is not assigned to it: 404.
	chipoAs := c.as(devseed.EstatesAgent2Email)
	wantError(t, e.get(listingPath, chipoAs), http.StatusNotFound, httpx.CodeNotFound)
	// MANDATORY: another organisation's owner gets 404 for the listing and
	// for the organisation, like something that does not exist.
	other := c.as(devseed.CommercialAgentEmail)
	unknown := wantError(t, e.get("/v1/analytics/listings/"+uuid.NewString(), other), http.StatusNotFound, httpx.CodeNotFound)
	if foreignListing := wantError(t, e.get(listingPath, other), http.StatusNotFound, httpx.CodeNotFound); foreignListing.Error.Message != unknown.Error.Message {
		t.Fatal("another organisation's listing must look like an unknown one")
	}
	wantError(t, e.get(orgPath, other), http.StatusNotFound, httpx.CodeNotFound)
	wantError(t, e.get("/v1/analytics/organisations/"+uuid.NewString(), tendaiAs), http.StatusNotFound, httpx.CodeNotFound)
	wantError(t, e.get("/v1/analytics/organisations/not-a-uuid", tendaiAs), http.StatusNotFound, httpx.CodeNotFound)

	// The owner sees every listing of the agency; the agent only his own.
	owner := decode[analyticsReportBody](t, e.get(orgPath, c.as(devseed.EstatesOwnerEmail)))
	if owner.Totals.Views != 4 || owner.ListingsCount < 2 || len(owner.TopListings) != 2 || owner.TopListings[0].ListingID != tendai.ID.String() ||
		owner.TopListings[0].Views != 3 || owner.TopListings[0].Slug == nil || *owner.TopListings[0].Slug != tendai.Slug {
		t.Fatalf("owner's report: %+v", owner)
	}
	agent := decode[analyticsReportBody](t, e.get(orgPath, tendaiAs))
	if agent.Totals.Views != 3 || agent.ListingsCount >= owner.ListingsCount || len(agent.TopListings) != 1 || agent.TopListings[0].ListingID != tendai.ID.String() {
		t.Fatalf("agent's report: %+v", agent)
	}
	for _, top := range agent.TopListings {
		if top.ListingID == chipo.ID.String() {
			t.Fatal("an agent sees another agent's listing")
		}
	}

	// The administrators' summary. Each administrator signs in from its own
	// address (the admin sign-in limit is 5 a minute).
	adminN := 0
	asAdmin := func(email string) reqOpt {
		adminN++
		return browser(e.adminLogin(email, seedPassword, withIP(fmt.Sprintf("10.30.0.%d", adminN))))
	}
	summary := "/v1/admin/analytics/summary"
	wantError(t, e.get(summary), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.get(summary, e.as(identity.SeedConsumerEmail)), http.StatusForbidden, httpx.CodeForbidden)
	for _, email := range []string{devseed.SupportEmail, devseed.ModeratorEmail, devseed.FinanceAdminEmail, devseed.ContentAdminEmail} {
		wantError(t, e.get(summary, asAdmin(email)), http.StatusForbidden, httpx.CodeForbidden)
	}
	all := decode[analyticsReportBody](t, e.get(summary, asAdmin(devseed.PlatformAdminEmail)))
	if all.Totals.Views != 5 || len(all.Markets) == 0 || len(all.TopListings) != 3 || all.FilteredBotEvents != 1 {
		t.Fatalf("site summary: %+v", all)
	}
	if s := decode[analyticsReportBody](t, e.get(summary+"?market=ZW&period=24h", asAdmin(identity.SeedAdminEmail))); !s.NearRealTime || s.Totals.Views == 0 {
		t.Fatalf("super admin, ZW 24h: %+v", s)
	}
	market := asAdmin(devseed.MarketAdminEmail)
	wantStatus(t, e.get(summary+"?market=ZW", market), http.StatusOK)
	wantError(t, e.get(summary+"?market=GB", market), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get(summary+"?market=XX", market), http.StatusBadRequest, httpx.CodeValidationFailed)

	// No endpoint returns raw events: no session, network or event_id in any report.
	for _, rec := range []*httptest.ResponseRecorder{e.get(listingPath+"?period=24h", tendaiAs), e.get(summary+"?period=24h", asAdmin(devseed.PlatformAdminEmail))} {
		body := rec.Body.String()
		if strings.Contains(body, "tendaiSession") || strings.Contains(body, "event_id") || strings.Contains(body, "/24") {
			t.Fatalf("raw data in a report: %s", body)
		}
	}
}

// A relist writes LISTING_RELISTED, which the worker's recorder turns into a
// listing_relist event (once, however often it is delivered).
func TestRelistIsCountedServerSide(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	id := propertyrequests.SeedListingID(1).String()
	managedOf(t, e.post("/v1/me/listings/"+id+"/archive", nil, tendai), http.StatusOK)
	managedOf(t, e.post("/v1/me/listings/"+id+"/relist", nil, tendai), http.StatusOK)
	var relist []outbox.Event
	for _, ev := range e.listings.Outbox() {
		if ev.Type == listings.EventListingRelisted {
			relist = append(relist, ev)
		}
	}
	if len(relist) != 1 {
		t.Fatalf("%d LISTING_RELISTED events", len(relist))
	}
	svc := AnalyticsService(e.analytics, e.listings, e.marketStore, 90, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return e.now })
	rec := analytics.NewRecorder(svc, AppointmentListing(e.appts))
	for range 2 {
		if err := rec.Handle(context.Background(), relist[0]); err != nil {
			t.Fatal(err)
		}
	}
	got := e.analytics.Events()
	if len(got) != 1 || got[0].Type != analytics.TypeListingRelist || got[0].ListingID.String() != id || got[0].Origin != analytics.OriginServer ||
		got[0].ID != analytics.ServerEventID(relist[0].ID) {
		t.Fatalf("recorded: %+v", got)
	}
}
