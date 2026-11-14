package app

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/dashboard"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// The dashboards through the full router: GET /v1/me/dashboard, the members
// of an organisation and the administrators' overview.

type dashboardBody struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Roles []struct {
		Role   string `json:"role"`
		Label  string `json:"label"`
		Status string `json:"status"`
	} `json:"roles"`
	Organisations []struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		Role              string `json:"role"`
		RoleLabel         string `json:"role_label"`
		CanSeeMembers     bool   `json:"can_see_members"`
		CanManageListings bool   `json:"can_manage_listings"`
	} `json:"organisations"`
	DirectOwner  bool `json:"direct_owner"`
	Capabilities struct {
		ReceivesRequests          bool `json:"receives_requests"`
		ReceivesBusinessEnquiries bool `json:"receives_business_enquiries"`
		ManagesAppointments       bool `json:"manages_appointments"`
		ManagesListings           bool `json:"manages_listings"`
		SeesTeam                  bool `json:"sees_team"`
	} `json:"capabilities"`
	Onboarding []struct {
		Role          string `json:"role"`
		Status        string `json:"status"`
		StatusLabel   string `json:"status_label"`
		MissingFields int    `json:"missing_fields"`
	} `json:"onboarding"`
	NeedsAttention int `json:"needs_attention"`
	Counters       []struct {
		Key    string  `json:"key"`
		Label  string  `json:"label"`
		Value  int     `json:"value"`
		Group  string  `json:"group"`
		Target *string `json:"target"`
	} `json:"counters"`
	NextActions []struct {
		Code   string  `json:"code"`
		Text   string  `json:"text"`
		Target string  `json:"target"`
		Count  *int    `json:"count"`
		Role   *string `json:"role"`
	} `json:"next_actions"`
	Recent struct {
		ReceivedRequests []obj `json:"received_requests"`
		MyRequests       []obj `json:"my_requests"`
		Appointments     []obj `json:"appointments"`
	} `json:"recent"`
}

// counter returns a counter's value, or -1 when the dashboard has none.
func (d dashboardBody) counter(key string) int {
	for _, c := range d.Counters {
		if c.Key == key {
			return c.Value
		}
	}
	return -1
}

func (d dashboardBody) action(code string) bool {
	for _, a := range d.NextActions {
		if a.Code == code {
			return true
		}
	}
	return false
}

func (e *env) dashboard(who reqOpt) dashboardBody {
	e.t.Helper()
	rec := e.get("/v1/me/dashboard", who)
	wantStatus(e.t, rec, http.StatusOK)
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		e.t.Fatalf("Cache-Control = %q", got)
	}
	// Arrays are never null.
	for _, key := range []string{"roles", "organisations", "onboarding", "counters", "next_actions", "received_requests", "my_requests", "appointments"} {
		if strings.Contains(rec.Body.String(), `"`+key+`":null`) {
			e.t.Fatalf("%s is null: %s", key, rec.Body)
		}
	}
	return decode[dashboardBody](e.t, rec)
}

