package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/analytics"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/venues"
)

// The test clock is 2026-11-01 09:00 UTC (a Sunday). The seeded showroom
// bookings fall on Monday 9 November (the tenant's CONFIRMED 10:00-14:00
// Harare time and the consumer's overlapping REQUESTED 12:00-15:00) and
// Monday 16 November (the tenant's REQUESTED 09:00-12:00).

type venueBookingBody struct {
	ID           string   `json:"id"`
	Reference    string   `json:"reference"`
	ServiceType  string   `json:"service_type"`
	Status       string   `json:"status"`
	Actions      []string `json:"actions"`
	Notice       string   `json:"notice"`
	Mode         string   `json:"mode"`
	Cancellation *struct {
		By   string `json:"by"`
		Free bool   `json:"free"`
	} `json:"cancellation"`
	Customer *struct {
		Email string `json:"email"`
	} `json:"customer"`
	Events []struct {
		Action string `json:"action"`
		Actor  string `json:"actor"`
	} `json:"events"`
}

type venuePage struct {
	Items []venueBookingBody `json:"items"`
	Total int                `json:"total"`
}

func showroomOrg(t *testing.T) uuid.UUID {
	for _, l := range devseed.Listings() {
		if l.ID == devseed.ShowroomListingID && l.OrganisationID != nil {
			return *l.OrganisationID
		}
	}
	t.Fatal("no showroom organisation")
	return uuid.Nil
}

func hourlyPath(id uuid.UUID) string { return "/v1/me/listings/" + id.String() + "/hourly" }

func TestVenueSettingsEligibilityAndListing(t *testing.T) {
	e := newEnv(t)
	owner := e.loginCookie(showroomHost, seedPassword)
	rec := e.get(hourlyPath(devseed.ShowroomListingID), browser(owner))
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	for _, want := range []string{`"eligible":true`, `"configured":true`, `"rate_per_hour":"50.00"`, `"currency":"USD"`, `"currencies":["USD"]`,
		`"time_zone":"Africa/Harare"`, `"increment_minutes":30`, `"deposit":"100.00"`, `"code":"TAX","label":"VAT 15%","kind":"PERCENT","amount":"15"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings lack %s:\n%s", want, body)
		}
	}
	// A guest house (listing 23, same owner) is not hourly-bookable: GET says so, PUT is 400.
	guest := propertyrequests.SeedListingID(23)
	rec = e.get(hourlyPath(guest), browser(owner))
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"eligible":false`) || !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("not eligible: %s", rec.Body.String())
	}
	put := obj{"enabled": true, "currency": "USD", "rate_per_hour": "20", "increment_minutes": 60, "min_minutes": 60, "max_minutes": 120,
		"cancellation_policy": "Free", "opening_hours": []obj{{"weekday": 1, "opens": "08:00", "closes": "12:00"}}}
	wantError(t, e.do(http.MethodPut, hourlyPath(guest), put, browser(owner)), http.StatusBadRequest, venues.CodeListingNotEligible)
	// Another organisation's owner: 404.
	other := e.loginCookie(devseed.EstatesOwnerEmail, seedPassword)
	wantError(t, e.get(hourlyPath(devseed.ShowroomListingID), browser(other)), http.StatusNotFound, venues.CodeListingNotFound)
	// A wrong currency is a field error.
	put["currency"] = "EUR"
	env := wantError(t, e.do(http.MethodPut, hourlyPath(devseed.ShowroomListingID), put, browser(owner)), http.StatusBadRequest, "VALIDATION_FAILED")
	if env.Error.Fields["currency"] == "" {
		t.Fatalf("fields %v", env.Error.Fields)
	}

	// The reference data flags the type; the listing carries hourly_booking.
	if ref := e.get("/v1/reference/property_types").Body.String(); !strings.Contains(ref, `"code":"showroom","label":"Showroom"`) ||
		!strings.Contains(ref, `"hourly_bookable":true`) || !strings.Contains(ref, `"hourly_bookable":false`) {
		t.Fatalf("reference: %s", ref)
	}
	if l := e.get("/v1/listings/" + devseed.ShowroomListingSlug).Body.String(); !strings.Contains(l,
		`"hourly_booking":{"enabled":true,"rate_per_hour":"50.00","currency":"USD","increment_minutes":30}`) {
		t.Fatalf("listing: %s", l)
	}
	if l := e.get("/v1/listings/garden-guest-house-avondale").Body.String(); !strings.Contains(l, `"hourly_booking":null`) {
		t.Fatalf("guest house listing: %s", l)
	}
	if s := e.get("/v1/listings?q=showroom").Body.String(); !strings.Contains(s, `"hourly_booking":{"enabled":true`) {
		t.Fatalf("search card: %s", s)
	}
}

