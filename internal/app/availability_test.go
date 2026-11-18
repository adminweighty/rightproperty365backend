package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/availability"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// The test clock is 2026-11-01 09:00 UTC: "today" is 2026-11-01 in Harare,
// London and Mauritius, and the booking window ends on 2027-11-01.
const (
	fallsHost    = "sample-falls-stays@example.test" // OWNER of the host that lists 22 (Victoria Falls) and 24 (Mauritius)
	showroomHost = "sample-showrooms@example.test"   // OWNER of another host, which lists 23
	fallsSlug    = "safari-lodge-victoria-falls"
)

func listingPath(n int) string {
	return "/v1/me/listings/" + propertyrequests.SeedListingID(n).String()
}

type calendarBody struct {
	Listing struct {
		ID, Slug, Title, Reference string
	} `json:"listing"`
	Timezone string `json:"timezone"`
	Window   struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"window"`
	MinNights int    `json:"min_nights"`
	MaxGuests *int   `json:"max_guests"`
	From      string `json:"from"`
	To        string `json:"to"`
	Days      []struct {
		Date      string  `json:"date"`
		Available bool    `json:"available"`
		Reason    *string `json:"reason"`
		Note      *string `json:"note"`
	} `json:"days"`
	Ranges []struct {
		ID         string  `json:"id"`
		FirstNight string  `json:"first_night"`
		LastNight  string  `json:"last_night"`
		Reason     string  `json:"reason"`
		Note       *string `json:"note"`
		UpdatedAt  string  `json:"updated_at"`
	} `json:"ranges"`
	AvailabilityUpdatedAt *string `json:"availability_updated_at"`
}

func (c calendarBody) ranges() string {
	var out []string
	for _, r := range c.Ranges {
		s := r.FirstNight + ".." + r.LastNight + " " + r.Reason
		if r.Note != nil {
			s += "(" + *r.Note + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

func change(from, to, status string, note ...string) obj {
	c := obj{"from": from, "to": to, "status": status}
	if len(note) > 0 {
		c["note"] = note[0]
	}
	return c
}

// setCalendar applies changes as a lister and returns the calendar.
func (c *cast) setCalendar(email string, n int, changes ...obj) calendarBody {
	c.e.t.Helper()
	rec := c.e.put(listingPath(n)+"/availability", obj{"changes": changes}, c.as(email))
	wantStatus(c.e.t, rec, http.StatusOK)
	return decode[calendarBody](c.e.t, rec)
}

// member creates an account and makes it a member of an organisation.
func (c *cast) member(email string, organisationID uuid.UUID, role string) string {
	c.e.t.Helper()
	cookie := c.e.selfRegister(email, "CONSUMER")
	user, _ := c.e.me(cookie)
	if err := c.e.identity.AddMembership(uuid.MustParse(user.ID), organisationID, devseed.MarketCode, role); err != nil {
		c.e.t.Fatal(err)
	}
	c.tokens[email] = c.e.loginBearer(email, registerBody(email)["password"].(string))
	return email
}

func organisationOf(t *testing.T, email string) uuid.UUID {
	t.Helper()
	for _, a := range devseed.Accounts() {
		if a.Email == email && a.Organisation != nil {
			return a.Organisation.ID
		}
	}
	t.Fatalf("no organisation for %s", email)
	return uuid.Nil
}

type myListingsBody struct {
	Items []struct {
		ID                    string           `json:"id"`
		Slug                  string           `json:"slug"`
		Reference             string           `json:"reference"`
		Title                 string           `json:"title"`
		Category              listings.Coded   `json:"category"`
		PropertyType          listings.Coded   `json:"property_type"`
		Status                string           `json:"status"`
		Price                 listings.Money   `json:"price"`
		PricePeriod           *string          `json:"price_period"`
		City                  listings.Place   `json:"city"`
		Country               listings.Country `json:"country"`
		MaxGuests             *int             `json:"max_guests"`
		MinNights             *int             `json:"min_nights"`
		AvailabilityUpdatedAt *string          `json:"availability_updated_at"`
		Photo                 *listings.Photo  `json:"photo"`
		CanEdit               bool             `json:"can_edit"`
	} `json:"items"`
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

func (b myListingsBody) slugs() string {
	var out []string
	for _, it := range b.Items {
		out = append(out, it.Slug)
	}
	return strings.Join(out, ",")
}

// Who may manage a calendar: the capability table, nothing else.
func TestAvailabilityAuthorisation(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	host := organisationOf(t, fallsHost)
	manager := c.member("falls.manager@example.com", host, auth.RoleManager)
	agent := c.member("falls.agent@example.com", host, auth.RoleAgent)
	staff := c.member("falls.staff@example.com", host, auth.RoleStaff)

	// Not signed in.
	wantError(t, e.get("/v1/me/listings"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.get(listingPath(22)+"/availability"), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// GET /v1/me/listings: the listings the caller may manage.
	list := func(email, query string) myListingsBody {
		t.Helper()
		rec := e.get("/v1/me/listings"+query, c.as(email))
		wantStatus(t, rec, http.StatusOK)
		return decode[myListingsBody](t, rec)
	}
	for _, email := range []string{fallsHost, manager, agent} {
		got := list(email, "")
		if got.Total != 2 || got.slugs() != "safari-lodge-victoria-falls,beach-villa-grand-baie" || got.Page != 1 || got.TotalPages != 1 {
			t.Fatalf("%s manages %s", email, got.slugs())
		}
	}
	falls := list(fallsHost, "?category=short-stay&status=PUBLISHED&page_size=1")
	if falls.Total != 2 || len(falls.Items) != 1 || falls.TotalPages != 2 {
		t.Fatalf("paging: %+v", falls)
	}
	it := falls.Items[0]
	if it.ID != propertyrequests.SeedListingID(22).String() || it.Reference != "RP-ZW-10019" || it.Category.Code != "short-stay" || it.PropertyType.Code != "lodge" ||
		it.Status != "PUBLISHED" || it.Price != (listings.Money{Amount: "320.00", Currency: "USD"}) || it.PricePeriod == nil || *it.PricePeriod != "night" ||
		!it.City.Is("victoria-falls") || it.Country.Code != "ZW" || *it.MaxGuests != 8 || *it.MinNights != 2 || it.AvailabilityUpdatedAt != nil || it.Photo == nil {
		t.Fatalf("summary = %+v", it)
	}
	if got := list(fallsHost, "?category=residential"); got.Total != 0 || len(got.Items) != 0 {
		t.Fatalf("category=residential: %d", got.Total)
	}
	// Every listing of Sample Estates, not only the three assigned to him,
	// and whatever its status: the expired one, the sample draft and the
	// sample listing waiting for review are there too.
	if got := list("agent@example.test", ""); got.Total != 9 {
		t.Fatalf("an agency's agent manages %d listings: %s", got.Total, got.slugs())
	}
	// ...but may edit (and draft leases for) only the ones assigned to him;
	// the organisation's manager may edit every one.
	editable := func(email string) int {
		n := 0
		for _, item := range list(email, "").Items {
			if item.CanEdit {
				n++
			}
		}
		return n
	}
	if n := editable("agent@example.test"); n == 0 || n >= 9 {
		t.Fatalf("the agent may edit %d of Sample Estates' 9 listings", n)
	}
	if n := editable("agency.admin@example.test"); n != 9 {
		t.Fatalf("the manager may edit %d of 9 listings", n)
	}
	if got := list("agent@example.test", "?status=EXPIRED"); got.Total != 1 || got.Items[0].Slug != devseed.ExpiredListingSlug || got.Items[0].Status != "EXPIRED" {
		t.Fatalf("status=EXPIRED: %s", got.slugs())
	}
	if got := list("agent@example.test", "?category=short-stay"); got.Total != 0 {
		t.Fatalf("Sample Estates has %d short stays", got.Total)
	}
	// The direct owner sees their own listing.
	if got := list("seller@example.test", ""); got.Total != 1 || got.Items[0].ID != propertyrequests.SeedListingID(6).String() {
		t.Fatalf("the direct owner manages %s", got.slugs())
	}
	// 403 for a caller who manages no listings at all: a consumer, STAFF
	// (who support the others) and a member of a service provider.
	for _, email := range []string{"consumer@example.test", staff, "sample-plumbers@example.test"} {
		wantError(t, e.get("/v1/me/listings", c.as(email)), http.StatusForbidden, httpx.CodeForbidden)
	}
	wantError(t, e.get("/v1/me/listings?page=0", c.as(fallsHost)), http.StatusBadRequest, httpx.CodeValidationFailed)

	// One calendar: OWNER, MANAGER and AGENT of the owning organisation.
	body := obj{"changes": []obj{change("2026-12-01", "2026-12-02", "BLOCKED")}}
	for _, email := range []string{fallsHost, manager, agent} {
		wantStatus(t, e.get(listingPath(22)+"/availability", c.as(email)), http.StatusOK)
		wantStatus(t, e.put(listingPath(22)+"/availability", body, c.as(email)), http.StatusOK)
		wantStatus(t, e.do(http.MethodPatch, listingPath(22)+"/stay-rules", obj{"min_nights": 2}, c.as(email)), http.StatusOK)
	}
	// Everybody else gets 404, as if the listing did not exist: STAFF of the
	// same organisation, another host, another organisation's agent, a consumer.
	for _, email := range []string{staff, showroomHost, "agent-b@example.test", "agent@example.test", "consumer@example.test", "seller@example.test"} {
		for _, rec := range []*httptest.ResponseRecorder{
			e.get(listingPath(22)+"/availability", c.as(email)),
			e.put(listingPath(22)+"/availability", body, c.as(email)),
			e.do(http.MethodPatch, listingPath(22)+"/stay-rules", obj{"min_nights": 1}, c.as(email)),
		} {
			wantError(t, rec, http.StatusNotFound, listings.CodeListingNotFound)
		}
	}
	wantError(t, e.get("/v1/me/listings/not-a-uuid/availability", c.as(fallsHost)), http.StatusNotFound, listings.CodeListingNotFound)
	wantError(t, e.get("/v1/me/listings/"+uuid.NewString()+"/availability", c.as(fallsHost)), http.StatusNotFound, listings.CodeListingNotFound)
}

// The direct owner of a short stay manages its calendar; a listing that is
// not a short stay has none.
func TestAvailabilityDirectOwnerAndOtherCategories(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	// Grace (an ACTIVE property seller) lets a cottage by the night herself.
	two := 2
	seller := devseed.SellerID
	cottage := listings.Record{
		ID: propertyrequests.SeedListingID(60), Slug: "cottage-by-the-night", Reference: "RP-ZW-10060", Title: "Cottage by the night",
		Deal: "rent", Category: listings.CategoryShortStay, PropertyType: "guest-house", Price: listings.Money{Amount: "60.00", Currency: "USD"},
		PricePeriod: "night", MaxGuests: &two, City: "harare", Suburb: "avondale", MarketCode: "ZW", Status: listings.StatusPublished,
		PublishedAt: e.now, ExpiresAt: e.now.AddDate(0, 0, 90), UpdatedAt: e.now, SellerUserID: &seller,
		Advertiser: listings.Advertiser{Type: listings.AdvertiserPrivateSeller, Name: "Grace Mhlanga"},
	}
	e.listings.Put(cottage)
	got := c.setCalendar("seller@example.test", 60, change("2026-11-10", "2026-11-12", "BOOKED", "Family"))
	if got.ranges() != "2026-11-10..2026-11-12 BOOKED(Family)" || got.MinNights != 1 || got.Listing.Slug != "cottage-by-the-night" {
		t.Fatalf("the direct owner's calendar: %+v", got)
	}
	// Not hers: 404. Hers but not a short stay (listing 6 is a house for sale): 409.
	wantError(t, e.get(listingPath(22)+"/availability", c.as("seller@example.test")), http.StatusNotFound, listings.CodeListingNotFound)
	for _, rec := range []*httptest.ResponseRecorder{
		e.get(listingPath(6)+"/availability", c.as("seller@example.test")),
		e.put(listingPath(6)+"/availability", obj{"changes": []obj{change("2026-12-01", "2026-12-02", "BLOCKED")}}, c.as("seller@example.test")),
		e.do(http.MethodPatch, listingPath(6)+"/stay-rules", obj{"min_nights": 2}, c.as("seller@example.test")),
		e.get(listingPath(1)+"/availability", c.as("agent@example.test")),
		e.get("/v1/listings/" + devseed.Sample().Listings[0].Slug + "/availability"),
	} {
		wantError(t, rec, http.StatusConflict, availability.CodeNotShortStay)
	}
	// The public endpoint answers like the listing: 404 unknown, 410 expired.
	wantError(t, e.get("/v1/listings/no-such-listing/availability"), http.StatusNotFound, listings.CodeListingNotFound)
	wantError(t, e.get("/v1/listings/"+devseed.ExpiredListingSlug+"/availability"), http.StatusGone, listings.CodeListingUnavailable)
}

// The lister's calendar: reading, changing (in order, atomically), the
// audit trail, and validation.
func TestAvailabilityCalendar(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	read := func(query string) calendarBody {
		t.Helper()
		rec := e.get(listingPath(22)+"/availability"+query, c.as(fallsHost))
		wantStatus(t, rec, http.StatusOK)
		return decode[calendarBody](t, rec)
	}
	empty := read("")
	if empty.Listing.Slug != fallsSlug || empty.Listing.Reference != "RP-ZW-10019" || empty.Timezone != "Africa/Harare" ||
		empty.Window.From != "2026-11-01" || empty.Window.To != "2027-11-01" || empty.MinNights != 2 || *empty.MaxGuests != 8 ||
		empty.From != "2026-11-01" || empty.To != "2027-01-30" || len(empty.Days) != 91 || len(empty.Ranges) != 0 || empty.AvailabilityUpdatedAt != nil {
		t.Fatalf("empty calendar: %+v", empty)
	}
	for _, day := range empty.Days {
		if !day.Available || day.Reason != nil || day.Note != nil {
			t.Fatalf("an untouched night: %+v", day)
		}
	}
	// Block a range, book another, re-open part of the first: one request, in order.
	got := c.setCalendar(fallsHost, 22,
		change("2026-11-10", "2026-11-20", "BLOCKED", "Owner away"),
		change("2026-12-24", "2026-12-30", "BOOKED", "The Moyos"),
		change("2026-11-13", "2026-11-15", "AVAILABLE"))
	if got.ranges() != "2026-11-10..2026-11-12 BLOCKED(Owner away); 2026-11-16..2026-11-20 BLOCKED(Owner away); 2026-12-24..2026-12-30 BOOKED(The Moyos)" {
		t.Fatalf("ranges = %s", got.ranges())
	}
	if got.AvailabilityUpdatedAt == nil || *got.AvailabilityUpdatedAt != "2026-11-01T09:00:00Z" || got.From != "2026-11-01" || len(got.Days) != 91 {
		t.Fatalf("after the change: %+v", got.AvailabilityUpdatedAt)
	}
	byDate := map[string]int{}
	for i, day := range got.Days {
		byDate[day.Date] = i
	}
	for date, want := range map[string][2]string{
		"2026-11-09": {"", ""}, "2026-11-10": {"BLOCKED", "Owner away"}, "2026-11-13": {"", ""}, "2026-11-15": {"", ""},
		"2026-11-16": {"BLOCKED", "Owner away"}, "2026-12-24": {"BOOKED", "The Moyos"}, "2026-12-31": {"", ""},
	} {
		day := got.Days[byDate[date]]
		reason, note := "", ""
		if day.Reason != nil {
			reason = *day.Reason
		}
		if day.Note != nil {
			note = *day.Note
		}
		if [2]string{reason, note} != want || day.Available != (want[0] == "") {
			t.Fatalf("%s = %+v", date, day)
		}
	}
	// A span: from / to, nights before today are never available.
	past := read("?from=2026-10-30&to=2026-11-02")
	if len(past.Days) != 4 || past.Days[0].Available || past.Days[1].Available || !past.Days[2].Available || past.Days[0].Reason != nil {
		t.Fatalf("past nights: %+v", past.Days)
	}
	edge := read("?from=2027-10-31&to=2027-11-03")
	if !edge.Days[0].Available || !edge.Days[1].Available || edge.Days[2].Available || edge.Days[3].Available {
		t.Fatalf("the end of the window: %+v", edge.Days)
	}
	for _, bad := range []string{"?from=2026-11-01&to=2027-11-02", "?from=2026-12-01&to=2026-11-30", "?from=soon", "?to=2026-02-30"} {
		wantError(t, e.get(listingPath(22)+"/availability"+bad, c.as(fallsHost)), http.StatusBadRequest, httpx.CodeValidationFailed)
	}

	// The audit trail: who changed which nights to what, in order.
	audits := e.calendars.Audits()
	if len(audits) != 3 || audits[0].Status != "BLOCKED" || audits[0].From.String() != "2026-11-10" || audits[0].To.String() != "2026-11-20" ||
		audits[2].Status != "AVAILABLE" || audits[1].Note != "The Moyos" || audits[0].ListingID != propertyrequests.SeedListingID(22) {
		t.Fatalf("audit trail: %+v", audits)
	}
	owner, _ := e.me(e.loginCookie(fallsHost, seedPassword))
	for _, a := range audits {
		if a.ActorID.String() != owner.ID || !a.At.Equal(e.now) {
			t.Fatalf("audit entry: %+v", a)
		}
	}

	// Validation: field paths name the change; one bad change applies none.
	before := read("").ranges()
	for name, tt := range map[string]struct {
		body  any
		field string
	}{
		"in the past":       {obj{"changes": []obj{change("2026-12-01", "2026-12-02", "BLOCKED"), change("2026-10-31", "2026-11-02", "BLOCKED")}}, "changes[1].from"},
		"beyond the window": {obj{"changes": []obj{change("2027-10-30", "2027-11-02", "BOOKED")}}, "changes[0].to"},
		"to before from":    {obj{"changes": []obj{change("2026-12-05", "2026-12-01", "BOOKED")}}, "changes[0].to"},
		"a bad status":      {obj{"changes": []obj{change("2026-12-01", "2026-12-02", "HELD")}}, "changes[0].status"},
		"a long note":       {obj{"changes": []obj{change("2026-12-01", "2026-12-02", "BOOKED", strings.Repeat("a", 201))}}, "changes[0].note"},
		"no changes":        {obj{"changes": []obj{}}, "changes"},
		"51 changes":        {obj{"changes": make([]obj, 51)}, "changes"},
	} {
		env := wantError(t, e.put(listingPath(22)+"/availability", tt.body, c.as(fallsHost)), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields[tt.field] == "" {
			t.Fatalf("%s: fields = %v, want %s", name, env.Error.Fields, tt.field)
		}
	}
	// Strict JSON, like everywhere.
	wantStatus(t, e.put(listingPath(22)+"/availability", obj{"changes": []obj{change("2026-12-01", "2026-12-02", "BLOCKED")}, "extra": 1}, c.as(fallsHost)), http.StatusBadRequest)
	if after := read("").ranges(); after != before || len(e.calendars.Audits()) != 3 {
		t.Fatalf("a refused change was applied: %s", after)
	}

	// Stay rules.
	patch := func(body obj) *httptest.ResponseRecorder {
		return e.do(http.MethodPatch, listingPath(22)+"/stay-rules", body, c.as(fallsHost))
	}
	rec := patch(obj{"min_nights": 3, "max_guests": 10})
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"min_nights":3`) || !strings.Contains(body, `"max_guests":10`) || !strings.Contains(body, `"listing":{"id":"`+propertyrequests.SeedListingID(22).String()) {
		t.Fatalf("stay rules: %s", body)
	}
	wantStatus(t, patch(obj{"max_guests": 6}), http.StatusOK)
	if got := read(""); got.MinNights != 3 || *got.MaxGuests != 6 {
		t.Fatalf("after the stay rules: %d nights, %d guests", got.MinNights, *got.MaxGuests)
	}
	for field, body := range map[string]obj{"min_nights": {"min_nights": 0}, "max_guests": {"max_guests": 51}} {
		env := wantError(t, patch(body), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields[field] == "" {
			t.Fatalf("%v: %v", body, env.Error.Fields)
		}
	}
	wantError(t, patch(obj{}), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, patch(obj{"min_nights": 366}), http.StatusBadRequest, httpx.CodeValidationFailed)

	// The CSRF rule: a cookie session needs the web app's Origin on PUT and PATCH.
	cookie := e.loginCookie(fallsHost, seedPassword)
	one := obj{"changes": []obj{change("2026-12-01", "2026-12-02", "BLOCKED")}}
	wantError(t, e.put(listingPath(22)+"/availability", one, withCookie(cookie)), http.StatusForbidden, "CSRF_ORIGIN")
	wantError(t, e.do(http.MethodPatch, listingPath(22)+"/stay-rules", obj{"min_nights": 2}, withCookie(cookie)), http.StatusForbidden, "CSRF_ORIGIN")
	wantStatus(t, e.put(listingPath(22)+"/availability", one, browser(cookie)), http.StatusOK)
}