func TestMyDashboard(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	tendai, rudo, blake := c.as(identity.SeedAgentEmail), c.as(devseed.EstatesOwnerEmail), c.as(identity.SeedAgentBEmail)
	consumer, pending := c.as(identity.SeedConsumerEmail), c.as(devseed.PendingAgentEmail)

	// The consumer sends two requests about Tendai's listings, one with a
	// viewing; a guest sends one about Chipo's listing 7.
	first := e.sent(asCustomer(e.propertyRequest(1)), consumer)
	e.now = e.now.Add(time.Minute)
	e.sent(asCustomer(e.viewingRequest(2, 72*time.Hour)), consumer)
	e.now = e.now.Add(time.Minute)
	e.sent(e.propertyRequest(7))

	t.Run("anonymous", func(t *testing.T) {
		wantError(t, e.get("/v1/me/dashboard"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("a property seeker", func(t *testing.T) {
		d := e.dashboard(consumer)
		if d.Kind != dashboard.KindSeeker || d.Name != "Casey Consumer" || len(d.Roles) != 1 || d.Roles[0].Role != "CONSUMER" ||
			len(d.Organisations) != 0 || d.DirectOwner || len(d.Onboarding) != 0 {
			t.Fatalf("seeker: %+v", d)
		}
		if d.Capabilities.ReceivesRequests || d.Capabilities.ManagesListings || d.Capabilities.ManagesAppointments || d.Capabilities.SeesTeam {
			t.Fatalf("a seeker has professional capabilities: %+v", d.Capabilities)
		}
		if d.counter("my_requests.total") != 2 || d.counter("my_requests.open") != 2 || d.counter("my_requests.status.RECEIVED") != 2 ||
			d.counter("my_appointments.upcoming") != 1 || d.counter("my_appointments.needs_reply") != 0 {
			t.Fatalf("seeker counters: %+v", d.Counters)
		}
		// Nothing of the receiving side.
		for _, c := range d.Counters {
			if c.Group != dashboard.GroupMyActivity {
				t.Fatalf("a seeker got counter %s", c.Key)
			}
		}
		if len(d.Recent.MyRequests) != 2 || len(d.Recent.ReceivedRequests) != 0 || len(d.Recent.Appointments) != 1 ||
			d.Recent.Appointments[0]["side"] != "CUSTOMER" || d.Recent.MyRequests[0]["status"] != "RECEIVED" {
			t.Fatalf("seeker recent: %+v", d.Recent)
		}
		if !d.action("FIND_A_PROPERTY") || len(d.NextActions) != 1 || d.NextActions[0].Target != dashboard.TargetSearch || d.NeedsAttention != 0 {
			t.Fatalf("seeker actions: %+v", d.NextActions)
		}
	})

	t.Run("a professional waiting for verification", func(t *testing.T) {
		d := e.dashboard(pending)
		if d.Kind != dashboard.KindProfessionalPending || len(d.Organisations) != 0 || len(d.Roles) != 1 ||
			d.Roles[0].Role != "AGENT" || d.Roles[0].Status != "PENDING_VERIFICATION" || d.Roles[0].Label == "AGENT" {
			t.Fatalf("pending: %+v", d)
		}
		if len(d.Onboarding) != 1 || d.Onboarding[0].Role != "AGENT" || d.Onboarding[0].Status != "DRAFT" || d.Onboarding[0].MissingFields == 0 {
			t.Fatalf("onboarding: %+v", d.Onboarding)
		}
		if len(d.NextActions) != 1 || d.NextActions[0].Code != "COMPLETE_ONBOARDING" || d.NextActions[0].Target != dashboard.TargetOnboarding ||
			d.NextActions[0].Role == nil || *d.NextActions[0].Role != "AGENT" {
			t.Fatalf("actions: %+v", d.NextActions)
		}
		// No access yet: no counters, no lists.
		if len(d.Counters) != 0 || d.Capabilities.ReceivesRequests || len(d.Recent.ReceivedRequests) != 0 {
			t.Fatalf("an unverified professional sees work: %+v", d)
		}
	})

	t.Run("an agent sees their own share", func(t *testing.T) {
		d := e.dashboard(tendai)
		if d.Kind != dashboard.KindProfessional || len(d.Organisations) != 1 || d.Organisations[0].Name != "Sample Estates" ||
			d.Organisations[0].RoleLabel != "Estate Agent" || d.Organisations[0].CanSeeMembers || !d.Organisations[0].CanManageListings ||
			d.Capabilities.SeesTeam || !d.Capabilities.ReceivesRequests || !d.Capabilities.ManagesAppointments || !d.Capabilities.ManagesListings {
			t.Fatalf("agent: %+v", d)
		}
		// Two of the three requests are his; Chipo's is not.
		if d.counter("requests.new") != 2 || d.counter("requests.open") != 2 || d.counter("requests.stage.NEW_ENQUIRY") != 2 ||
			d.counter("requests.stage.CLOSED") != 0 || d.counter("business_enquiries.new") != 0 || d.counter("appointments.requested") != 1 {
			t.Fatalf("agent counters: %+v", d.Counters)
		}
		if len(d.Recent.ReceivedRequests) != 2 || d.Recent.ReceivedRequests[0]["stage"] != "NEW_ENQUIRY" ||
			len(d.Recent.Appointments) != 1 || d.Recent.Appointments[0]["side"] != "PROFESSIONAL" || len(d.Recent.MyRequests) != 0 {
			t.Fatalf("agent recent: %+v", d.Recent)
		}
		// Two new requests and one appointment to confirm.
		if !d.action("RESPOND_TO_NEW_REQUESTS") || !d.action("CONFIRM_APPOINTMENTS") || d.action("FIND_A_PROPERTY") || d.NeedsAttention != 3 {
			t.Fatalf("agent actions: %+v", d.NextActions)
		}
		if d.counter("my_requests.total") != -1 {
			t.Fatal("a professional who sent nothing got seeker counters")
		}
		// The same number as the badge endpoint.
		summary := decode[obj](t, e.get("/v1/inbox/summary", tendai))
		if summary["property_requests"].(obj)["new"] != float64(2) {
			t.Fatalf("inbox summary: %v", summary)
		}
		// Listing counters match GET /v1/me/listings.
		mine := decode[obj](t, e.get("/v1/me/listings", tendai))
		if d.counter("listings.total") != int(mine["total"].(float64)) || d.counter("listings.status.PUBLISHED") < 1 {
			t.Fatalf("listing counters: %+v vs %v", d.Counters, mine["total"])
		}
	})

	t.Run("working a request moves the numbers", func(t *testing.T) {
		wantStatus(t, e.patch("/v1/agent/property-requests/"+first, obj{"stage": "FOLLOW_UP_REQUIRED"}, tendai), http.StatusOK)
		d := e.dashboard(tendai)
		if d.counter("requests.new") != 1 || d.counter("requests.open") != 2 || d.counter("requests.stage.FOLLOW_UP_REQUIRED") != 1 || !d.action("FOLLOW_UP_REQUESTS") {
			t.Fatalf("after a move: %+v", d.Counters)
		}
		if got := e.dashboard(consumer); got.counter("my_requests.status.IN_PROGRESS") != 1 || got.counter("my_requests.status.RECEIVED") != 1 {
			t.Fatalf("the customer's counters: %+v", got.Counters)
		}
	})

	t.Run("the owner sees the whole organisation", func(t *testing.T) {
		d := e.dashboard(rudo)
		if d.Kind != dashboard.KindProfessional || !d.Organisations[0].CanSeeMembers || !d.Capabilities.SeesTeam ||
			d.Organisations[0].RoleLabel != "Agency Principal" {
			t.Fatalf("owner: %+v", d)
		}
		if d.counter("requests.new") != 2 || d.counter("requests.open") != 3 || len(d.Recent.ReceivedRequests) != 3 {
			t.Fatalf("owner counters: %+v", d.Counters)
		}
	})

	t.Run("another organisation learns nothing", func(t *testing.T) {
		d := e.dashboard(blake)
		if d.Kind != dashboard.KindProfessional || d.Organisations[0].Name != identity.SeedSecondOrganisationName {
			t.Fatalf("other agent: %+v", d)
		}
		if d.counter("requests.new") != 0 || d.counter("requests.open") != 0 || d.counter("appointments.requested") != 0 ||
			len(d.Recent.ReceivedRequests) != 0 || len(d.Recent.Appointments) != 0 || len(d.NextActions) != 0 {
			t.Fatalf("another organisation saw: %+v", d)
		}
	})

	t.Run("a direct seller", func(t *testing.T) {
		e.sent(e.propertyRequest(6))
		d := e.dashboard(c.as(devseed.SellerEmail))
		if d.Kind != dashboard.KindDirectOwner || !d.DirectOwner || len(d.Organisations) != 0 || d.Capabilities.ReceivesBusinessEnquiries ||
			d.counter("requests.new") != 1 || d.counter("business_enquiries.new") != -1 || d.counter("listings.total") < 1 {
			t.Fatalf("seller: %+v", d)
		}
	})

	t.Run("an administrator's session is sent to the overview", func(t *testing.T) {
		d := e.dashboard(withCookie(e.admin()))
		if d.Kind != dashboard.KindAdmin || len(d.Counters) != 0 || len(d.NextActions) != 1 || d.NextActions[0].Target != dashboard.TargetAdminOverview {
			t.Fatalf("admin: %+v", d)
		}
	})
}

func TestOrganisationMembers(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	estates := "/v1/agent/organisations/" + propertyrequests.SeedOrganisationID.String() + "/members"

	t.Run("refusals", func(t *testing.T) {
		wantError(t, e.get(estates), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		// Not a member: the same answer as for an organisation that does not exist.
		for _, email := range []string{identity.SeedConsumerEmail, devseed.PendingAgentEmail, identity.SeedAgentBEmail, devseed.CommercialAgentEmail, devseed.SellerEmail} {
			wantError(t, e.get(estates, c.as(email)), http.StatusNotFound, dashboard.CodeOrganisationNotFound)
		}
		wantError(t, e.get(estates, withCookie(e.admin())), http.StatusNotFound, dashboard.CodeOrganisationNotFound)
		wantError(t, e.get("/v1/agent/organisations/nope/members", c.as(devseed.EstatesOwnerEmail)), http.StatusNotFound, dashboard.CodeOrganisationNotFound)
		wantError(t, e.get("/v1/agent/organisations/"+identity.SeedSecondOrganisationID.String()+"/members", c.as(devseed.EstatesOwnerEmail)),
			http.StatusNotFound, dashboard.CodeOrganisationNotFound)
		// A member who is neither owner nor manager.
		wantError(t, e.get(estates, c.as(identity.SeedAgentEmail)), http.StatusForbidden, httpx.CodeForbidden)
	})

	t.Run("the owner sees the team", func(t *testing.T) {
		rec := e.get(estates, c.as(devseed.EstatesOwnerEmail))
		wantStatus(t, rec, http.StatusOK)
		body := decode[obj](t, rec)
		if org := body["organisation"].(obj); org["name"] != "Sample Estates" || org["kind"] != "AGENCY" {
			t.Fatalf("organisation: %v", org)
		}
		items := list(body, "items")
		if len(items) != 3 {
			t.Fatalf("members: %s", rec.Body)
		}
		first := items[0].(obj)
		if first["name"] != "Rudo Moyo" || first["role"] != "OWNER" || first["role_label"] != "Agency Principal" ||
			first["status"] != "ACTIVE" || first["status_label"] != "Active" || first["is_you"] != true || first["market_code"] != "ZW" {
			t.Fatalf("owner row: %v", first)
		}
		// Agents after the owner, by name; no contact details anywhere.
		if items[1].(obj)["name"] != "Chipo Dube" || items[2].(obj)["name"] != "Tendai Ncube" || items[2].(obj)["role_label"] != "Estate Agent" || items[2].(obj)["is_you"] != false {
			t.Fatalf("members: %s", rec.Body)
		}
		if strings.Contains(rec.Body.String(), "@") || strings.Contains(rec.Body.String(), "email") {
			t.Fatalf("the member list carries contact details: %s", rec.Body)
		}
	})

	t.Run("a manager may too", func(t *testing.T) {
		if err := e.identity.SetMembership(identity.SeedAgentUserID, propertyrequests.SeedOrganisationID, "ZW", auth.RoleManager); err != nil {
			t.Fatal(err)
		}
		rec := e.get(estates, withBearer(e.loginBearer(identity.SeedAgentEmail, seedPassword)))
		wantStatus(t, rec, http.StatusOK)
		if items := list(decode[obj](t, rec), "items"); items[1].(obj)["role_label"] != "Agency Administrator" || items[1].(obj)["is_you"] != true {
			t.Fatalf("manager's view: %s", rec.Body)
		}
	})
}

func TestAdminOverview(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	org := propertyrequests.SeedOrganisationID
	e.requests.PutListing(propertyrequests.Listing{
		ID: propertyrequests.SeedListingID(32), MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
		OrganisationID: &org, OrganisationEligible: false,
	})
	e.sent(e.propertyRequest(32)) // a routing exception
	routed := e.sent(e.propertyRequest(1))
	e.sent(e.propertyRequest(2))
	wantStatus(t, e.patch("/v1/agent/property-requests/"+routed, obj{"stage": "CONTACTED"}, c.as(identity.SeedAgentEmail)), http.StatusOK)

	const path = "/v1/admin/overview"
	overview := func(cookie *http.Cookie) obj {
		t.Helper()
		rec := e.get(path, withCookie(cookie))
		wantStatus(t, rec, http.StatusOK)
		return decode[obj](t, rec)
	}
	sections := func(o obj) string {
		var out []string
		for _, s := range list(o, "sections") {
			out = append(out, s.(string))
		}
		return strings.Join(out, ",")
	}
	countOf := func(rows []any, key, value string) float64 {
		for _, row := range rows {
			if row.(obj)[key] == value {
				return row.(obj)["count"].(float64)
			}
		}
		return -1
	}

	t.Run("refusals", func(t *testing.T) {
		wantError(t, e.get(path), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		for _, who := range []reqOpt{c.as(identity.SeedConsumerEmail), c.as(devseed.EstatesOwnerEmail), c.as(devseed.PendingAgentEmail), withCookie(e.customerScopeAdmin())} {
			wantError(t, e.get(path, who), http.StatusForbidden, httpx.CodeForbidden)
		}
	})

	t.Run("a super administrator sees every section", func(t *testing.T) {
		o := overview(e.admin())
		if sections(o) != "accounts,verifications,listings,requests" {
			t.Fatalf("sections = %s", sections(o))
		}
		accounts := o["accounts"].(obj)
		if accounts["total"].(float64) != float64(e.identity.UserCount()-1) || accounts["administrators"] != float64(1) ||
			countOf(list(accounts, "by_status"), "status", "ACTIVE") != accounts["total"].(float64) {
			t.Fatalf("accounts: %v", accounts)
		}
		var consumers float64
		for _, row := range list(accounts, "by_role") {
			if r := row.(obj); r["role"] == "CONSUMER" && r["status"] == "ACTIVE" && r["label"] != "CONSUMER" {
				consumers = r["count"].(float64)
			}
		}
		recent := list(accounts, "recent")
		if consumers < 1 || len(recent) != 10 || recent[0].(obj)["email"] == nil || recent[0].(obj)["roles"] == nil {
			t.Fatalf("by_role / recent: %v", accounts)
		}
		for _, row := range recent {
			if row.(obj)["email"] == identity.SeedAdminEmail {
				t.Fatal("an administrator is listed among the new accounts")
			}
		}
		verifications := o["verifications"].(obj)
		if verifications["waiting"] != float64(1) || countOf(list(verifications, "by_status"), "status", "SUBMITTED") != 1 {
			t.Fatalf("verifications: %v", verifications)
		}
		listings := o["listings"].(obj)
		if listings["market_codes"] != nil || listings["total"].(float64) < 18 || countOf(list(listings, "by_status"), "status", "PUBLISHED") < 18 ||
			countOf(list(listings, "by_status"), "status", "EXPIRED") != 1 {
			t.Fatalf("listings: %v", listings)
		}
		requests := o["requests"].(obj)
		if requests["market_codes"] != nil || requests["total"] != float64(3) || requests["last_7_days"] != float64(3) ||
			countOf(list(requests, "by_stage"), "stage", "NEW_ENQUIRY") != 1 || countOf(list(requests, "by_stage"), "stage", "CONTACTED") != 1 ||
			requests["routing_exceptions"].(obj)["open"] != float64(1) || requests["routing_exceptions"].(obj)["resolved"] != float64(0) {
			t.Fatalf("requests: %v", requests)
		}
		// Counts only: nothing about any single request.
		if body := e.get(path, withCookie(e.admin())).Body.String(); strings.Contains(body, "tariro@example.com") || strings.Contains(body, "RP-") {
			t.Fatalf("the overview carries request data: %s", body)
		}
	})

	t.Run("each role gets its own sections", func(t *testing.T) {
		tests := []struct{ role, market, want string }{
			{auth.PlatformAdmin, "", "accounts,verifications,listings,requests"},
			{auth.PlatformSupport, "", "accounts,requests"},
			{auth.PlatformModerator, "", "verifications,listings"},
			{auth.PlatformMarketAdmin, "ZW", "verifications,listings,requests"},
			{auth.PlatformFinanceAdmin, "", ""},
			{auth.PlatformContentAdmin, "", ""},
		}
		for _, tt := range tests {
			o := overview(e.platformAdmin(strings.ToLower(tt.role)+"@example.test", tt.role, tt.market))
			if sections(o) != tt.want {
				t.Fatalf("%s: sections = %q, want %q", tt.role, sections(o), tt.want)
			}
			for _, section := range []string{"accounts", "verifications", "listings", "requests"} {
				if has := o[section] != nil; has != strings.Contains(tt.want, section) {
					t.Fatalf("%s: section %s present = %v", tt.role, section, has)
				}
			}
			if roles := list(o, "roles"); len(roles) != 1 || roles[0].(obj)["role"] != tt.role {
				t.Fatalf("%s: roles = %v", tt.role, o["roles"])
			}
		}
	})

	t.Run("a market administrator counts their own country only", func(t *testing.T) {
		all := overview(e.admin())["listings"].(obj)["total"].(float64)
		gb := overview(e.platformAdmin("gb.overview@example.test", auth.PlatformMarketAdmin, "GB"))
		listings, requests := gb["listings"].(obj), gb["requests"].(obj)
		if codes := list(listings, "market_codes"); len(codes) != 1 || codes[0] != "GB" || listings["total"].(float64) < 1 || listings["total"].(float64) >= all {
			t.Fatalf("GB listings: %v (all: %v)", listings, all)
		}
		// Every request above is about a Zimbabwean listing.
		if requests["total"] != float64(0) || requests["routing_exceptions"].(obj)["open"] != float64(0) || gb["accounts"] != nil {
			t.Fatalf("GB requests: %v", requests)
		}
	})
}