func TestVenueQuoteRequestAndConfirmConflict(t *testing.T) {
	e := newEnv(t)
	venue := "/v1/venues/" + devseed.ShowroomListingID.String()
	rec := e.get(venue + "/availability?from=2026-11-08&to=2026-11-10")
	wantStatus(t, rec, http.StatusOK)
	av := rec.Body.String()
	// Sunday closed; Monday booked 07:30Z-12:30Z (10:00-14:00 Harare widened by the 30-minute buffer on both sides); no customer data.
	for _, want := range []string{`"date":"2026-11-08","weekday":7,"closed":true`, `"booked":[{"start":"2026-11-09T07:30:00Z","end":"2026-11-09T12:30:00Z","local_start":"2026-11-09T09:30:00+02:00"`,
		`"notice":"` + venues.Notice + `"`} {
		if !strings.Contains(av, want) {
			t.Fatalf("availability lacks %s:\n%s", want, av)
		}
	}
	if strings.Contains(av, "tenant") || strings.Contains(av, "Tariro") {
		t.Fatal("availability exposes a customer")
	}
	wantError(t, e.get(venue+"/availability?from=2026-11-01&to=2027-02-01"), http.StatusBadRequest, "VALIDATION_FAILED")
	wantError(t, e.get("/v1/venues/"+uuid.NewString()+"/availability?from=2026-11-08&to=2026-11-09"), http.StatusNotFound, venues.CodeVenueNotFound)

	// 409 with the reason; 400 for misalignment.
	wantError(t, e.post(venue+"/quotes", obj{"start": "2026-11-09T09:00:00Z", "end": "2026-11-09T11:00:00Z"}), http.StatusConflict, venues.CodeSlotUnavailable)
	if !strings.Contains(e.post(venue+"/quotes", obj{"start": "2026-11-09T09:00:00Z", "end": "2026-11-09T11:00:00Z"}).Body.String(), `"reason":"TAKEN"`) {
		t.Fatal("no reason")
	}
	if b := e.post(venue+"/quotes", obj{"start": "2026-11-09T12:00:00Z", "end": "2026-11-09T14:00:00Z"}).Body.String(); !strings.Contains(b, `"reason":"BUFFER"`) {
		t.Fatalf("buffer: %s", b)
	}
	if b := e.post(venue+"/quotes", obj{"start": "2026-11-01T10:00:00Z", "end": "2026-11-01T12:00:00Z"}).Body.String(); !strings.Contains(b, `"reason":"LEAD_TIME"`) {
		t.Fatalf("lead time: %s", b)
	}
	wantError(t, e.post(venue+"/quotes", obj{"start": "2026-11-10T08:15:00Z", "end": "2026-11-10T10:15:00Z"}), http.StatusBadRequest, "VALIDATION_FAILED")

	rec = e.post(venue+"/quotes", obj{"start": "2026-11-10T08:00:00Z", "end": "2026-11-10T12:00:00Z"})
	wantStatus(t, rec, http.StatusCreated)
	q := rec.Body.String()
	for _, want := range []string{`"currency":"USD"`, `{"code":"RATE","kind":"RATE","label":"4 h × USD 50.00","amount":"200.00"}`,
		`{"code":"TAX","kind":"PERCENT","label":"VAT 15%","amount":"30.00"}`, `"subtotal":"200.00","deposit":"100.00","deposit_note":"payable to the host if confirmed","total_excluding_deposit":"230.00"`,
		`"local_start":"2026-11-10T10:00:00+02:00"`, `"mode":"ENQUIRY"`, `"duration_minutes":240`} {
		if !strings.Contains(q, want) {
			t.Fatalf("quote lacks %s:\n%s", want, q)
		}
	}
	quoteID := decode[struct {
		QuoteID string `json:"quote_id"`
	}](t, rec).QuoteID
	request := obj{"quote_id": quoteID, "attendees": 12, "consent": obj{"privacy_version": "2026-01", "accepted": true}}
	wantStatus(t, e.post("/v1/me/venue-bookings", request), http.StatusUnauthorized)
	consumer := e.loginCookie(identity.SeedConsumerEmail, seedPassword)
	rec = e.post("/v1/me/venue-bookings", request, browser(consumer))
	wantStatus(t, rec, http.StatusCreated)
	created := decode[venueBookingBody](t, rec)
	if created.Status != venues.StatusRequested || created.ServiceType != "VENUE_HOURLY" || created.Notice != venues.Notice || created.Customer != nil ||
		len(created.Events) != 1 || created.Events[0].Actor != "CUSTOMER" {
		t.Fatalf("created %+v", created)
	}
	wantStatus(t, e.post("/v1/me/venue-bookings", request, browser(consumer)), http.StatusOK)
	tenant := e.loginCookie(devseed.TenantEmail, seedPassword)
	wantError(t, e.post("/v1/me/venue-bookings", request, browser(tenant)), http.StatusConflict, venues.CodeQuoteUsed)

	// The host confirms the seeded overlapping request: 409 SLOT_TAKEN (the tenant's is confirmed).
	owner := e.loginCookie(showroomHost, seedPassword)
	base := "/v1/agent/organisations/" + showroomOrg(t).String() + "/venue-bookings/"
	rec = e.post(base+devseed.ConsumerOverlapBookingID.String()+"/confirm", nil, browser(owner))
	wantError(t, rec, http.StatusConflict, venues.CodeSlotTaken)
	if !strings.Contains(rec.Body.String(), `"details":{"reason":"TAKEN"}`) {
		t.Fatalf("slot taken: %s", rec.Body.String())
	}
	// The new request is confirmed; the host sees the customer's email.
	rec = e.post(base+created.ID+"/confirm", nil, browser(owner))
	wantStatus(t, rec, http.StatusOK)
	confirmed := decode[venueBookingBody](t, rec)
	if confirmed.Status != venues.StatusConfirmed || confirmed.Customer == nil || confirmed.Customer.Email != identity.SeedConsumerEmail {
		t.Fatalf("confirmed %+v", confirmed)
	}
	// The host list filters by status and date.
	page := decode[venuePage](t, e.get(base+"?status=CONFIRMED&from=2026-11-09&to=2026-11-10", browser(owner)))
	if page.Total != 2 {
		t.Fatalf("host list %+v", page)
	}
	// The customer cancels: free (more than 48 hours ahead), recorded with the actor.
	rec = e.post("/v1/me/venue-bookings/"+created.ID+"/cancel", obj{"reason": "Moved"}, browser(consumer))
	wantStatus(t, rec, http.StatusOK)
	if c := decode[venueBookingBody](t, rec); c.Cancellation == nil || !c.Cancellation.Free || c.Cancellation.By != "CUSTOMER" {
		t.Fatalf("cancel %+v", c)
	}
	// The organisation audit trail recorded the host's confirmation.
	if audit := e.get("/v1/agent/organisations/"+showroomOrg(t).String()+"/audit", browser(owner)).Body.String(); !strings.Contains(audit, "VENUE_BOOKING_CONFIRMED") {
		t.Fatalf("audit: %s", audit)
	}
}

