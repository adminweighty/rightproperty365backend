package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Working a property request, the customer's own history and the routing
// exceptions, all through the full router.
//
// The cast: listing 1 belongs to Sample Estates and is assigned to Tendai
// (identity.SeedAgentEmail); Rudo (devseed.EstatesOwnerEmail) owns the agency
// and Chipo (devseed.EstatesAgent2Email) is a colleague who is NOT assigned.
// Blake (identity.SeedAgentBEmail) works for another agency. Listing 6 is
// sold directly by Grace (devseed.SellerEmail).

func (e *env) patch(path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	return e.do(http.MethodPatch, path, body, opts...)
}

// sent posts a property request and returns its id.
func (e *env) sent(body obj, opts ...reqOpt) string {
	e.t.Helper()
	rec := e.post("/v1/property-requests", body, opts...)
	wantStatus(e.t, rec, http.StatusCreated)
	return decode[obj](e.t, rec)["request_id"].(string)
}

// asCustomer makes a request body one of a signed-in customer.
func asCustomer(body obj) obj {
	delete(body, "guest_contact")
	return body
}

func requestOf(t *testing.T, rec *httptest.ResponseRecorder) obj {
	t.Helper()
	wantStatus(t, rec, rec.Code)
	return decode[obj](t, rec)["request"].(obj)
}

func list(o obj, key string) []any {
	v, _ := o[key].([]any)
	return v
}

const internalNote = "Called twice, prefers evenings. Budget is firm."

