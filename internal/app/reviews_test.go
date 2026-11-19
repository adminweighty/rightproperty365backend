package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/reviews"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/httpx"
)

func reviewsPath(book uuid.UUID, rest string) string {
	return "/v1/agent/organisations/" + book.String() + "/reviews" + rest
}

const plumbersReviews = "/v1/directory/service-providers/sample-plumbers/reviews"

type ratingBody struct {
	Average *string `json:"average"`
	Count   int     `json:"count"`
}

type myReviewBody struct {
	ID      string `json:"id"`
	Profile struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"profile"`
	Basis struct {
		Kind      string `json:"kind"`
		ID        string `json:"id"`
		Reference string `json:"reference"`
	} `json:"basis"`
	Rating   int    `json:"rating"`
	Text     string `json:"text"`
	Status   string `json:"status"`
	Reviewer struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"reviewer"`
	Author *struct {
		UserID *string `json:"user_id"`
		Name   string  `json:"name"`
	} `json:"author"`
	Moderation *struct {
		Reason string `json:"reason"`
	} `json:"moderation"`
	Can struct {
		Edit   bool `json:"edit"`
		Delete bool `json:"delete"`
	} `json:"can"`
}

type eligibilityBody struct {
	Profile struct {
		ID string `json:"id"`
	} `json:"profile"`
	Reviewable bool `json:"reviewable"`
	Eligible   bool `json:"eligible"`
	Bases      []struct {
		Kind      string `json:"kind"`
		ID        string `json:"id"`
		Reference string `json:"reference"`
	} `json:"bases"`
	Upcoming []struct {
		Basis struct {
			ID        string `json:"id"`
			Reference string `json:"reference"`
		} `json:"basis"`
		Reason       string     `json:"reason"`
		EligibleFrom *time.Time `json:"eligible_from"`
	} `json:"upcoming"`
	Reviewed []struct {
		Basis struct {
			ID string `json:"id"`
		} `json:"basis"`
		ReviewID *string `json:"review_id"`
		Status   string  `json:"status"`
	} `json:"reviewed"`
}

type adminReviewBody struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Reviewer struct {
		Kind   string  `json:"kind"`
		Name   string  `json:"name"`
		UserID *string `json:"user_id"`
	} `json:"reviewer"`
	Reply *struct {
		Text string `json:"text"`
	} `json:"reply"`
	OpenReports int `json:"open_reports"`
	Reports     *[]struct {
		Status     string  `json:"status"`
		Resolution *string `json:"resolution"`
	} `json:"reports"`
	Can struct {
		Publish     bool `json:"publish"`
		Hide        bool `json:"hide"`
		Reject      bool `json:"reject"`
		RemoveReply bool `json:"remove_reply"`
	} `json:"can"`
}