// Every host endpoint answers 404 to another organisation, and every
// customer endpoint 404 to another customer.
func TestVenueBookingsWrongOrganisationAndCustomer(t *testing.T) {
	e := newEnv(t)
	booking := devseed.TenantConfirmedBookingID.String()
	other := e.loginCookie(devseed.EstatesOwnerEmail, seedPassword)
	quote := obj{"quote_id": uuid.NewString()}
	reason := obj{"reason": "No"}
	for _, org := range []string{showroomOrg(t).String(), devseed.SampleEstates.ID.String()} {
		base := "/v1/agent/organisations/" + org + "/venue-bookings"
		listCode := venues.CodeOrganisationUnknown
		if org == devseed.SampleEstates.ID.String() {
			// Their own organisation: an empty list, and the booking is not theirs.
			if p := decode[venuePage](t, e.get(base, browser(other))); p.Total != 0 {
				t.Fatalf("own list %+v", p)
			}
		} else {
			wantError(t, e.get(base, browser(other)), http.StatusNotFound, listCode)
		}
		wantError(t, e.get(base+"/"+booking, browser(other)), http.StatusNotFound, venues.CodeBookingNotFound)
		for action, body := range map[string]any{"confirm": nil, "decline": reason, "cancel": reason, "propose": quote, "complete": nil, "no-show": nil} {
			wantError(t, e.post(base+"/"+booking+"/"+action, body, browser(other)), http.StatusNotFound, venues.CodeBookingNotFound)
		}
	}
	consumer := e.loginCookie(identity.SeedConsumerEmail, seedPassword)
	mine := "/v1/me/venue-bookings/" + booking
	wantError(t, e.get(mine, browser(consumer)), http.StatusNotFound, venues.CodeBookingNotFound)
	for action, body := range map[string]any{"cancel": obj{}, "reschedule": quote, "accept": nil} {
		wantError(t, e.post(mine+"/"+action, body, browser(consumer)), http.StatusNotFound, venues.CodeBookingNotFound)
	}
	// The tenant sees their own two; the consumer only theirs.
	tenant := e.loginCookie(devseed.TenantEmail, seedPassword)
	if p := decode[venuePage](t, e.get("/v1/me/venue-bookings", browser(tenant))); p.Total != 2 {
		t.Fatalf("tenant %+v", p)
	}
	if p := decode[venuePage](t, e.get("/v1/me/venue-bookings", browser(consumer))); p.Total != 1 {
		t.Fatalf("consumer %+v", p)
	}
	// Not a direct owner: the hosted list is 403.
	wantError(t, e.get("/v1/me/hosted-venue-bookings", browser(consumer)), http.StatusForbidden, "FORBIDDEN")
}