func TestWorkingAPropertyRequest(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	tendai, rudo, chipo, blake := c.as(identity.SeedAgentEmail), c.as(devseed.EstatesOwnerEmail), c.as(devseed.EstatesAgent2Email), c.as(identity.SeedAgentBEmail)
	consumer, pending := c.as(identity.SeedConsumerEmail), c.as(devseed.PendingAgentEmail)

	id := e.sent(asCustomer(e.propertyRequest(1)), consumer)
	path := "/v1/agent/property-requests/" + id

	t.Run("who may open a request", func(t *testing.T) {
		wantError(t, e.get(path), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		wantError(t, e.get(path, consumer), http.StatusForbidden, httpx.CodeForbidden)
		wantError(t, e.get(path, pending), http.StatusForbidden, httpx.CodeForbidden)
		// Another agency, and a colleague the request is not assigned to: the
		// same answer as for a request that does not exist.
		for _, who := range []reqOpt{blake, chipo} {
			wantError(t, e.get(path, who), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		}
		wantError(t, e.get("/v1/agent/property-requests/not-a-uuid", tendai), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		wantError(t, e.get("/v1/agent/property-requests/0195c0de-0000-7000-8000-00000000ffff", tendai), http.StatusNotFound, propertyrequests.CodeRequestNotFound)

		for _, who := range []reqOpt{tendai, rudo} {
			rec := e.get(path, who)
			wantStatus(t, rec, http.StatusOK)
			r := requestOf(t, rec)
			if r["stage"] != "NEW_ENQUIRY" || r["stage_label"] != "New enquiry" || r["stage_changed_at"] != nil || r["status"] != "RECEIVED" {
				t.Fatalf("new request: %v", r)
			}
			if len(list(r, "allowed_stages")) != 6 || len(list(r, "timeline")) != 1 || len(list(r, "notes")) != 0 {
				t.Fatalf("stages / timeline / notes: %s", rec.Body)
			}
			if r["contact"].(obj)["email"] != identity.SeedConsumerEmail || r["listing"].(obj)["reference"] != "RP-ZW-10001" {
				t.Fatalf("contact / listing: %s", rec.Body)
			}
		}
	})

	t.Run("who may move a request", func(t *testing.T) {
		move := obj{"stage": "CONTACTED"}
		wantError(t, e.patch(path, move), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		wantError(t, e.patch(path, move, consumer), http.StatusForbidden, httpx.CodeForbidden)
		wantError(t, e.patch(path, move, pending), http.StatusForbidden, httpx.CodeForbidden)
		wantError(t, e.patch(path, move, blake), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		wantError(t, e.patch(path, move, chipo), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		// A cookie session without the web app's Origin is refused before anything is read.
		wantError(t, e.patch(path, move, withCookie(e.loginCookie(identity.SeedAgentEmail, seedPassword))), http.StatusForbidden, httpx.CodeCSRFOrigin)
		if got := requestOf(t, e.get(path, tendai))["stage"]; got != "NEW_ENQUIRY" {
			t.Fatalf("a refused move changed the stage to %v", got)
		}
	})

	t.Run("stage transitions are validated and recorded", func(t *testing.T) {
		env := wantError(t, e.patch(path, obj{"stage": "WON"}, tendai), http.StatusBadRequest, httpx.CodeValidationFailed)
		if !strings.Contains(env.Error.Fields["stage"], "NEW_ENQUIRY") {
			t.Fatalf("fields = %v", env.Error.Fields)
		}
		wantError(t, e.patch(path, obj{"stage": "CONTACTED", "assigned_to": "x"}, tendai), http.StatusBadRequest, httpx.CodeInvalidJSON)
		// Not reachable from NEW_ENQUIRY.
		for _, stage := range []string{"CONVERTED", "NEW_ENQUIRY"} {
			wantError(t, e.patch(path, obj{"stage": stage}, tendai), http.StatusConflict, propertyrequests.CodeInvalidStageTransition)
		}

		e.now = e.now.Add(time.Hour)
		rec := e.patch(path, obj{"stage": "CONTACTED"}, tendai)
		wantStatus(t, rec, http.StatusOK)
		r := requestOf(t, rec)
		if r["stage"] != "CONTACTED" || r["stage_label"] != "Contacted" || r["stage_changed_at"] != e.now.Format(time.RFC3339) {
			t.Fatalf("moved request: %s", rec.Body)
		}
		timeline := list(r, "timeline")
		if len(timeline) != 2 {
			t.Fatalf("timeline: %s", rec.Body)
		}
		last := timeline[1].(obj)
		if last["event"] != "STAGE_CHANGED" || last["from_stage"] != "NEW_ENQUIRY" || last["to_stage"] != "CONTACTED" ||
			last["actor"] != "PROFESSIONAL" || last["actor_name"] != "Tendai Ncube" || last["label"] != "Moved to Contacted" {
			t.Fatalf("timeline entry: %v", last)
		}
		// The same stage again, and back to new: refused.
		wantError(t, e.patch(path, obj{"stage": "CONTACTED"}, tendai), http.StatusConflict, propertyrequests.CodeInvalidStageTransition)
		wantError(t, e.patch(path, obj{"stage": "NEW_ENQUIRY"}, rudo), http.StatusConflict, propertyrequests.CodeInvalidStageTransition)

		// One outbox event per move, carrying no personal data.
		var events int
		for _, ev := range e.requests.Outbox() {
			if ev.EventType == propertyrequests.EventStageChanged {
				events++
				if body := string(ev.Payload); !strings.Contains(body, `"to":"CONTACTED"`) || strings.Contains(body, "example") {
					t.Fatalf("event payload: %s", body)
				}
			}
		}
		if events != 1 {
			t.Fatalf("stage events = %d, want 1", events)
		}

		// CONVERTED is terminal but for closing; CLOSED reopens only as a follow-up.
		wantStatus(t, e.patch(path, obj{"stage": "CONVERTED"}, rudo), http.StatusOK)
		wantError(t, e.patch(path, obj{"stage": "CONTACTED"}, rudo), http.StatusConflict, propertyrequests.CodeInvalidStageTransition)
		wantStatus(t, e.patch(path, obj{"stage": "CLOSED"}, rudo), http.StatusOK)
		wantError(t, e.patch(path, obj{"stage": "CONVERTED"}, rudo), http.StatusConflict, propertyrequests.CodeInvalidStageTransition)
		wantStatus(t, e.patch(path, obj{"stage": "FOLLOW_UP_REQUIRED"}, rudo), http.StatusOK)
		wantStatus(t, e.patch(path, obj{"stage": "CONTACTED"}, tendai), http.StatusOK)
	})

	t.Run("internal notes", func(t *testing.T) {
		notes := path + "/notes"
		wantError(t, e.post(notes, obj{"body": internalNote}), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		wantError(t, e.post(notes, obj{"body": internalNote}, consumer), http.StatusForbidden, httpx.CodeForbidden)
		wantError(t, e.post(notes, obj{"body": internalNote}, pending), http.StatusForbidden, httpx.CodeForbidden)
		wantError(t, e.post(notes, obj{"body": internalNote}, blake), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		wantError(t, e.post(notes, obj{"body": internalNote}, chipo), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		for _, bad := range []string{"", "   ", strings.Repeat("x", propertyrequests.MaxNoteLength+1), "bell\a"} {
			env := wantError(t, e.post(notes, obj{"body": bad}, tendai), http.StatusBadRequest, httpx.CodeValidationFailed)
			if env.Error.Fields["body"] == "" {
				t.Fatalf("fields = %v", env.Error.Fields)
			}
		}
		if n := len(e.requests.StoredNotes()); n != 0 {
			t.Fatalf("refused notes were stored: %d", n)
		}

		rec := e.post(notes, obj{"body": "  " + internalNote + "  "}, tendai)
		wantStatus(t, rec, http.StatusCreated)
		got := list(requestOf(t, rec), "notes")
		if len(got) != 1 || got[0].(obj)["body"] != internalNote || got[0].(obj)["author_name"] != "Tendai Ncube" || got[0].(obj)["mine"] != true {
			t.Fatalf("notes: %s", rec.Body)
		}
		// The owner reads it, as somebody else's.
		rec = e.get(path, rudo)
		got = list(requestOf(t, rec), "notes")
		if len(got) != 1 || got[0].(obj)["mine"] != false {
			t.Fatalf("owner's view of the notes: %s", rec.Body)
		}
		// The timeline says a note was added, never what it says.
		if strings.Count(rec.Body.String(), internalNote) != 1 || !strings.Contains(rec.Body.String(), `"event":"NOTE_ADDED"`) {
			t.Fatalf("timeline: %s", rec.Body)
		}
	})

	t.Run("the customer sees their request in their own terms, and never a note", func(t *testing.T) {
		wantError(t, e.get("/v1/me/property-requests"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		wantError(t, e.get("/v1/me/property-requests/"+id), http.StatusUnauthorized, httpx.CodeUnauthenticated)

		rec := e.get("/v1/me/property-requests", consumer)
		wantStatus(t, rec, http.StatusOK)
		page := decode[obj](t, rec)
		items := list(page, "items")
		if len(items) != 1 || page["total"] != float64(1) || page["total_pages"] != float64(1) {
			t.Fatalf("my requests: %s", rec.Body)
		}
		item := items[0].(obj)
		if item["request_id"] != id || item["status"] != "RESPONDED" || item["status_label"] != "Responded" ||
			item["listing"].(obj)["slug"] != "four-bedroom-family-home-borrowdale" {
			t.Fatalf("my request: %s", rec.Body)
		}

		detail := e.get("/v1/me/property-requests/"+id, consumer)
		wantStatus(t, detail, http.StatusOK)
		// Received, responded, closed (converted), in progress (follow-up),
		// responded again: the CONVERTED -> CLOSED move changed nothing for the customer.
		var statuses []string
		for _, entry := range list(requestOf(t, detail), "timeline") {
			statuses = append(statuses, entry.(obj)["status"].(string))
		}
		if strings.Join(statuses, ",") != "RECEIVED,RESPONDED,CLOSED,IN_PROGRESS,RESPONDED" {
			t.Fatalf("customer timeline = %v", statuses)
		}
		for _, body := range []string{rec.Body.String(), detail.Body.String()} {
			for _, leak := range []string{internalNote, "notes", "Tendai", "Rudo", "stage", "CONTACTED", "FOLLOW_UP", "actor", "NOTE_ADDED", "contact\""} {
				if strings.Contains(body, leak) {
					t.Fatalf("the customer's view leaks %q: %s", leak, body)
				}
			}
		}

		// Nobody else can read it through the customer endpoint, not even its recipient.
		for _, who := range []reqOpt{tendai, rudo, blake, pending} {
			wantError(t, e.get("/v1/me/property-requests/"+id, who), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
			other := decode[obj](t, e.get("/v1/me/property-requests", who))
			if len(list(other, "items")) != 0 || other["total"] != float64(0) {
				t.Fatalf("somebody else's list is not empty: %v", other)
			}
		}

		// A request sent as a guest with the account's email address is not
		// linked to the account.
		guest := e.propertyRequest(2)
		guest["guest_contact"] = obj{"name": "Casey Consumer", "email": identity.SeedConsumerEmail}
		guestID := e.sent(guest)
		wantError(t, e.get("/v1/me/property-requests/"+guestID, consumer), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		if n := len(list(decode[obj](t, e.get("/v1/me/property-requests", consumer)), "items")); n != 1 {
			t.Fatalf("a guest request was linked to the account: %d items", n)
		}

		// Paging: a second, newer request; one per page, newest first.
		newer := e.sent(asCustomer(e.propertyRequest(3)), consumer)
		for pageNo, want := range map[int]string{1: newer, 2: id} {
			p := decode[obj](t, e.get(fmt.Sprintf("/v1/me/property-requests?page_size=1&page=%d", pageNo), consumer))
			items := list(p, "items")
			if len(items) != 1 || items[0].(obj)["request_id"] != want || p["total"] != float64(2) || p["total_pages"] != float64(2) || p["page"] != float64(pageNo) {
				t.Fatalf("page %d: %v", pageNo, p)
			}
		}
		if p := decode[obj](t, e.get("/v1/me/property-requests?page_size=1&page=3", consumer)); len(list(p, "items")) != 0 || p["total"] != float64(2) {
			t.Fatalf("page past the end: %v", p)
		}

		// The status filter.
		for query, want := range map[string]int{"status=RESPONDED": 1, "status=RECEIVED": 1, "status=RECEIVED,RESPONDED": 2, "status=CLOSED&status=IN_PROGRESS": 0} {
			if n := len(list(decode[obj](t, e.get("/v1/me/property-requests?"+query, consumer)), "items")); n != want {
				t.Fatalf("%s: %d items, want %d", query, n, want)
			}
		}
		env := wantError(t, e.get("/v1/me/property-requests?status=CONTACTED&page=0&page_size=500", consumer), http.StatusBadRequest, httpx.CodeValidationFailed)
		if len(env.Error.Fields) != 3 {
			t.Fatalf("fields = %v", env.Error.Fields)
		}
	})

	t.Run("a direct seller works their own requests and nobody else's", func(t *testing.T) {
		grace := c.as(devseed.SellerEmail)
		own := e.sent(e.propertyRequest(6))
		rec := e.patch("/v1/agent/property-requests/"+own, obj{"stage": "VIEWING_REQUESTED"}, grace)
		wantStatus(t, rec, http.StatusOK)
		if requestOf(t, rec)["stage"] != "VIEWING_REQUESTED" {
			t.Fatalf("seller's move: %s", rec.Body)
		}
		wantStatus(t, e.post("/v1/agent/property-requests/"+own+"/notes", obj{"body": "Wants to view on Saturday."}, grace), http.StatusCreated)
		wantError(t, e.get("/v1/agent/property-requests/"+own, tendai), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		wantError(t, e.get(path, grace), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		wantError(t, e.patch(path, obj{"stage": "CLOSED"}, grace), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
	})
}

func TestRequestInboxFiltersAndPaging(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	rudo, tendai, blake := c.as(devseed.EstatesOwnerEmail), c.as(identity.SeedAgentEmail), c.as(identity.SeedAgentBEmail)

	// Five requests for Sample Estates, a minute apart: listings 1, 2 and 5
	// are Tendai's, 7 and 10 are Chipo's.
	start := e.now
	var ids []string
	for i, n := range []int{1, 2, 5, 7, 10} {
		e.now = start.Add(time.Duration(i) * time.Minute)
		body := e.propertyRequest(n)
		switch i {
		case 1:
			body = e.viewingRequest(n, 48*time.Hour)
		case 2:
			body["guest_contact"] = obj{"name": "Farai Dube", "email": "farai@example.com"}
			body["message"] = "Is the borehole working? 100% sure_about it"
		}
		ids = append(ids, e.sent(body))
	}
	e.now = start.Add(10 * time.Minute)
	wantStatus(t, e.patch("/v1/agent/property-requests/"+ids[0], obj{"stage": "CONTACTED"}, tendai), http.StatusOK)
	wantStatus(t, e.patch("/v1/agent/property-requests/"+ids[3], obj{"stage": "CLOSED"}, rudo), http.StatusOK)

	type page struct {
		Items []struct {
			RequestID string `json:"request_id"`
			Stage     string `json:"stage"`
		} `json:"items"`
		Page        int `json:"page"`
		PageSize    int `json:"page_size"`
		Total       int `json:"total"`
		TotalPages  int `json:"total_pages"`
		StageCounts []struct {
			Stage string `json:"stage"`
			Label string `json:"label"`
			Count int    `json:"count"`
		} `json:"stage_counts"`
	}
	fetch := func(who reqOpt, query string) page {
		t.Helper()
		rec := e.get("/v1/agent/property-requests?"+query, who)
		wantStatus(t, rec, http.StatusOK)
		return decode[page](t, rec)
	}
	got := func(p page) string {
		var out []string
		for _, it := range p.Items {
			for i, id := range ids {
				if id == it.RequestID {
					out = append(out, fmt.Sprint(i))
				}
			}
		}
		return strings.Join(out, ",")
	}

	t.Run("the owner sees all of it, newest first, with counts per stage", func(t *testing.T) {
		p := fetch(rudo, "")
		if got(p) != "4,3,2,1,0" || p.Total != 5 || p.TotalPages != 1 || p.Page != 1 || p.PageSize != 20 {
			t.Fatalf("page = %+v", p)
		}
		counts := map[string]int{}
		for _, c := range p.StageCounts {
			counts[c.Stage] = c.Count
		}
		if len(p.StageCounts) != 8 || p.StageCounts[0].Stage != "NEW_ENQUIRY" || p.StageCounts[5].Label != "Follow-up required" ||
			counts["NEW_ENQUIRY"] != 3 || counts["CONTACTED"] != 1 || counts["CLOSED"] != 1 {
			t.Fatalf("stage counts = %+v", p.StageCounts)
		}
	})

	t.Run("filters", func(t *testing.T) {
		from := url.QueryEscape(start.Add(time.Minute).Format(time.RFC3339))
		to := url.QueryEscape(start.Add(3 * time.Minute).Format(time.RFC3339))
		tests := []struct{ query, want string }{
			{"stage=NEW_ENQUIRY", "4,2,1"},
			{"stage=CONTACTED,CLOSED", "3,0"},
			{"stage=CONTACTED&stage=CLOSED", "3,0"},
			{"stage=CONVERTED", ""},
			{"type=VIEWING_REQUEST", "1"},
			{"listing_id=" + propertyrequests.SeedListingID(5).String(), "2"},
			{"from=" + from + "&to=" + to, "2,1"}, // from is inclusive, to exclusive
			{"q=farai", "2"},
			{"q=FARAI%40example.com", "2"},
			{"q=borehole", "2"},
			{"q=100%25+sure_about", "2"}, // % and _ are text, not wildcards
			{"q=no-such-thing", ""},
			{"q=borehole&stage=CLOSED", ""},
		}
		for _, tt := range tests {
			if p := fetch(rudo, tt.query); got(p) != tt.want || p.Total != len(p.Items) {
				t.Fatalf("%s: got %q (total %d), want %q", tt.query, got(p), p.Total, tt.want)
			}
		}
		// The counts per stage ignore the stage filter and follow the others.
		p := fetch(rudo, "stage=CLOSED&q=enquiry")
		counts := map[string]int{}
		for _, c := range p.StageCounts {
			counts[c.Stage] = c.Count
		}
		if got(p) != "3" || counts["NEW_ENQUIRY"] != 2 || counts["CONTACTED"] != 1 || counts["CLOSED"] != 1 {
			t.Fatalf("filtered counts: %+v", p)
		}
	})

	t.Run("paging", func(t *testing.T) {
		for pageNo, want := range map[int]string{1: "4,3", 2: "2,1", 3: "0", 4: ""} {
			p := fetch(rudo, fmt.Sprintf("page=%d&page_size=2", pageNo))
			if got(p) != want || p.Total != 5 || p.TotalPages != 3 || p.Page != pageNo || p.PageSize != 2 {
				t.Fatalf("page %d = %+v, want %q", pageNo, p, want)
			}
		}
	})

	t.Run("invalid parameters", func(t *testing.T) {
		bad := []string{"stage=WON", "type=GIFT", "type=ENQUIRY,VIEWING_REQUEST", "listing_id=7", "from=yesterday", "page=0", "page_size=101",
			"from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z", "q=" + strings.Repeat("x", 101)}
		for _, query := range bad {
			wantError(t, e.get("/v1/agent/property-requests?"+query, rudo), http.StatusBadRequest, httpx.CodeValidationFailed)
		}
	})

	t.Run("a filter never widens the scope", func(t *testing.T) {
		// Tendai sees his own three; Chipo's two are not his.
		if p := fetch(tendai, ""); got(p) != "2,1,0" || p.Total != 3 {
			t.Fatalf("agent's page = %+v", p)
		}
		if p := fetch(tendai, "listing_id="+propertyrequests.SeedListingID(7).String()); p.Total != 0 {
			t.Fatalf("an agent read a colleague's request: %+v", p)
		}
		for _, query := range []string{"", "q=enquiry", "stage=NEW_ENQUIRY", "listing_id=" + propertyrequests.SeedListingID(1).String()} {
			p := fetch(blake, query)
			if p.Total != 0 || len(p.Items) != 0 {
				t.Fatalf("another agency saw %+v", p)
			}
			for _, c := range p.StageCounts {
				if c.Count != 0 {
					t.Fatalf("another agency learned the counts: %+v", p.StageCounts)
				}
			}
		}
	})
}

func TestRoutingExceptions(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	org := propertyrequests.SeedOrganisationID
	// A listing nobody can receive: stored as ROUTING_EXCEPTION, in no inbox.
	e.requests.PutListing(propertyrequests.Listing{
		ID: propertyrequests.SeedListingID(32), MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
		OrganisationID: &org, OrganisationEligible: false,
	})
	exception := e.sent(e.propertyRequest(32))
	normal := e.sent(e.propertyRequest(1))

	super := e.admin()
	support := e.platformAdmin("support@example.test", auth.PlatformSupport, "")
	zw := e.platformAdmin("zw.admin@example.test", auth.PlatformMarketAdmin, "ZW")
	gb := e.platformAdmin("gb.admin@example.test", auth.PlatformMarketAdmin, "GB")
	moderator := e.platformAdmin("moderator@example.test", auth.PlatformModerator, "")

	const queue = "/v1/admin/routing-exceptions"
	resolve := queue + "/" + exception + "/resolve"
	total := func(cookie *http.Cookie, query string) int {
		t.Helper()
		rec := e.get(queue+query, withCookie(cookie))
		wantStatus(t, rec, http.StatusOK)
		return int(decode[obj](t, rec)["total"].(float64))
	}

	t.Run("who may see the queue", func(t *testing.T) {
		wantError(t, e.get(queue), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		wantError(t, e.post(resolve, nil), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		for _, who := range []reqOpt{c.as(identity.SeedConsumerEmail), c.as(devseed.EstatesOwnerEmail), c.as(devseed.PendingAgentEmail),
			withCookie(moderator), withCookie(e.customerScopeAdmin())} {
			wantError(t, e.get(queue, who), http.StatusForbidden, httpx.CodeForbidden)
		}
		wantError(t, e.post(resolve, nil, browser(moderator)), http.StatusForbidden, httpx.CodeForbidden)

		rec := e.get(queue, withCookie(super))
		wantStatus(t, rec, http.StatusOK)
		items := list(decode[obj](t, rec), "items")
		if len(items) != 1 {
			t.Fatalf("queue: %s", rec.Body)
		}
		item := items[0].(obj)
		if item["request_id"] != exception || item["state"] != "OPEN" || item["resolved_at"] != nil ||
			item["contact"].(obj)["email"] != "tariro@example.com" {
			t.Fatalf("exception: %s", rec.Body)
		}
		// A market administrator sees their own country's only.
		if total(support, "") != 1 || total(zw, "") != 1 || total(gb, "") != 0 || total(gb, "?state=ALL") != 0 {
			t.Fatal("market scoping of the queue is wrong")
		}
		wantError(t, e.get(queue+"?state=DONE", withCookie(super)), http.StatusBadRequest, httpx.CodeValidationFailed)
	})

	t.Run("the exception is in nobody's inbox", func(t *testing.T) {
		wantError(t, e.get("/v1/agent/property-requests/"+exception, c.as(devseed.EstatesOwnerEmail)), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		wantError(t, e.patch("/v1/agent/property-requests/"+exception, obj{"stage": "CONTACTED"}, c.as(devseed.EstatesOwnerEmail)),
			http.StatusNotFound, propertyrequests.CodeRequestNotFound)
	})

	t.Run("resolving", func(t *testing.T) {
		// Another country's administrator, and a request that was routed: not found.
		wantError(t, e.post(resolve, nil, browser(gb)), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		wantError(t, e.post(queue+"/"+normal+"/resolve", nil, browser(super)), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		wantError(t, e.post(queue+"/nope/resolve", nil, browser(super)), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		// The administrator session is a cookie: the Origin rule applies.
		wantError(t, e.post(resolve, nil, withCookie(zw)), http.StatusForbidden, httpx.CodeCSRFOrigin)
		wantError(t, e.post(resolve, obj{"note": strings.Repeat("x", 2001)}, browser(zw)), http.StatusBadRequest, httpx.CodeValidationFailed)

		e.now = e.now.Add(10 * time.Minute) // inside the administrator session's idle limit
		rec := e.post(resolve, obj{"note": "Phoned the customer and passed them to the agency."}, browser(zw))
		wantStatus(t, rec, http.StatusOK)
		r := requestOf(t, rec)
		if r["state"] != "RESOLVED" || r["resolved_at"] != e.now.Format(time.RFC3339) || r["resolution_note"] != "Phoned the customer and passed them to the agency." {
			t.Fatalf("resolved: %s", rec.Body)
		}
		wantError(t, e.post(resolve, nil, browser(super)), http.StatusConflict, propertyrequests.CodeAlreadyResolved)
		if total(super, "") != 0 || total(super, "?state=RESOLVED") != 1 || total(super, "?state=ALL") != 1 {
			t.Fatal("the queue did not move the resolved exception")
		}
		if !strings.Contains(e.logs.String(), `"audit":"routing_exception_resolved"`) {
			t.Fatal("no audit line for the resolution")
		}
	})
}