type publicReviewsBody struct {
	Rating ratingBody `json:"rating"`
	Items  []struct {
		ID       string `json:"id"`
		Rating   int    `json:"rating"`
		Reviewer struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"reviewer"`
		BasisKind string `json:"basis_kind"`
		Reply     *struct {
			Text string `json:"text"`
		} `json:"reply"`
	} `json:"items"`
	Total int `json:"total"`
}

func reviewText(n int) string { return strings.Repeat("a", n) }

func (e *env) plumbersRating(t *testing.T) ratingBody {
	t.Helper()
	return decode[struct {
		Profile struct {
			Rating ratingBody `json:"rating"`
		} `json:"profile"`
	}](t, e.get("/v1/directory/service-providers/sample-plumbers")).Profile.Rating
}

// The seed's published reviews make the rating; the public list shows
// published reviews only, with replies and public names; directory JSON
// carries `rating` on every profile.
func TestReviewsPublicSide(t *testing.T) {
	e := newEnv(t)
	if r := e.plumbersRating(t); r.Average == nil || *r.Average != "4.5" || r.Count != 2 {
		t.Fatalf("plumbers rating: %+v", r)
	}
	agency := decode[struct {
		Profile struct {
			Rating ratingBody `json:"rating"`
		} `json:"profile"`
	}](t, e.get("/v1/directory/agencies/sample-estates")).Profile.Rating
	if agency.Average != nil || agency.Count != 0 {
		t.Fatalf("an agency's rating: %+v", agency)
	}
	if body := e.get("/v1/directory/service-providers").Body.String(); !strings.Contains(body, `"rating":{"average":"4.5","count":2}`) {
		t.Fatalf("the list lacks the rating: %s", body)
	}

	rec := e.get(plumbersReviews)
	wantStatus(t, rec, http.StatusOK)
	if cc := rec.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "public") {
		t.Fatalf("Cache-Control %q", cc)
	}
	page := decode[publicReviewsBody](t, rec)
	if page.Total != 2 || len(page.Items) != 2 || page.Rating.Count != 2 {
		t.Fatalf("public list: %+v", page)
	}
	first, second := page.Items[0], page.Items[1]
	if first.Reviewer.Name != "Casey C." || first.Reviewer.Kind != "CUSTOMER" || first.Reply != nil || first.BasisKind != "ENQUIRY" ||
		second.Reviewer.Name != "Sample Estates" || second.Reviewer.Kind != "ORGANISATION" || second.Reply == nil {
		t.Fatalf("public items: %+v", page.Items)
	}
	for _, banned := range []string{identity.SeedConsumerUserID.String(), "Casey Consumer", "Rudo Moyo", "Grace", "PENDING", "reports"} {
		if strings.Contains(rec.Body.String(), banned) {
			t.Fatalf("public list carries %q", banned)
		}
	}
	if two := decode[publicReviewsBody](t, e.get(plumbersReviews+"?page=2&page_size=1")); len(two.Items) != 1 || two.Items[0].ID != second.ID {
		t.Fatalf("page 2: %+v", two)
	}
	wantError(t, e.get("/v1/directory/service-providers/nobody/reviews"), http.StatusNotFound, reviews.CodeProfileNotFound)
	wantError(t, e.get(plumbersReviews+"?page_size=51"), http.StatusBadRequest, httpx.CodeValidationFailed)
}

// A book reviews a RESOLVED or CLOSED request of its own book assigned to
// the provider; every other request is refused with its reason.
func TestReviewsMaintenanceBasis(t *testing.T) {
	e := newEnv(t)
	org, second := devseed.SampleEstates.ID, devseed.SecondStreetRealty.ID
	plumbers := devseed.PlumbersProfile()
	rudo := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	chipo := browser(e.loginCookie(devseed.EstatesAgent2Email, seedPassword))
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))

	el := decode[eligibilityBody](t, e.get(reviewsPath(org, "/eligibility?kind=service-providers&slug=sample-plumbers"), rudo))
	if !el.Reviewable || el.Eligible || len(el.Bases) != 0 || len(el.Reviewed) != 1 || el.Reviewed[0].Basis.ID != devseed.MaintenanceDrainID.String() ||
		el.Reviewed[0].ReviewID == nil || len(el.Upcoming) != 1 || el.Upcoming[0].Reason != reviews.ReasonNotCompleted ||
		el.Upcoming[0].Basis.Reference != "RPM-2026-900001" || el.Profile.ID != plumbers.ID.String() {
		t.Fatalf("eligibility: %+v", el)
	}
	// Somebody else's book is 404 before anything else.
	wantError(t, e.get(reviewsPath(org, "/eligibility?kind=service-providers&slug=sample-plumbers"), blake), http.StatusNotFound,
		reviews.CodeBookNotFound)
	wantError(t, e.get(reviewsPath(org, ""), blake), http.StatusNotFound, reviews.CodeBookNotFound)

	body := func(basis uuid.UUID, rating int, text string) obj {
		return obj{"directory_profile_id": plumbers.ID.String(), "basis": obj{"kind": "MAINTENANCE", "id": basis.String()},
			"rating": rating, "text": text}
	}
	good := "Turned up on time, fixed the leak and left the kitchen clean."
	wantReason := func(basis uuid.UUID, cookie reqOpt, book uuid.UUID, reason string) {
		t.Helper()
		rec := e.post(reviewsPath(book, ""), body(basis, 5, good), cookie)
		wantError(t, rec, http.StatusConflict, reviews.CodeNotEligible)
		if !strings.Contains(rec.Body.String(), `"reason":"`+reason+`"`) {
			t.Fatalf("want %s: %s", reason, rec.Body.String())
		}
	}
	wantReason(devseed.MaintenanceTapID, rudo, org, reviews.ReasonNotCompleted)    // ASSIGNED: wrong status
	wantReason(devseed.MaintenanceHedgeID, rudo, org, reviews.ReasonWrongProvider) // CLOSED, never assigned to them
	wantReason(uuid.New(), rudo, org, reviews.ReasonBasisNotFound)
	// Another organisation's request: as if unknown, both ways.
	wantReason(devseed.MaintenanceDrainID, blake, second, reviews.ReasonBasisNotFound)
	theirs := maintenance.Request{ID: uuid.New(), Reference: "RPM-2026-990001", Book: tenancies.OrganisationBook(second), PropertyID: uuid.New(),
		Category: "plumbing", CategoryLabel: "Plumbing", Priority: "low", PriorityLabel: "Low", Title: "Theirs", Status: maintenance.StatusClosed,
		RaisedByKind: maintenance.ActorManager, Provider: &maintenance.Provider{ProfileID: &plumbers.ID, Kind: plumbers.Kind, Slug: plumbers.Slug,
			Name: plumbers.Name}, AssignedAt: &e.now, StatusChangedAt: e.now, CreatedAt: e.now, UpdatedAt: e.now}
	if err := e.maintenance.Put(maintenance.Change{Create: &theirs}); err != nil {
		t.Fatal(err)
	}
	wantReason(theirs.ID, rudo, org, reviews.ReasonBasisNotFound)
	// One review per basis: the drain is already reviewed.
	wantError(t, e.post(reviewsPath(org, ""), body(devseed.MaintenanceDrainID, 5, good), rudo), http.StatusConflict, reviews.CodeExists)

	// The tap is resolved: now it may be reviewed.
	tap := devseed.MaintenanceTapID.String()
	for _, to := range []string{"IN_PROGRESS", "RESOLVED"} {
		wantStatus(t, e.post(maintenancePath(org, "/"+tap+"/status"), obj{"status": to}, chipo), http.StatusOK)
	}
	el = decode[eligibilityBody](t, e.get(reviewsPath(org, "/eligibility?directory_profile_id="+plumbers.ID.String()), rudo))
	if !el.Eligible || len(el.Bases) != 1 || el.Bases[0].ID != tap || el.Bases[0].Kind != "MAINTENANCE" || len(el.Upcoming) != 0 {
		t.Fatalf("eligibility after resolving: %+v", el)
	}
	// Bounds and lengths.
	for _, tc := range []struct {
		body  obj
		field string
	}{
		{body(devseed.MaintenanceTapID, 0, good), "rating"},
		{body(devseed.MaintenanceTapID, 6, good), "rating"},
		{body(devseed.MaintenanceTapID, 5, reviewText(19)), "text"},
		{body(devseed.MaintenanceTapID, 5, reviewText(2001)), "text"},
		{body(devseed.MaintenanceTapID, 5, "   "+reviewText(19)+"   "), "text"},
		{obj{"directory_profile_id": plumbers.ID.String(), "basis": obj{"kind": "ENQUIRY", "id": tap}, "rating": 5, "text": good}, "basis.kind"},
		{obj{"directory_profile_id": devseed.SampleEstates.ID.String(), "basis": obj{"kind": "MAINTENANCE", "id": tap}, "rating": 5, "text": good},
			"directory_profile_id"},
	} {
		env := wantError(t, e.post(reviewsPath(org, ""), tc.body, rudo), http.StatusBadRequest, httpx.CodeValidationFailed)
		if _, ok := env.Error.Fields[tc.field]; !ok {
			t.Fatalf("want field %s: %+v", tc.field, env.Error.Fields)
		}
	}
	wantError(t, e.post(reviewsPath(org, ""), obj{"directory_profile_id": plumbers.ID.String(), "basis": obj{"kind": "MAINTENANCE", "id": tap},
		"rating": 4.5, "text": good}, rudo), http.StatusBadRequest, httpx.CodeInvalidJSON)
	wantError(t, e.post(reviewsPath(org, ""), body(devseed.MaintenanceTapID, 5, good), staff), http.StatusForbidden, httpx.CodeForbidden)
	// 20 and 2000 characters are fine.
	rec := e.post(reviewsPath(org, ""), body(devseed.MaintenanceTapID, 5, reviewText(20)), rudo)
	wantStatus(t, rec, http.StatusCreated)
	created := decode[struct {
		Review myReviewBody `json:"review"`
	}](t, rec).Review
	if created.Status != "PENDING" || created.Reviewer.Name != "Sample Estates" || created.Reviewer.Kind != "ORGANISATION" ||
		created.Author == nil || created.Author.Name != "Rudo Moyo" || !created.Can.Edit || created.Basis.Reference != "RPM-2026-900001" {
		t.Fatalf("created: %+v", created)
	}
	wantError(t, e.post(reviewsPath(org, ""), body(devseed.MaintenanceTapID, 4, good), chipo), http.StatusConflict, reviews.CodeExists)
	edited := decode[struct {
		Review myReviewBody `json:"review"`
	}](t, e.patch(reviewsPath(org, "/"+created.ID), obj{"rating": 4, "text": reviewText(2000)}, chipo)).Review
	if edited.Rating != 4 || len(edited.Text) != 2000 {
		t.Fatalf("edited: %+v", edited)
	}
	// STAFF reads, cannot edit; another book does not see it.
	list := decode[struct {
		Items       []myReviewBody `json:"items"`
		Permissions struct {
			Write bool `json:"write"`
		} `json:"permissions"`
	}](t, e.get(reviewsPath(org, ""), staff))
	if len(list.Items) != 2 || list.Permissions.Write || list.Items[0].Can.Edit {
		t.Fatalf("staff list: %+v", list)
	}
	wantError(t, e.patch(reviewsPath(org, "/"+created.ID), obj{"rating": 3}, staff), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get(reviewsPath(second, "/"+created.ID), blake), http.StatusNotFound, reviews.CodeNotFound)
	wantError(t, e.deleteReq(reviewsPath(second, "/"+created.ID), blake), http.StatusNotFound, reviews.CodeNotFound)
	// Pending reviews are not public and do not count.
	if r := e.plumbersRating(t); r.Count != 2 {
		t.Fatalf("a pending review counted: %+v", r)
	}

	// Once moderated it is locked; it may still be deleted.
	mod := browser(e.adminLogin(devseed.ModeratorEmail, seedPassword))
	wantStatus(t, e.post("/v1/admin/reviews/"+created.ID+"/publish", obj{}, mod), http.StatusOK)
	wantError(t, e.patch(reviewsPath(org, "/"+created.ID), obj{"rating": 3}, rudo), http.StatusConflict, reviews.CodeLocked)
	if r := e.plumbersRating(t); r.Count != 3 || r.Average == nil || *r.Average != "4.3" {
		t.Fatalf("rating after publishing a 4: %+v", r)
	}
	wantStatus(t, e.deleteReq(reviewsPath(org, "/"+created.ID), rudo), http.StatusNoContent)
	wantError(t, e.get(reviewsPath(org, "/"+created.ID), rudo), http.StatusNotFound, reviews.CodeNotFound)
	if r := e.plumbersRating(t); r.Count != 2 {
		t.Fatalf("rating after the delete: %+v", r)
	}
}

// putEnquiry stores an enquiry to a profile as if sent some days ago.
func (e *env) putEnquiry(t *testing.T, customer uuid.UUID, profile devseed.Profile, daysAgo int, replied bool, version string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	created := e.now.AddDate(0, 0, -daysAgo)
	enq := businessenquiries.NewEnquiry{ID: id, Reference: "BE-T" + strings.ToUpper(id.String()[:7]), MarketID: devseed.MarketID("ZW"),
		ProfileID: profile.ID, ProfileKind: profile.Kind, ProfileSlug: profile.Slug, ProfileName: profile.Name,
		Status: businessenquiries.StatusReceived, Message: "Test", PreferredContactMethod: "EMAIL", IdempotencyKey: id.String(),
		RequestHash: "x", CustomerUserID: &customer, GuestName: "Test", GuestEmail: "test@example.test", PrivacyVersion: version,
		ConsentAcceptedAt: created, Recipient: &businessenquiries.Recipient{OrganisationID: profile.Organisation.ID}, CreatedAt: created}
	if replied {
		at := created.Add(time.Hour)
		enq.RepliedAt = &at
	}
	if _, _, err := e.enquiries.Create(context.Background(), enq); err != nil {
		t.Fatal(err)
	}
	return id
}

func profileBySlug(t *testing.T, slug string) devseed.Profile {
	t.Helper()
	for _, p := range devseed.Profiles() {
		if p.Slug == slug {
			return p
		}
	}
	t.Fatalf("no profile %s", slug)
	return devseed.Profile{}
}

// A customer reviews an enquiry they sent at least 14 days ago that the
// provider marked replied; every other enquiry is refused with its reason.
func TestReviewsEnquiryBasis(t *testing.T) {
	e := newEnv(t)
	plumbers, electricians := devseed.PlumbersProfile(), profileBySlug(t, "sample-electricians")
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	owner := browser(e.loginCookie(devseed.BusinessOwnerEmail(plumbers.Slug), seedPassword))
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))

	ok := e.putEnquiry(t, devseed.TenantID, plumbers, 20, true, "2026-01")
	recent := e.putEnquiry(t, devseed.TenantID, plumbers, 10, true, "2026-01")
	unreplied := e.putEnquiry(t, devseed.TenantID, plumbers, 30, false, "2026-01")
	elsewhere := e.putEnquiry(t, devseed.TenantID, electricians, 30, true, "2026-01")
	someoneElse := e.putEnquiry(t, identity.SeedConsumerUserID, plumbers, 30, true, "2026-01")
	assignment := e.putEnquiry(t, devseed.TenantID, plumbers, 30, true, maintenance.DefaultEnquiryPrivacyVer)

	el := decode[eligibilityBody](t, e.get("/v1/me/reviews/eligibility?kind=service-providers&slug=sample-plumbers", tenant))
	if !el.Eligible || len(el.Bases) != 1 || el.Bases[0].ID != ok.String() || len(el.Upcoming) != 2 || len(el.Reviewed) != 0 {
		t.Fatalf("eligibility: %+v", el)
	}
	reasons := map[string]string{}
	for _, u := range el.Upcoming {
		reasons[u.Basis.ID] = u.Reason
		if u.Reason == reviews.ReasonTooRecent && (u.EligibleFrom == nil || !u.EligibleFrom.Equal(e.now.AddDate(0, 0, 4))) {
			t.Fatalf("eligible_from: %+v", u)
		}
	}
	if reasons[recent.String()] != reviews.ReasonTooRecent || reasons[unreplied.String()] != reviews.ReasonNotReplied {
		t.Fatalf("upcoming: %+v", el.Upcoming)
	}
	body := func(basis uuid.UUID, profile uuid.UUID) obj {
		return obj{"directory_profile_id": profile.String(), "basis": obj{"kind": "ENQUIRY", "id": basis.String()}, "rating": 4,
			"text": "Answered the same day and the quote was clear."}
	}
	for basis, reason := range map[uuid.UUID]string{recent: reviews.ReasonTooRecent, unreplied: reviews.ReasonNotReplied,
		elsewhere: reviews.ReasonWrongProvider, someoneElse: reviews.ReasonBasisNotFound, assignment: reviews.ReasonBasisNotFound} {
		rec := e.post("/v1/me/reviews", body(basis, plumbers.ID), tenant)
		wantError(t, rec, http.StatusConflict, reviews.CodeNotEligible)
		if !strings.Contains(rec.Body.String(), `"reason":"`+reason+`"`) {
			t.Fatalf("%s: want %s: %s", basis, reason, rec.Body.String())
		}
	}
	env := wantError(t, e.post("/v1/me/reviews", obj{"directory_profile_id": plumbers.ID.String(),
		"basis": obj{"kind": "MAINTENANCE", "id": devseed.MaintenanceDrainID.String()}, "rating": 4, "text": reviewText(30)}, tenant),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	if _, has := env.Error.Fields["basis.kind"]; !has {
		t.Fatalf("fields: %+v", env.Error.Fields)
	}
	// A provider does not review itself.
	env = wantError(t, e.post("/v1/me/reviews", body(ok, plumbers.ID), owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	if _, has := env.Error.Fields["directory_profile_id"]; !has {
		t.Fatalf("fields: %+v", env.Error.Fields)
	}
	wantError(t, e.post("/v1/me/reviews", body(ok, plumbers.ID)), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	rec := e.post("/v1/me/reviews", body(ok, plumbers.ID), tenant)
	wantStatus(t, rec, http.StatusCreated)
	mine := decode[struct {
		Review myReviewBody `json:"review"`
	}](t, rec).Review
	if mine.Reviewer.Name != "Tariro T." || mine.Reviewer.Kind != "CUSTOMER" || mine.Author != nil || mine.Status != "PENDING" {
		t.Fatalf("created: %+v", mine)
	}
	wantError(t, e.post("/v1/me/reviews", body(ok, plumbers.ID), tenant), http.StatusConflict, reviews.CodeExists)
	if list := decode[struct {
		Items []myReviewBody `json:"items"`
	}](t, e.get("/v1/me/reviews", tenant)); len(list.Items) != 1 || list.Items[0].ID != mine.ID {
		t.Fatalf("my reviews: %+v", list.Items)
	}
	// Never anonymous to the platform: the user ID is stored.
	stored, err := e.reviews.Review(context.Background(), uuid.MustParse(mine.ID))
	if err != nil || stored.ReviewerUserID == nil || *stored.ReviewerUserID != devseed.TenantID {
		t.Fatalf("stored reviewer: %+v %v", stored.ReviewerUserID, err)
	}
	// Somebody else's review is 404 to the tenant routes.
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	wantError(t, e.get("/v1/me/reviews/"+mine.ID, consumer), http.StatusNotFound, reviews.CodeNotFound)
	wantError(t, e.patch("/v1/me/reviews/"+mine.ID, obj{"rating": 1}, consumer), http.StatusNotFound, reviews.CodeNotFound)
	wantStatus(t, e.patch("/v1/me/reviews/"+mine.ID, obj{"rating": 5}, tenant), http.StatusOK)

	// Marking an enquiry replied: the provider's inbox only.
	rec = e.post("/v1/business-enquiries", obj{"recipient": obj{"kind": "service-providers", "slug": "sample-plumbers"},
		"message": "Do you service geysers?", "preferred_contact_method": "EMAIL", "idempotency_key": "review-test-0001",
		"consent": obj{"privacy_version": "2026-01", "accepted": true}}, tenant)
	wantStatus(t, rec, http.StatusCreated)
	sent := decode[struct {
		EnquiryID string `json:"enquiry_id"`
	}](t, rec).EnquiryID
	wantError(t, e.post("/v1/inbox/business-enquiries/"+sent+"/replied", nil, blake), http.StatusNotFound, businessenquiries.CodeEnquiryNotFound)
	item := decode[struct {
		Item struct {
			RepliedAt *time.Time `json:"replied_at"`
		} `json:"item"`
	}](t, e.post("/v1/inbox/business-enquiries/"+sent+"/replied", nil, owner)).Item
	if item.RepliedAt == nil || !item.RepliedAt.Equal(e.now) {
		t.Fatalf("replied_at: %+v", item.RepliedAt)
	}
	e.now = e.now.Add(time.Hour)
	again := decode[struct {
		Item struct {
			RepliedAt *time.Time `json:"replied_at"`
		} `json:"item"`
	}](t, e.post("/v1/inbox/business-enquiries/"+sent+"/replied", nil, owner)).Item
	if again.RepliedAt == nil || !again.RepliedAt.Equal(*item.RepliedAt) {
		t.Fatal("a second mark changed replied_at")
	}
	el = decode[eligibilityBody](t, e.get("/v1/me/reviews/eligibility?directory_profile_id="+plumbers.ID.String(), tenant))
	found := false
	for _, u := range el.Upcoming {
		found = found || (u.Basis.ID == sent && u.Reason == reviews.ReasonTooRecent)
	}
	if !found || len(el.Reviewed) != 1 {
		t.Fatalf("the replied enquiry is not upcoming: %+v", el)
	}
}

// The provider replies once, published directly, editable for 7 days;
// members read, owners and managers reply; other organisations get 404.
func TestReviewsProviderReply(t *testing.T) {
	e := newEnv(t)
	plumbers := devseed.PlumbersProfile()
	org := plumbers.Organisation.ID
	owner := browser(e.loginCookie(devseed.BusinessOwnerEmail(plumbers.Slug), seedPassword))
	rudo := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))

	type receivedBody struct {
		Items []struct {
			ID                 string     `json:"id"`
			ReplyEditableUntil *time.Time `json:"reply_editable_until"`
			Reply              *struct {
				Text string `json:"text"`
			} `json:"reply"`
			Can struct {
				Reply     bool `json:"reply"`
				EditReply bool `json:"edit_reply"`
			} `json:"can"`
		} `json:"items"`
		Total int `json:"total"`
	}
	got := decode[receivedBody](t, e.get(reviewsPath(org, "/received"), owner))
	if got.Total != 2 || got.Items[0].ID != devseed.ReviewConsumerID.String() || !got.Items[0].Can.Reply ||
		got.Items[1].Reply == nil || got.Items[1].Can.EditReply || got.Items[1].Can.Reply {
		t.Fatalf("received: %+v", got)
	}
	wantError(t, e.get(reviewsPath(org, "/received"), rudo), http.StatusNotFound, reviews.CodeBookNotFound)
	reply := func(id uuid.UUID) string { return reviewsPath(org, "/"+id.String()+"/reply") }
	wantError(t, e.post(reply(devseed.ReviewConsumerID), obj{"text": "Thanks"}, rudo), http.StatusNotFound, reviews.CodeBookNotFound)
	// The seeded reply is 21 days old: locked; and there is only one.
	rec := e.patch(reply(devseed.ReviewEstatesID), obj{"text": "Edited"}, owner)
	wantError(t, rec, http.StatusConflict, reviews.CodeReplyLocked)
	if !strings.Contains(rec.Body.String(), `"editable_until"`) {
		t.Fatalf("no editable_until: %s", rec.Body.String())
	}
	wantError(t, e.post(reply(devseed.ReviewEstatesID), obj{"text": "Again"}, owner), http.StatusConflict, reviews.CodeReplyExists)
	// A pending review is not theirs to see.
	wantError(t, e.post(reply(devseed.ReviewSellerID), obj{"text": "Thanks"}, owner), http.StatusNotFound, reviews.CodeNotFound)
	wantError(t, e.patch(reply(devseed.ReviewConsumerID), obj{"text": "No reply yet"}, owner), http.StatusNotFound, reviews.CodeReplyNotFound)
	wantError(t, e.post(reply(devseed.ReviewConsumerID), obj{"text": "  "}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)

	wantStatus(t, e.post(reply(devseed.ReviewConsumerID), obj{"text": "Thank you, Casey."}, owner), http.StatusCreated)
	e.now = e.now.Add(7*24*time.Hour - time.Minute)
	owner = browser(e.loginCookie(devseed.BusinessOwnerEmail(plumbers.Slug), seedPassword))
	wantStatus(t, e.patch(reply(devseed.ReviewConsumerID), obj{"text": "Thank you, Casey. Sorry we were late."}, owner), http.StatusOK)
	e.now = e.now.Add(2 * time.Minute)
	wantError(t, e.patch(reply(devseed.ReviewConsumerID), obj{"text": "Too late"}, owner), http.StatusConflict, reviews.CodeReplyLocked)
	page := decode[publicReviewsBody](t, e.get(plumbersReviews))
	if page.Items[0].Reply == nil || page.Items[0].Reply.Text != "Thank you, Casey. Sorry we were late." {
		t.Fatalf("the reply is not public: %+v", page.Items[0])
	}
}

// Reports: anybody signed in, published reviews only, one open report per
// person; the review stays visible.
func TestReviewsReports(t *testing.T) {
	e := newEnv(t)
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	report := func(id uuid.UUID) string { return "/v1/reviews/" + id.String() + "/report" }
	wantError(t, e.post(report(devseed.ReviewEstatesID), obj{"reason": "Spam"}), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	// The seed's open report is the tenant's.
	wantError(t, e.post(report(devseed.ReviewConsumerID), obj{"reason": "Again"}, tenant), http.StatusConflict, reviews.CodeAlreadyReported)
	wantError(t, e.post(report(devseed.ReviewSellerID), obj{"reason": "Pending"}, tenant), http.StatusNotFound, reviews.CodeNotFound)
	wantError(t, e.post(report(devseed.ReviewEstatesID), obj{"reason": ""}, consumer), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantStatus(t, e.post(report(devseed.ReviewEstatesID), obj{"reason": "Not about this provider"}, consumer), http.StatusCreated)
	wantError(t, e.post(report(devseed.ReviewEstatesID), obj{"reason": "Twice"}, consumer), http.StatusConflict, reviews.CodeAlreadyReported)
	if page := decode[publicReviewsBody](t, e.get(plumbersReviews)); page.Total != 2 {
		t.Fatalf("a reported review left the public list: %d", page.Total)
	}
}

// Moderation: the queue (new and reported), publish / hide / reject /
// remove the reply with a reason, the state machine, the admin audit and
// the notices; only published reviews count.
func TestReviewsModeration(t *testing.T) {
	e := newEnv(t)
	mod := browser(e.adminLogin(devseed.ModeratorEmail, seedPassword))
	content := browser(e.adminLogin(devseed.ContentAdminEmail, seedPassword))
	support := browser(e.adminLogin(devseed.SupportEmail, seedPassword))
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	wantError(t, e.get("/v1/admin/reviews", support), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get("/v1/admin/reviews", tenant), http.StatusForbidden, httpx.CodeForbidden)

	type listBody struct {
		Items  []adminReviewBody `json:"items"`
		Total  int               `json:"total"`
		Counts struct {
			Pending  int `json:"pending"`
			Reported int `json:"reported"`
		} `json:"counts"`
	}
	q := decode[listBody](t, e.get("/v1/admin/reviews", mod))
	if q.Total != 2 || q.Items[0].ID != devseed.ReviewConsumerID.String() || q.Items[0].OpenReports != 1 ||
		q.Items[1].ID != devseed.ReviewSellerID.String() || q.Counts.Pending != 1 || q.Counts.Reported != 1 || q.Items[0].Reports != nil {
		t.Fatalf("queue: %+v", q)
	}
	if all := decode[listBody](t, e.get("/v1/admin/reviews?view=all&basis_kind=ENQUIRY", mod)); all.Total != 2 {
		t.Fatalf("all enquiry reviews: %d", all.Total)
	}
	if r := decode[listBody](t, e.get("/v1/admin/reviews?reported=true", mod)); r.Total != 1 {
		t.Fatalf("reported: %d", r.Total)
	}
	if r := decode[listBody](t, e.get("/v1/admin/reviews?view=all&status=PUBLISHED&rating=5", mod)); r.Total != 1 {
		t.Fatalf("published 5 stars: %d", r.Total)
	}
	wantError(t, e.get("/v1/admin/reviews?status=GONE", mod), http.StatusBadRequest, httpx.CodeValidationFailed)

	act := func(id uuid.UUID, action string, reason string, who reqOpt) adminReviewBody {
		t.Helper()
		rec := e.post("/v1/admin/reviews/"+id.String()+"/"+action, obj{"reason": reason}, who)
		wantStatus(t, rec, http.StatusOK)
		return decode[struct {
			Review adminReviewBody `json:"review"`
		}](t, rec).Review
	}
	notices := func() int { return len(e.reviews.Outbox()) }
	audits := func(action string) int {
		n := 0
		for _, ev := range e.audit.All() {
			if ev.Action == action && ev.TargetType == adminaudit.TargetProviderReview {
				n++
			}
		}
		return n
	}

	// Reject the pending one: a reason is required.
	seller := "/v1/admin/reviews/" + devseed.ReviewSellerID.String()
	wantError(t, e.post(seller+"/reject", obj{"reason": " "}, mod), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.post(seller+"/hide", obj{"reason": "x"}, mod), http.StatusConflict, reviews.CodeInvalidTransition)
	r := act(devseed.ReviewSellerID, "reject", "Mentions a person by name", mod)
	if r.Status != "REJECTED" || r.Can.Publish || r.Can.Reject || r.Can.Hide || audits(adminaudit.ActionReviewRejected) != 1 || notices() != 1 {
		t.Fatalf("rejected: %+v, %d notices", r, notices())
	}
	wantError(t, e.post(seller+"/publish", obj{}, mod), http.StatusConflict, reviews.CodeInvalidTransition)

	// The reported one: publishing keeps it and closes the report, without a notice.
	r = act(devseed.ReviewConsumerID, "publish", "", content)
	if r.Status != "PUBLISHED" || r.OpenReports != 0 || r.Reports == nil || len(*r.Reports) != 1 || (*r.Reports)[0].Status != "RESOLVED" ||
		(*r.Reports)[0].Resolution == nil || *(*r.Reports)[0].Resolution != "PUBLISH" || notices() != 1 || audits(adminaudit.ActionReviewPublished) != 1 {
		t.Fatalf("kept: %+v, %d notices", r, notices())
	}
	if q := decode[listBody](t, e.get("/v1/admin/reviews", mod)); q.Total != 0 || q.Counts.Pending != 0 || q.Counts.Reported != 0 {
		t.Fatalf("queue after acting: %+v", q)
	}
	wantError(t, e.post("/v1/admin/reviews/"+devseed.ReviewConsumerID.String()+"/publish", obj{}, mod), http.StatusConflict,
		reviews.CodeInvalidTransition)

	// Hiding takes it out of the rating; publishing again is not a first publication.
	r = act(devseed.ReviewEstatesID, "hide", "Under investigation", mod)
	if r.Status != "HIDDEN" || !r.Can.Publish || notices() != 2 {
		t.Fatalf("hidden: %+v", r)
	}
	if rt := e.plumbersRating(t); rt.Count != 1 || rt.Average == nil || *rt.Average != "4.0" {
		t.Fatalf("rating with one hidden: %+v", rt)
	}
	if page := decode[publicReviewsBody](t, e.get(plumbersReviews)); page.Total != 1 {
		t.Fatalf("a hidden review is public: %d", page.Total)
	}
	act(devseed.ReviewEstatesID, "publish", "Cleared", mod)
	out := e.reviews.Outbox()
	if len(out) != 3 || strings.Contains(string(out[2].Payload), `"first_publish":true`) {
		t.Fatalf("republish notice: %s", out[len(out)-1].Payload)
	}
	// Removing the reply: audited, no notice; the provider cannot reply again.
	r = act(devseed.ReviewEstatesID, "remove-reply", "Contains a phone number", mod)
	if r.Reply != nil || r.Can.RemoveReply || audits(adminaudit.ActionReviewReplyRemoved) != 1 || notices() != 3 {
		t.Fatalf("reply removed: %+v", r)
	}
	plumbers := devseed.PlumbersProfile()
	owner := browser(e.loginCookie(devseed.BusinessOwnerEmail(plumbers.Slug), seedPassword))
	wantError(t, e.post(reviewsPath(plumbers.Organisation.ID, "/"+devseed.ReviewEstatesID.String()+"/reply"), obj{"text": "Again"}, owner),
		http.StatusConflict, reviews.CodeReplyLocked)
	// A first publication tells the provider.
	pending := decode[struct {
		Review myReviewBody `json:"review"`
	}](t, e.post("/v1/me/reviews", obj{"directory_profile_id": plumbers.ID.String(),
		"basis":  obj{"kind": "ENQUIRY", "id": e.putEnquiry(t, devseed.TenantID, plumbers, 15, true, "2026-01").String()},
		"rating": 2, "text": "Slow to answer and the price went up on the day."}, tenant)).Review
	act(uuid.MustParse(pending.ID), "publish", "", mod)
	out = e.reviews.Outbox()
	if len(out) != 4 || !strings.Contains(string(out[3].Payload), `"first_publish":true`) {
		t.Fatalf("first publication notice: %s", out[len(out)-1].Payload)
	}
	if rt := e.plumbersRating(t); rt.Count != 3 || *rt.Average != "3.7" {
		t.Fatalf("rating: %+v", rt)
	}
	detail := decode[struct {
		Review adminReviewBody `json:"review"`
	}](t, e.get("/v1/admin/reviews/"+pending.ID, mod)).Review
	if detail.Reviewer.UserID == nil || *detail.Reviewer.UserID != devseed.TenantID.String() || detail.Reports == nil {
		t.Fatalf("detail: %+v", detail)
	}
	wantError(t, e.get("/v1/admin/reviews/"+uuid.NewString(), mod), http.StatusNotFound, reviews.CodeNotFound)
}

// A deleted customer's reviews stay, as "Former customer", unlinked.
func TestReviewsAccountDeletion(t *testing.T) {
	e := newEnv(t)
	cookie := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	wantStatus(t, e.del("/v1/me", obj{"password": seedPassword}, cookie), http.StatusOK)
	page := decode[publicReviewsBody](t, e.get(plumbersReviews))
	if page.Total != 2 || page.Items[0].Reviewer.Name != reviews.FormerCustomer || page.Items[0].Reviewer.Kind != "CUSTOMER" {
		t.Fatalf("after deletion: %+v", page.Items)
	}
	stored, err := e.reviews.Review(context.Background(), devseed.ReviewConsumerID)
	if err != nil || stored.ReviewerUserID != nil || stored.Rating != 4 {
		t.Fatalf("stored: %+v %v", stored, err)
	}
	// The tenant's report is unlinked when the tenant goes.
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	wantStatus(t, e.del("/v1/me", obj{"password": seedPassword}, tenant), http.StatusOK)
	reports, _ := e.reviews.Reports(context.Background(), devseed.ReviewConsumerID)
	if len(reports) != 1 || reports[0].ReporterUserID != nil || reports[0].Status != reviews.ReportOpen {
		t.Fatalf("reports: %+v", reports)
	}
	if r := e.plumbersRating(t); r.Count != 2 {
		t.Fatalf("rating: %+v", r)
	}
}