// FR-022: hourly bookings are their own service type and never appear in
// the appointment lists or counts.
func TestVenueBookingsAreNotAppointments(t *testing.T) {
	e := newEnv(t)
	tenant := e.loginCookie(devseed.TenantEmail, seedPassword)
	owner := e.loginCookie(showroomHost, seedPassword)
	if p := decode[venuePage](t, e.get("/v1/me/venue-bookings", browser(tenant))); p.Total != 2 || p.Items[0].ServiceType != venues.ServiceType {
		t.Fatalf("venue bookings %+v", p)
	}
	for _, c := range []struct {
		path   string
		cookie any
	}{{"/v1/me/appointments", tenant}, {"/v1/inbox/appointments", owner}} {
		rec := e.get(c.path, browser(c.cookie.(*http.Cookie)))
		wantStatus(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), `"total":0`) || strings.Contains(rec.Body.String(), "VB-") {
			t.Fatalf("%s counts hourly bookings: %s", c.path, rec.Body.String())
		}
	}
	if s := e.get("/v1/inbox/summary", browser(owner)).Body.String(); strings.Contains(s, `"requested":1`) || strings.Contains(s, `"requested":2`) {
		t.Fatalf("summary counts hourly bookings: %s", s)
	}
	if len(e.appts.All()) != 0 {
		t.Fatal("an hourly booking became an appointment")
	}

	// Confirming one is analytics' booking_confirm, never an appointment_confirm.
	path := "/v1/agent/organisations/" + showroomOrg(t).String() + "/venue-bookings/" + devseed.TenantRequestedBookingID.String() + "/confirm"
	wantStatus(t, e.post(path, nil, browser(owner)), http.StatusOK)
	events := e.venues.Outbox()
	if len(events) != 1 || events[0].Type != venues.EventConfirmed {
		t.Fatalf("outbox %+v", events)
	}
	svc := AnalyticsService(e.analytics, e.listings, e.marketStore, 90, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return e.now })
	if err := analytics.NewRecorder(svc, AppointmentListing(e.appts)).Handle(context.Background(), events[0]); err != nil {
		t.Fatal(err)
	}
	if got := e.analytics.Events(); len(got) != 1 || got[0].Type != analytics.TypeBookingConfirm || got[0].ListingID != devseed.ShowroomListingID {
		t.Fatalf("analytics %+v", got)
	}
	if !strings.Contains(string(events[0].Payload), `"service_type":"VENUE_HOURLY"`) {
		t.Fatalf("payload %s", events[0].Payload)
	}
}

// Deleting an account removes the customer's hourly bookings (the users row is
// anonymised, not deleted, so no foreign-key cascade does it).
func TestVenueBookingsGoWithTheAccount(t *testing.T) {
	e := newEnv(t)
	tenant := devseed.TenantID
	count := func() int {
		_, n, err := e.venues.List(t.Context(), venues.Filter{CustomerUserID: &tenant, Page: 1, PageSize: 50})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n == 0 {
		t.Fatal("the tenant has no seeded venue bookings")
	}
	cookie := e.loginCookie(devseed.TenantEmail, seedPassword)
	wantStatus(t, e.del("/v1/me", obj{"password": seedPassword}, browser(cookie)), http.StatusOK)
	if n := count(); n != 0 {
		t.Fatalf("%d venue bookings left after the account was deleted", n)
	}
}