// The public sees available / unavailable per night and nothing about why:
// not on the calendar, not on the listing, not in search.
func TestPublicAvailabilityHidesReasonsAndNotes(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	const secret = "Private note: the Moyo family, deposit paid"
	c.setCalendar(fallsHost, 22, change("2026-11-01", "2026-11-05", "BOOKED", secret), change("2026-12-24", "2026-12-26", "BLOCKED", "Owner's sabbatical"))
	owner, _ := e.me(e.loginCookie(fallsHost, seedPassword))

	rec := e.get("/v1/listings/" + fallsSlug + "/availability")
	wantStatus(t, rec, http.StatusOK)
	var body struct {
		Listing               map[string]any   `json:"listing"`
		Timezone              string           `json:"timezone"`
		MinNights             int              `json:"min_nights"`
		MaxGuests             *int             `json:"max_guests"`
		From                  string           `json:"from"`
		To                    string           `json:"to"`
		Days                  []map[string]any `json:"days"`
		AvailabilityUpdatedAt *string          `json:"availability_updated_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Listing) != 1 || body.Listing["slug"] != fallsSlug || body.Timezone != "Africa/Harare" || body.MinNights != 2 || *body.MaxGuests != 8 ||
		body.From != "2026-11-01" || body.To != "2027-01-30" || len(body.Days) != 91 || body.AvailabilityUpdatedAt == nil {
		t.Fatalf("public calendar: %s", rec.Body.String())
	}
	for i, day := range body.Days {
		if len(day) != 2 || day["date"] == nil {
			t.Fatalf("a public day has %d keys: %v", len(day), day)
		}
		date := day["date"].(string)
		taken := (date >= "2026-11-01" && date <= "2026-11-05") || (date >= "2026-12-24" && date <= "2026-12-26")
		if day["available"] != !taken {
			t.Fatalf("day %d (%s) available = %v", i, date, day["available"])
		}
	}
	// Cacheable, and the same whoever asks.
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=30, stale-while-revalidate=120" || rec.Header().Get("ETag") == "" {
		t.Fatalf("cache headers: %v", rec.Header())
	}
	signedIn := e.get("/v1/listings/"+fallsSlug+"/availability", c.as(fallsHost))
	if signedIn.Body.String() != rec.Body.String() || strings.Contains(strings.ToLower(strings.Join(signedIn.Header().Values("Vary"), ",")), "cookie") {
		t.Fatal("the public calendar varies by caller")
	}
	if span := e.get("/v1/listings/" + fallsSlug + "/availability?from=2026-12-20&to=2026-12-27"); !strings.Contains(span.Body.String(),
		`"days":[{"date":"2026-12-20","available":true},{"date":"2026-12-21","available":true},{"date":"2026-12-22","available":true},{"date":"2026-12-23","available":true},{"date":"2026-12-24","available":false},{"date":"2026-12-25","available":false},{"date":"2026-12-26","available":false},{"date":"2026-12-27","available":true}]`) {
		t.Fatalf("span: %s", span.Body.String())
	}
	wantError(t, e.get("/v1/listings/"+fallsSlug+"/availability?from=2026-11-01&to=2027-11-02"), http.StatusBadRequest, httpx.CodeValidationFailed)

	// The listing says when the calendar changed and the first free night: 6 November.
	listing := e.get("/v1/listings/" + fallsSlug).Body.String()
	if !strings.Contains(listing, `"availability":{"updated_at":"2026-11-01T09:00:00Z","next_available":"2026-11-06"}`) {
		t.Fatalf("listing availability: %s", listing)
	}
	// A short stay nobody has touched: no change date, free from today.
	if other := e.get("/v1/listings/beach-villa-grand-baie").Body.String(); !strings.Contains(other, `"availability":{"updated_at":null,"next_available":"2026-11-01"}`) {
		t.Fatalf("untouched short stay: %s", other)
	}
	// Not a short stay: null.
	if house := e.get("/v1/listings/" + devseed.Sample().Listings[0].Slug).Body.String(); !strings.Contains(house, `"availability":null`) {
		t.Fatalf("a house for sale: %s", house)
	}
	// A fully blocked window has no next available night.
	c.setCalendar(showroomHost, 23, change("2026-11-01", "2027-11-01", "BLOCKED"))
	if full := e.get("/v1/listings/garden-guest-house-avondale").Body.String(); !strings.Contains(full, `"next_available":null`) {
		t.Fatalf("fully blocked: %s", full)
	}

	// Nothing private anywhere public.
	for _, path := range []string{
		"/v1/listings/" + fallsSlug + "/availability", "/v1/listings/" + fallsSlug + "/availability?from=2026-11-01&to=2027-11-01",
		"/v1/listings/" + fallsSlug, "/v1/listings?category=short-stay", "/v1/listings?check_in=2026-11-10&check_out=2026-11-12",
		"/v1/directory/hosts/sample-falls-stays", "/v1/sitemap",
	} {
		rec := e.get(path)
		wantStatus(t, rec, http.StatusOK)
		public := rec.Body.String()
		for _, private := range []string{secret, "Moyo", "deposit", "sabbatical", "BOOKED", "BLOCKED", `"reason"`, `"note"`, `"ranges"`, `"updated_by"`, owner.ID, "first_night"} {
			if strings.Contains(public, private) {
				t.Fatalf("%s contains %q", path, private)
			}
		}
	}
}

// Search by stay: only short stays that are free for every night of it and
// whose shortest stay fits; facet counts follow.
func TestSearchByStay(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	// Victoria Falls (22: 8 guests, 2 nights minimum) is booked 10-16 November
	// and blocked on the weekend of 5-6 December.
	c.setCalendar(fallsHost, 22, change("2026-11-10", "2026-11-16", "BOOKED"), change("2026-12-05", "2026-12-06", "BLOCKED"))
	const falls, harare, london, mauritius = "safari-lodge-victoria-falls", "garden-guest-house-avondale", "serviced-apartment-shoreditch", "beach-villa-grand-baie"
	has := func(page listingPage, slug string) bool {
		for _, l := range page.Items {
			if l.Slug == slug {
				return true
			}
		}
		return false
	}
	// Three nights that are free everywhere: all four short stays (min_nights 2, 1, 2, 3).
	free := e.search("?check_in=2026-11-20&check_out=2026-11-23")
	if free.Total != 4 || !has(free, falls) || !has(free, mauritius) {
		t.Fatalf("free dates: %d %s", free.Total, slugs(free.Items))
	}
	for _, l := range free.Items {
		if l.Category.Code != "short-stay" {
			t.Fatalf("a stay search returned %s (%s)", l.Slug, l.Category.Code)
		}
	}
	// The category is implied; naming it is the same search.
	if named := e.search("?check_in=2026-11-20&check_out=2026-11-23&category=short-stay"); named.Total != 4 {
		t.Fatalf("category=short-stay: %d", named.Total)
	}
	// One of the nights is booked: the lodge is out, also when only the last night touches.
	for _, q := range []string{"?check_in=2026-11-12&check_out=2026-11-15", "?check_in=2026-11-07&check_out=2026-11-11", "?check_in=2026-11-16&check_out=2026-11-19", "?check_in=2026-12-04&check_out=2026-12-07"} {
		got := e.search(q)
		if got.Total != 3 || has(got, falls) || !has(got, harare) || !has(got, london) || !has(got, mauritius) {
			t.Fatalf("%s: %d %s", q, got.Total, slugs(got.Items))
		}
		// The facets are counted with the exclusion applied.
		if facet(got.Facets.Country) != "MU=1,GB=1,ZW=1" || facet(got.Facets.City) != "harare=1,london=1" || facet(got.Facets.PropertyType) != "guest-house=1,holiday-home=1,serviced-apartment=1" {
			t.Fatalf("%s facets: country %s city %s type %s", q, facet(got.Facets.Country), facet(got.Facets.City), facet(got.Facets.PropertyType))
		}
	}
	// Check-out day is not a night: leaving on the 10th, or arriving on the 17th, is fine.
	for _, q := range []string{"?check_in=2026-11-07&check_out=2026-11-10", "?check_in=2026-11-17&check_out=2026-11-20"} {
		if got := e.search(q); got.Total != 4 || !has(got, falls) {
			t.Fatalf("%s: %d", q, got.Total)
		}
	}
	// min_nights: one night suits only the guest house (minimum 1); two nights not the villa (minimum 3).
	if got := e.search("?check_in=2026-11-20&check_out=2026-11-21"); got.Total != 1 || !has(got, harare) {
		t.Fatalf("one night: %s", slugs(got.Items))
	}
	if got := e.search("?check_in=2026-11-20&check_out=2026-11-22"); got.Total != 3 || has(got, mauritius) {
		t.Fatalf("two nights: %s", slugs(got.Items))
	}
	// With guests_min and a country.
	if got := e.search("?check_in=2026-11-20&check_out=2026-11-23&guests_min=5"); got.Total != 2 || !has(got, falls) || !has(got, mauritius) {
		t.Fatalf("guests_min=5: %s", slugs(got.Items))
	}
	if got := e.search("?check_in=2026-11-12&check_out=2026-11-15&country=ZW"); got.Total != 1 || !has(got, harare) || got.Facets.Price.Currency != "USD" {
		t.Fatalf("country=ZW: %s", slugs(got.Items))
	}
	// A price filter needs one currency here too.
	wantError(t, e.get("/v1/listings?check_in=2026-11-20&check_out=2026-11-23&max_price=500"), http.StatusBadRequest, httpx.CodeValidationFailed)
	// Re-opening the nights brings the lodge back.
	c.setCalendar(fallsHost, 22, change("2026-11-10", "2026-11-16", "AVAILABLE"))
	if got := e.search("?check_in=2026-11-12&check_out=2026-11-15"); got.Total != 4 || !has(got, falls) {
		t.Fatalf("after re-opening: %s", slugs(got.Items))
	}

	for q, field := range map[string]string{
		"?check_in=2026-11-20":                                           "check_out",
		"?check_out=2026-11-23":                                          "check_in",
		"?check_in=2026-11-23&check_out=2026-11-20":                      "check_out",
		"?check_in=2026-11-20&check_out=2026-11-20":                      "check_out",
		"?check_in=2026-11-20&check_out=2027-02-19":                      "check_out", // 91 nights
		"?check_in=2026-10-30&check_out=2026-11-02":                      "check_in",  // in the past everywhere
		"?check_in=2027-10-30&check_out=2027-11-05":                      "check_out", // beyond the window everywhere
		"?check_in=20-11-2026&check_out=2026-11-23":                      "check_in",
		"?check_in=2026-11-20&check_out=2026-11-23&category=residential": "category",
		"?check_in=2026-11-20&check_out=2026-11-23&category=nonexistent": "category",
	} {
		env := wantError(t, e.get("/v1/listings"+q), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields[field] == "" {
			t.Fatalf("%s: fields = %v, want %s", q, env.Error.Fields, field)
		}
	}
	// Exactly 90 nights is allowed.
	wantStatus(t, e.get("/v1/listings?check_in=2026-11-20&check_out=2027-02-18"), http.StatusOK)
}

// "Today" differs by time zone: on the last evening (UTC) of a day, a stay
// starting that day is already in the past in countries ahead of UTC.
func TestStaySearchAtTheEdgeOfTheDay(t *testing.T) {
	e := newEnv(t)
	e.now = time.Date(2026, 11, 1, 22, 30, 0, 0, time.UTC) // 2 November in Harare and Mauritius, still 1 November in London
	got := e.search("?check_in=2026-11-01&check_out=2026-11-04")
	if got.Total != 1 || got.Items[0].Slug != "serviced-apartment-shoreditch" {
		t.Fatalf("a stay from 1 November at 22:30 UTC: %s", slugs(got.Items))
	}
	// The calendars agree: the night of 1 November is over in Harare, not in London.
	if body := e.get("/v1/listings/" + fallsSlug + "/availability?from=2026-11-01&to=2026-11-02").Body.String(); !strings.Contains(body,
		`"days":[{"date":"2026-11-01","available":false},{"date":"2026-11-02","available":true}]`) {
		t.Fatalf("Harare: %s", body)
	}
	if body := e.get("/v1/listings/serviced-apartment-shoreditch/availability?from=2026-11-01&to=2026-11-02").Body.String(); !strings.Contains(body,
		`"timezone":"Europe/London"`) || !strings.Contains(body, `{"date":"2026-11-01","available":true}`) {
		t.Fatalf("London: %s", body)
	}
	if body := e.get("/v1/listings/" + fallsSlug).Body.String(); !strings.Contains(body, `"next_available":"2026-11-02"`) {
		t.Fatalf("next available in Harare: %s", body)
	}
}

// A request to stay: validated against the listing and its calendar; it is
// an enquiry, so it blocks nothing and creates no appointment.
func TestStayRequests(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	c.setCalendar(fallsHost, 22, change("2026-11-10", "2026-11-16", "BOOKED", "The Moyos"))
	before := c.setCalendar(fallsHost, 22, change("2027-01-01", "2027-01-01", "AVAILABLE")).ranges()
	audits := len(e.calendars.Audits())

	// Each request from its own address: the per-IP limit is not under test.
	post := func(body any) *httptest.ResponseRecorder {
		e.seq++
		return e.post("/v1/property-requests", body, withIP(fmt.Sprintf("198.51.100.%d", e.seq%250)))
	}
	stay := func(n int, checkIn, checkOut string, guests any) obj {
		pr := e.propertyRequest(n)
		pr["request_type"] = "STAY_REQUEST"
		pr["check_in"], pr["check_out"] = checkIn, checkOut
		if guests != nil {
			pr["guests"] = guests
		}
		return pr
	}
	// Free nights: 201, and the receipt is the usual one.
	rec := post(stay(22, "2026-11-20", "2026-11-23", 4))
	wantStatus(t, rec, http.StatusCreated)
	if body := rec.Body.String(); !strings.Contains(body, `"status":"RECEIVED"`) || !strings.Contains(body, `"appointment":null`) {
		t.Fatalf("receipt: %s", body)
	}
	// Nights that are taken: 409 with the first unavailable date under fields.check_in.
	for checkIn, first := range map[string]string{"2026-11-08": "2026-11-10", "2026-11-12": "2026-11-12", "2026-11-16": "2026-11-16"} {
		env := wantError(t, post(stay(22, checkIn, "2026-11-18", 2)), http.StatusConflict, propertyrequests.CodeDatesUnavailable)
		if !strings.Contains(env.Error.Fields["check_in"], first) || len(env.Error.Fields) != 1 {
			t.Fatalf("check_in %s: fields = %v, want the first taken night %s", checkIn, env.Error.Fields, first)
		}
	}
	// Leaving on the first booked day is fine (the 10th is not a night of the stay).
	wantStatus(t, post(stay(22, "2026-11-07", "2026-11-10", 2)), http.StatusCreated)

	// The other rules are 400s.
	for name, tt := range map[string]struct {
		body  obj
		field string
	}{
		"fewer than min_nights": {stay(22, "2026-11-20", "2026-11-21", 2), "check_out"},
		"too many guests":       {stay(22, "2026-11-20", "2026-11-23", 9), "guests"},
		"in the past":           {stay(22, "2026-10-30", "2026-11-03", 2), "check_in"},
		"beyond the window":     {stay(22, "2027-10-30", "2027-11-05", 2), "check_out"},
		"out before in":         {stay(22, "2026-11-23", "2026-11-20", 2), "check_out"},
		"more than 90 nights":   {stay(22, "2026-11-20", "2027-02-19", 2), "check_out"},
		"no guests":             {stay(22, "2026-11-20", "2026-11-23", nil), "guests"},
		"zero guests":           {stay(22, "2026-11-20", "2026-11-23", 0), "guests"},
		"no check_out":          {stay(22, "2026-11-20", "", 2), "check_out"},
		"not a date":            {stay(22, "20 November", "2026-11-23", 2), "check_in"},
		"not a short stay":      {stay(1, "2026-11-20", "2026-11-23", 2), "request_type"},
		"a slot on a stay": {func() obj {
			b := stay(22, "2026-11-20", "2026-11-23", 2)
			b["preferred_slot_utc"] = e.slot(48 * time.Hour)
			return b
		}(), "preferred_slot_utc"},
		"dates on an enquiry": {func() obj { b := e.propertyRequest(22); b["check_in"] = "2026-11-20"; return b }(), "check_in"},
		"guests on a viewing": {func() obj { b := e.viewingRequest(22, 48*time.Hour); b["guests"] = 2; return b }(), "guests"},
	} {
		env := wantError(t, post(tt.body), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields[tt.field] == "" {
			t.Fatalf("%s: fields = %v, want %s", name, env.Error.Fields, tt.field)
		}
	}
	// Idempotent like every request: same key and content replays, other dates conflict.
	again := stay(22, "2026-12-01", "2026-12-04", 3)
	wantStatus(t, post(again), http.StatusCreated)
	wantStatus(t, post(again), http.StatusOK)
	again["check_out"] = "2026-12-05"
	wantError(t, post(again), http.StatusConflict, propertyrequests.CodeIdempotencyConflict)

	// It changed no availability: same ranges, no audit entry, and the nights
	// that were asked for are still free for the next person.
	rec = e.get(listingPath(22)+"/availability", c.as(fallsHost))
	if after := decode[calendarBody](t, rec).ranges(); after != before || len(e.calendars.Audits()) != audits {
		t.Fatalf("a request to stay changed the calendar: %s", after)
	}
	wantStatus(t, post(stay(22, "2026-11-20", "2026-11-23", 4)), http.StatusCreated)
	if e.appts != nil {
		if rec := e.get("/v1/inbox/appointments", c.as(fallsHost)); !strings.Contains(rec.Body.String(), `"total":0`) && !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Fatalf("a request to stay created an appointment: %s", rec.Body.String())
		}
	}

	// The lister's inbox shows the dates, nights and guests; other requests have none.
	wantStatus(t, post(e.propertyRequest(22)), http.StatusCreated)
	inbox := e.get("/v1/agent/property-requests", c.as(fallsHost))
	wantStatus(t, inbox, http.StatusOK)
	body := inbox.Body.String()
	if !strings.Contains(body, `"request_type":"STAY_REQUEST"`) || !strings.Contains(body, `"stay":{"check_in":"2026-11-20","check_out":"2026-11-23","nights":3,"guests":4}`) ||
		!strings.Contains(body, `"stay":null`) {
		t.Fatalf("inbox: %s", body)
	}
	// The stored request carries the stay, which is what the emails print.
	var stays int
	for _, req := range e.requests.Requests() {
		if req.Stay != nil {
			stays++
			if req.RequestType != propertyrequests.TypeStayRequest || req.Stay.Nights < 2 {
				t.Fatalf("stored stay: %+v", req.Stay)
			}
		}
	}
	if stays != 4 {
		t.Fatalf("%d stored requests carry a stay, want 4", stays)
	}
}
