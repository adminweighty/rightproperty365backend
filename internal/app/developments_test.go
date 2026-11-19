package app

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/developments"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/storage"
)

// The development and investment showcase (API 0.44.0, BR-019 / FR-048).

type devView struct {
	ID               string   `json:"id"`
	Slug             string   `json:"slug"`
	Name             string   `json:"name"`
	ModerationStatus string   `json:"moderation_status"`
	ReviewReason     *string  `json:"review_reason"`
	PublicURLPath    *string  `json:"public_url_path"`
	Actions          []string `json:"actions"`
	Phases           []struct {
		ID        string `json:"id"`
		UnitTypes []struct {
			ID string `json:"id"`
		} `json:"unit_types"`
	} `json:"phases"`
	Photos []struct {
		ID       string `json:"id"`
		Approved bool   `json:"approved"`
	} `json:"photos"`
	LinkedListings []struct {
		ListingID string `json:"listing_id"`
		Public    bool   `json:"public"`
	} `json:"linked_listings"`
}

func devOf(t *testing.T, rec *httptest.ResponseRecorder, status int) devView {
	t.Helper()
	wantStatus(t, rec, status)
	return decode[struct {
		Development devView `json:"development"`
	}](t, rec).Development
}

type publicDev struct {
	Development struct {
		Slug   string `json:"slug"`
		Name   string `json:"name"`
		Phases []struct {
			UnitTypes []struct {
				ID                    string `json:"id"`
				Name                  string `json:"name"`
				AvailableListingCount int    `json:"available_listing_count"`
			} `json:"unit_types"`
		} `json:"phases"`
		AvailableUnits []struct {
			UnitTypeID *string `json:"unit_type_id"`
			Listing    struct {
				Slug string `json:"slug"`
			} `json:"listing"`
		} `json:"available_units"`
		InvestmentHighlights struct {
			StatedBy   string `json:"stated_by"`
			Disclaimer string `json:"disclaimer"`
		} `json:"investment_highlights"`
		Developer struct {
			Slug     string `json:"slug"`
			Verified bool   `json:"verified"`
		} `json:"developer"`
	} `json:"development"`
}

// seedDevelopments puts the two sample projects (and their linked listing).
func (e *env) seedDevelopments() {
	devseed.SeedMemoryDevelopments(e.developments, e.listings, e.now)
}

func devOrg() uuid.UUID { return devseed.DevelopmentOrganisationID() }

func devPath(org uuid.UUID, rest string) string {
	return "/v1/agent/organisations/" + org.String() + "/developments" + rest
}

func (e *env) developer() reqOpt {
	return browser(e.loginCookie(devseed.DevelopmentOwnerEmail(), seedPassword))
}

// uploadDevPhoto sends a development photo, each from its own address.
func (e *env) uploadDevPhoto(path string, content []byte, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "photo.jpg")
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = part.Write(content)
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	e.seq++
	req.RemoteAddr = fmt.Sprintf("198.52.%d.%d:40000", 100+e.seq/250, e.seq%250+1)
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// completeDevelopment creates a project ready to be submitted: details, a
// phase with a unit type, and a described photo.
func (e *env) completeDevelopment(t *testing.T, as reqOpt, name string) devView {
	t.Helper()
	org := devOrg()
	d := devOf(t, e.post(devPath(org, ""), obj{"name": name, "summary": "Townhouses near the shops.",
		"description": "A sample project of townhouses.\nWith gardens.", "status": "SELLING", "city": "harare", "suburb": "borrowdale",
		"completion_estimate": "2027-Q3", "amenities": []string{"clubhouse", "gated-community"},
		"investment_highlights": []obj{{"label": "Expected rental yield", "value": "7% a year"}}}, as), http.StatusCreated)
	d = devOf(t, e.post(devPath(org, "/"+d.ID+"/phases"), obj{"name": "Phase 1", "status": "SELLING"}, as), http.StatusCreated)
	d = devOf(t, e.post(devPath(org, "/"+d.ID+"/phases/"+d.Phases[0].ID+"/unit-types"), obj{"name": "Three-bedroom townhouse",
		"bedrooms": 3, "bathrooms": 2, "size_min_m2": 150, "size_max_m2": 170, "price_from": "150000", "price_to": "175000.5",
		"currency": "USD", "units_total": 10, "units_available": 4}, as), http.StatusCreated)
	d = devOf(t, e.uploadDevPhoto(devPath(org, "/"+d.ID+"/photos"), testPhoto(t, 1200, 900), as), http.StatusCreated)
	return devOf(t, e.patch(devPath(org, "/"+d.ID+"/photos/"+d.Photos[0].ID), obj{"alt": "Front of the show house"}, as), http.StatusOK)
}

func TestDevelopmentModerationFlow(t *testing.T) {
	e := newEnv(t)
	e.seedDevelopments()
	owner := e.developer()
	org := devOrg()
	d := e.completeDevelopment(t, owner, "Greendale Mews")
	if d.ModerationStatus != "DRAFT" || d.Slug != "greendale-mews" || d.PublicURLPath != nil || len(d.Photos) != 1 || d.Photos[0].Approved {
		t.Fatalf("draft: %+v", d)
	}
	public := "/v1/developments/zw/greendale-mews"
	wantError(t, e.get(public), http.StatusNotFound, developments.CodeNotFound)

	// Incomplete projects cannot be submitted.
	bare := devOf(t, e.post(devPath(org, ""), obj{"name": "Bare plot"}, owner), http.StatusCreated)
	env := wantError(t, e.post(devPath(org, "/"+bare.ID+"/submit"), nil, owner), http.StatusUnprocessableEntity, developments.CodeIncomplete)
	if env.Error.Fields["photos"] == "" || env.Error.Fields["phases"] == "" || env.Error.Fields["description"] == "" {
		t.Fatalf("missing: %v", env.Error.Fields)
	}

	d = devOf(t, e.post(devPath(org, "/"+d.ID+"/submit"), nil, owner), http.StatusOK)
	if d.ModerationStatus != "PENDING_REVIEW" || !strings.Contains(strings.Join(d.Actions, ","), "withdraw") {
		t.Fatalf("submitted: %+v", d)
	}
	wantError(t, e.get(public), http.StatusNotFound, developments.CodeNotFound)
	// Locked while waiting: details and photos; phases still change.
	wantError(t, e.patch(devPath(org, "/"+d.ID), obj{"summary": "Changed"}, owner), http.StatusConflict, developments.CodeLocked)
	wantError(t, e.uploadDevPhoto(devPath(org, "/"+d.ID+"/photos"), testPhoto(t, 1200, 900), owner), http.StatusConflict, developments.CodeLocked)

	// The review queue: the listing reviewers; a customer session is refused.
	moderator := e.asAdmin(devseed.ModeratorEmail)
	wantStatus(t, e.get("/v1/admin/developments", owner), http.StatusForbidden)
	queue := decode[struct {
		Items []devView `json:"items"`
		Total int       `json:"total"`
	}](t, e.get("/v1/admin/developments", moderator))
	if queue.Total != 1 || queue.Items[0].ID != d.ID {
		t.Fatalf("queue: %+v", queue)
	}
	wantError(t, e.post("/v1/admin/developments/"+d.ID+"/reject", obj{"reason": "  "}, moderator), http.StatusBadRequest, httpx.CodeValidationFailed)
	d = devOf(t, e.post("/v1/admin/developments/"+d.ID+"/approve", nil, moderator), http.StatusOK)
	if d.ModerationStatus != "PUBLISHED" || !d.Photos[0].Approved {
		t.Fatalf("approved: %+v", d)
	}
	wantError(t, e.post("/v1/admin/developments/"+d.ID+"/approve", nil, moderator), http.StatusConflict, developments.CodeStateConflict)
	pub := decode[publicDev](t, e.get(public))
	if pub.Development.Name != "Greendale Mews" || pub.Development.Developer.Slug != devseed.SampleDevelopersSlug || !pub.Development.Developer.Verified {
		t.Fatalf("public: %+v", pub)
	}
	if !strings.Contains(e.get("/v1/developments?market=ZW&q=greendale").Body.String(), `"slug":"greendale-mews"`) {
		t.Fatal("the published project is listed")
	}
	wantError(t, e.patch(devPath(org, "/"+d.ID), obj{"summary": "Changed"}, owner), http.StatusConflict, developments.CodeLocked)

	// Withdrawn: no longer public. Resubmitted and rejected, with the reason.
	d = devOf(t, e.post(devPath(org, "/"+d.ID+"/withdraw"), nil, owner), http.StatusOK)
	wantError(t, e.get(public), http.StatusNotFound, developments.CodeNotFound)
	d = devOf(t, e.patch(devPath(org, "/"+d.ID), obj{"summary": "Changed after withdrawing."}, owner), http.StatusOK)
	d = devOf(t, e.post(devPath(org, "/"+d.ID+"/submit"), nil, owner), http.StatusOK)
	d = devOf(t, e.post("/v1/admin/developments/"+d.ID+"/reject", obj{"reason": "The photo does not show the project."}, moderator), http.StatusOK)
	if d.ModerationStatus != "REJECTED" || d.ReviewReason == nil || *d.ReviewReason != "The photo does not show the project." {
		t.Fatalf("rejected: %+v", d)
	}
	wantError(t, e.get(public), http.StatusNotFound, developments.CodeNotFound)

	// Both decisions are in the administrators' audit, with the reason.
	var approved, rejected bool
	for _, ev := range e.audit.All() {
		if ev.TargetType == developments.TargetDevelopment && ev.TargetID == d.ID {
			approved = approved || ev.Action == developments.ActionApproved
			rejected = rejected || (ev.Action == developments.ActionRejected && ev.Reason == "The photo does not show the project.")
		}
	}
	if !approved || !rejected {
		t.Fatalf("audit: approved %v rejected %v (%+v)", approved, rejected, e.audit.All())
	}
	audit := decode[struct {
		Items []struct {
			Action string `json:"action"`
		} `json:"items"`
	}](t, e.get("/v1/admin/audit?target_type=DEVELOPMENT", e.asAdmin(identity.SeedAdminEmail)))
	if len(audit.Items) < 2 {
		t.Fatalf("admin audit API: %+v", audit)
	}
}

func TestDevelopmentOnlyPublishedArePublic(t *testing.T) {
	e := newEnv(t)
	e.seedDevelopments()
	list := e.get("/v1/developments")
	wantStatus(t, list, http.StatusOK)
	body := list.Body.String()
	if !strings.Contains(body, `"slug":"borrowdale-heights"`) || strings.Contains(body, devseed.RuwaGardensSlug) || !strings.Contains(body, `"total":1`) {
		t.Fatalf("list: %s", body)
	}
	if list.Header().Get("Cache-Control") == "" || list.Header().Get("ETag") == "" {
		t.Fatal("the list is cacheable")
	}
	wantError(t, e.get("/v1/developments/zw/"+devseed.RuwaGardensSlug), http.StatusNotFound, developments.CodeNotFound)
	wantStatus(t, e.get("/v1/developments/ZW/borrowdale-heights"), http.StatusOK)
	wantError(t, e.get("/v1/developments/gb/borrowdale-heights"), http.StatusNotFound, developments.CodeNotFound)
	for q, want := range map[string]bool{"status=SELLING": true, "status=PLANNED": false, "city=harare": true, "city=bulawayo": false,
		"market=GB": false, "q=borrowdale": true, "q=kensington": false} {
		if got := strings.Contains(e.get("/v1/developments?"+q).Body.String(), `"slug":"borrowdale-heights"`); got != want {
			t.Fatalf("%s: listed %v, want %v", q, got, want)
		}
	}
	wantError(t, e.get("/v1/developments?status=BUILDING"), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.get("/v1/developments?city=atlantis"), http.StatusBadRequest, httpx.CodeValidationFailed)

	// The developer's directory profile lists its public projects; the sitemap names them.
	profile := e.get("/v1/directory/developers/" + devseed.SampleDevelopersSlug).Body.String()
	if !strings.Contains(profile, `"projects":[{`) || !strings.Contains(profile, `"slug":"borrowdale-heights"`) || strings.Contains(profile, devseed.RuwaGardensSlug) {
		t.Fatalf("profile: %s", profile)
	}
	if other := e.get("/v1/directory/agencies/sample-estates").Body.String(); strings.Contains(other, `"projects"`) {
		t.Fatal("only developers have projects")
	}
	if !strings.Contains(e.get("/v1/directory/developers/sample-heights").Body.String(), `"projects":[]`) {
		t.Fatal("a developer without projects has an empty list")
	}
	sitemap := e.get("/v1/sitemap").Body.String()
	if !strings.Contains(sitemap, `"developments":[{"market":"ZW","slug":"borrowdale-heights"`) || strings.Contains(sitemap, devseed.RuwaGardensSlug) {
		t.Fatalf("sitemap: %s", sitemap)
	}

	// A profile that is no longer public takes its projects with it.
	var rec directory.Record
	for _, r := range devseed.ProfileRecords(e.now) {
		if r.Slug == devseed.SampleDevelopersSlug {
			rec = r
		}
	}
	rec.Status = "SUSPENDED"
	e.directory.Put(rec)
	wantError(t, e.get("/v1/developments/zw/borrowdale-heights"), http.StatusNotFound, developments.CodeNotFound)
	if strings.Contains(e.get("/v1/developments").Body.String(), "borrowdale-heights") || strings.Contains(e.get("/v1/sitemap").Body.String(), "borrowdale-heights") {
		t.Fatal("a suspended developer's project is not public")
	}
}

// contactMarkers are what a contact detail would look like in a body.
var contactMarkers = []string{"@", "email", "phone", "whatsapp", "+263", "example.test", "enquiry_email", "address"}

func TestDevelopmentPublicJSONHasNoContactDetails(t *testing.T) {
	e := newEnv(t)
	e.seedDevelopments()
	for _, path := range []string{"/v1/developments", "/v1/developments?market=zw&page_size=48", "/v1/developments/zw/borrowdale-heights",
		"/v1/directory/developers/" + devseed.SampleDevelopersSlug, "/v1/sitemap"} {
		rec := e.get(path)
		wantStatus(t, rec, http.StatusOK)
		raw := strings.ToLower(rec.Body.String())
		for _, m := range contactMarkers {
			if strings.Contains(raw, m) {
				i := strings.Index(raw, m)
				t.Fatalf("%s carries %q: ...%s...", path, m, raw[max(0, i-80):min(len(raw), i+80)])
			}
		}
	}
	// The highlights are the developer's statements, never advice.
	pub := decode[publicDev](t, e.get("/v1/developments/zw/borrowdale-heights"))
	if pub.Development.InvestmentHighlights.StatedBy != "DEVELOPER" || !strings.Contains(pub.Development.InvestmentHighlights.Disclaimer, "not financial advice") {
		t.Fatalf("highlights: %+v", pub.Development.InvestmentHighlights)
	}
	// Contact details typed into a project's text are refused.
	owner := e.developer()
	org := devOrg()
	ruwa := devseed.RuwaGardensID.String()
	for field, text := range map[string]string{"description": "Call 0771 234 567 for a viewing.", "summary": "Write to sales@ruwa.example",
		"name": "Ruwa +263 77 123 4567", "investment_highlights": ""} {
		body := obj{field: text}
		if field == "investment_highlights" {
			body = obj{field: []obj{{"label": "WhatsApp", "value": "wa.me/263771234567"}}}
		}
		env := wantError(t, e.patch(devPath(org, "/"+ruwa), body, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
		found := false
		for _, msg := range env.Error.Fields {
			found = found || msg == developments.MsgContactDetails
		}
		if !found {
			t.Fatalf("%s: %v", field, env.Error.Fields)
		}
	}
	// A price range is not a phone number.
	wantStatus(t, e.patch(devPath(org, "/"+ruwa), obj{"description": "Stands from 18000-26000 USD, 400 to 600 m2."}, owner), http.StatusOK)
}

func TestDevelopmentPriceOnRequestShape(t *testing.T) {
	e := newEnv(t)
	e.seedDevelopments()
	raw := e.get("/v1/developments/zw/borrowdale-heights").Body.String()
	for _, want := range []string{
		`"price":{"on_request":true,"from":null,"to":null,"currency":null}`,
		`"price":{"on_request":false,"from":"185000.00","to":"240000.00","currency":"USD"}`,
		`"price":{"on_request":false,"from":"95000.00","to":null,"currency":"USD"}`,
		`"price_from":{"amount":"95000.00","currency":"USD"}`, `"price_on_request":false`,
		`"size_m2":{"min":240,"max":null}`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("detail lacks %s: %s", want, raw)
		}
	}
	owner := e.developer()
	org := devOrg()
	phase := devseed.RuwaPhase1ID.String()
	ruwa := devseed.RuwaGardensID.String()
	path := devPath(org, "/"+ruwa+"/phases/"+phase+"/unit-types")
	env := wantError(t, e.post(path, obj{"name": "Corner stand", "price_on_request": true, "price_from": "20000", "currency": "USD"}, owner),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["price_from"] == "" || env.Error.Fields["currency"] == "" {
		t.Fatalf("fields: %v", env.Error.Fields)
	}
	env = wantError(t, e.post(path, obj{"name": "Corner stand", "price_from": "20000", "price_to": "19000"}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["price_to"] == "" || env.Error.Fields["currency"] == "" {
		t.Fatalf("fields: %v", env.Error.Fields)
	}
	wantError(t, e.post(path, obj{"name": "Corner stand", "price_from": 20000, "currency": "USD"}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	d := devOf(t, e.post(path, obj{"name": "Corner stand", "price_on_request": true}, owner), http.StatusCreated)
	if len(d.Phases[0].UnitTypes) != 2 {
		t.Fatalf("unit types: %+v", d.Phases)
	}
	if !strings.Contains(e.get(devPath(org, "/"+ruwa), owner).Body.String(), `"name":"Corner stand","bedrooms":null,"bathrooms":null,"size_m2":null,"price":{"on_request":true,"from":null,"to":null,"currency":null}`) {
		t.Fatal("the manage view has the same shape")
	}
}

func TestDevelopmentLinkedListingsOnlyWhenPublished(t *testing.T) {
	e := newEnv(t)
	e.seedDevelopments()
	pub := decode[publicDev](t, e.get("/v1/developments/zw/borrowdale-heights"))
	if len(pub.Development.AvailableUnits) != 1 || pub.Development.AvailableUnits[0].Listing.Slug != devseed.DevelopmentListingSlug ||
		pub.Development.AvailableUnits[0].UnitTypeID == nil || *pub.Development.AvailableUnits[0].UnitTypeID != devseed.ClusterHomeUnitTypeID.String() ||
		pub.Development.Phases[0].UnitTypes[0].AvailableListingCount != 1 {
		t.Fatalf("available units: %+v", pub.Development)
	}
	// The listing stops being public: it is not shown, the link stays.
	r := devseed.DevelopmentListingRecord(e.now)
	r.Status = listings.StatusPaused
	e.listings.Put(r)
	pub = decode[publicDev](t, e.get("/v1/developments/zw/borrowdale-heights"))
	if len(pub.Development.AvailableUnits) != 0 || pub.Development.Phases[0].UnitTypes[0].AvailableListingCount != 0 {
		t.Fatalf("a paused listing is shown: %+v", pub.Development.AvailableUnits)
	}
	owner := e.developer()
	org := devOrg()
	id := devseed.BorrowdaleHeightsID.String()
	d := devOf(t, e.get(devPath(org, "/"+id), owner), http.StatusOK)
	if len(d.LinkedListings) != 1 || d.LinkedListings[0].Public {
		t.Fatalf("manage view links: %+v", d.LinkedListings)
	}

	// Only the organisation's own listings can be linked; a listing is in one project.
	sampleEstatesListing := devseed.DraftListingID.String()
	wantError(t, e.put(devPath(org, "/"+id+"/listings/"+sampleEstatesListing), obj{"unit_type_id": nil}, owner), http.StatusNotFound, developments.CodeListingNotFound)
	wantError(t, e.put(devPath(org, "/"+devseed.RuwaGardensID.String()+"/listings/"+devseed.DevelopmentListingID.String()), obj{}, owner),
		http.StatusConflict, developments.CodeListingLinked)
	wantError(t, e.put(devPath(org, "/"+id+"/listings/"+devseed.DevelopmentListingID.String()), obj{"unit_type_id": devseed.RuwaStandUnitTypeID.String()}, owner),
		http.StatusNotFound, developments.CodeUnitTypeNotFound)
	// Unlink, link again without a unit type (phases and links change while published).
	d = devOf(t, e.deleteReq(devPath(org, "/"+id+"/listings/"+devseed.DevelopmentListingID.String()), owner), http.StatusOK)
	if len(d.LinkedListings) != 0 || d.ModerationStatus != "PUBLISHED" {
		t.Fatalf("unlinked: %+v", d)
	}
	r.Status = listings.StatusPublished
	e.listings.Put(r)
	devOf(t, e.put(devPath(org, "/"+id+"/listings/"+devseed.DevelopmentListingID.String()), obj{"unit_type_id": nil}, owner), http.StatusOK)
	pub = decode[publicDev](t, e.get("/v1/developments/zw/borrowdale-heights"))
	if len(pub.Development.AvailableUnits) != 1 || pub.Development.AvailableUnits[0].UnitTypeID != nil {
		t.Fatalf("relinked: %+v", pub.Development.AvailableUnits)
	}
}

func (e *env) developmentEnquiry(dev obj) obj {
	body := e.enquiry("developers", devseed.SampleDevelopersSlug)
	delete(body, "recipient")
	delete(body, "service")
	body["message"] = fmt.Sprintf("Enquiry %d: is a three-bedroom home still available?", e.seq)
	body["development"] = dev
	return body
}

func TestDevelopmentEnquiryReachesTheDeveloper(t *testing.T) {
	e := newEnv(t)
	e.seedDevelopments()
	unit := devseed.ClusterHomeUnitTypeID.String()
	rec := e.post("/v1/business-enquiries", e.developmentEnquiry(obj{"market": "zw", "slug": "borrowdale-heights", "unit_type_id": unit}))
	wantStatus(t, rec, http.StatusCreated)
	receipt := decode[enquiryReceipt](t, rec)
	// No contact details of the developer in the receipt either.
	if strings.Contains(strings.ToLower(rec.Body.String()), "example.test") || receipt.Status != "RECEIVED" {
		t.Fatalf("receipt: %s", rec.Body)
	}

	owner := e.developer()
	inbox := e.get("/v1/inbox/business-enquiries", owner).Body.String()
	want := fmt.Sprintf(`"development":{"id":"%s","market":"ZW","slug":"borrowdale-heights","name":"Borrowdale Heights","unit_type":{"id":"%s","name":"Three-bedroom cluster home"}}`,
		devseed.BorrowdaleHeightsID, unit)
	if !strings.Contains(inbox, receipt.EnquiryID) || !strings.Contains(inbox, want) || !strings.Contains(inbox, `"slug":"sample-developments"`) {
		t.Fatalf("inbox: %s", inbox)
	}
	org := devOrg()
	project := e.get(devPath(org, "/"+devseed.BorrowdaleHeightsID.String()+"/enquiries"), owner)
	wantStatus(t, project, http.StatusOK)
	if !strings.Contains(project.Body.String(), receipt.EnquiryID) || !strings.Contains(project.Body.String(), "tariro@example.com") {
		t.Fatalf("project enquiries: %s", project.Body)
	}
	if strings.Contains(e.get(devPath(org, "/"+devseed.RuwaGardensID.String()+"/enquiries"), owner).Body.String(), receipt.EnquiryID) {
		t.Fatal("another project's enquiries do not show it")
	}
	stored := e.enquiries.Enquiries()
	last := stored[len(stored)-1]
	if last.Development == nil || last.Development.Name != "Borrowdale Heights" || last.ProfileSlug != devseed.SampleDevelopersSlug {
		t.Fatalf("stored: %+v", last)
	}

	// The recipient may be named, but only as the developer.
	body := e.developmentEnquiry(obj{"market": "ZW", "slug": "borrowdale-heights"})
	body["recipient"] = obj{"kind": "developers", "slug": devseed.SampleDevelopersSlug}
	wantStatus(t, e.post("/v1/business-enquiries", body), http.StatusCreated)
	body = e.developmentEnquiry(obj{"market": "ZW", "slug": "borrowdale-heights"})
	body["recipient"] = obj{"kind": "developers", "slug": "sample-heights"}
	wantError(t, e.post("/v1/business-enquiries", body), http.StatusBadRequest, httpx.CodeValidationFailed)
	// Not public, unknown or a foreign unit type.
	wantError(t, e.post("/v1/business-enquiries", e.developmentEnquiry(obj{"market": "ZW", "slug": devseed.RuwaGardensSlug})),
		http.StatusNotFound, businessenquiries.CodeDevelopmentNotFound)
	wantError(t, e.post("/v1/business-enquiries", e.developmentEnquiry(obj{"market": "GB", "slug": "borrowdale-heights"})),
		http.StatusNotFound, businessenquiries.CodeDevelopmentNotFound)
	env := wantError(t, e.post("/v1/business-enquiries", e.developmentEnquiry(obj{"market": "ZW", "slug": "borrowdale-heights",
		"unit_type_id": devseed.RuwaStandUnitTypeID.String()})), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["development.unit_type_id"] == "" {
		t.Fatalf("fields: %v", env.Error.Fields)
	}
	// Without a development, the recipient is still required.
	body = e.developmentEnquiry(nil)
	delete(body, "development")
	wantError(t, e.post("/v1/business-enquiries", body), http.StatusBadRequest, httpx.CodeValidationFailed)

	// The agent and staff of an organisation do not see the contact details.
	agentOfEstates := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	wantError(t, e.get(devPath(org, "/"+devseed.BorrowdaleHeightsID.String()+"/enquiries"), agentOfEstates), http.StatusNotFound, developments.CodeNotFound)

	// The same per-IP limit as every business enquiry.
	limited := false
	for i := 0; i < 15 && !limited; i++ {
		rec := e.post("/v1/business-enquiries", e.developmentEnquiry(obj{"market": "ZW", "slug": "borrowdale-heights"}), withIP("203.0.113.77"))
		limited = rec.Code == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("the enquiry rate limit applies")
	}
}

func TestDevelopmentWrongOrganisationIs404(t *testing.T) {
	e := newEnv(t)
	e.seedDevelopments()
	org := devOrg()
	id := devseed.BorrowdaleHeightsID.String()
	phase := devseed.BorrowdalePhase1ID.String()
	unit := devseed.ClusterHomeUnitTypeID.String()
	photo := "0195c0de-0000-7000-8000-00000000f641"
	estatesOwner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	consumer := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	otherDeveloper := browser(e.loginCookie(devseed.BusinessOwnerEmail("sample-heights"), seedPassword))
	type call struct{ method, path string }
	calls := []call{
		{http.MethodGet, ""}, {http.MethodPost, ""}, {http.MethodGet, "/" + id}, {http.MethodPatch, "/" + id}, {http.MethodDelete, "/" + id},
		{http.MethodPost, "/" + id + "/submit"}, {http.MethodPost, "/" + id + "/withdraw"}, {http.MethodPost, "/" + id + "/phases"},
		{http.MethodPatch, "/" + id + "/phases/" + phase}, {http.MethodDelete, "/" + id + "/phases/" + phase},
		{http.MethodPost, "/" + id + "/phases/" + phase + "/unit-types"}, {http.MethodPatch, "/" + id + "/unit-types/" + unit},
		{http.MethodDelete, "/" + id + "/unit-types/" + unit}, {http.MethodPut, "/" + id + "/photos/order"},
		{http.MethodPatch, "/" + id + "/photos/" + photo}, {http.MethodDelete, "/" + id + "/photos/" + photo},
		{http.MethodPut, "/" + id + "/listings/" + devseed.DevelopmentListingID.String()},
		{http.MethodDelete, "/" + id + "/listings/" + devseed.DevelopmentListingID.String()}, {http.MethodGet, "/" + id + "/enquiries"},
	}
	for _, who := range []reqOpt{estatesOwner, consumer, otherDeveloper} {
		for _, c := range calls {
			var body any
			if c.method == http.MethodPost || c.method == http.MethodPatch || c.method == http.MethodPut {
				body = obj{"name": "x"}
			}
			wantError(t, e.do(c.method, devPath(org, c.path), body, who), http.StatusNotFound, developments.CodeNotFound)
		}
		wantError(t, e.uploadDevPhoto(devPath(org, "/"+id+"/photos"), testPhoto(t, 1200, 900), who), http.StatusNotFound, developments.CodeNotFound)
	}
	// Another developer's own path with this project's id is 404 too.
	heights := devseed.DevelopmentRecords(e.now)[0].OrganisationID
	for _, p := range devseed.Profiles() {
		if p.Slug == "sample-heights" {
			heights = p.Organisation.ID
		}
	}
	wantError(t, e.get(devPath(heights, "/"+id), otherDeveloper), http.StatusNotFound, developments.CodeNotFound)
	wantStatus(t, e.get(devPath(heights, ""), otherDeveloper), http.StatusOK)
	// An agency without a developer profile cannot start one.
	wantError(t, e.post(devPath(devseed.SampleEstates.ID, ""), obj{"name": "Agency estate"}, estatesOwner), http.StatusConflict, developments.CodeNoProfile)
	// An administrator session is no member either.
	wantError(t, e.get(devPath(org, ""), e.asAdmin(identity.SeedAdminEmail)), http.StatusNotFound, developments.CodeNotFound)
	// Unsigned: 401.
	wantStatus(t, e.get(devPath(org, "")), http.StatusUnauthorized)
}

func TestDevelopmentSlugIsUniquePerMarket(t *testing.T) {
	e := newEnv(t)
	e.seedDevelopments()
	owner := e.developer()
	org := devOrg()
	wantError(t, e.post(devPath(org, ""), obj{"name": "Borrowdale Heights", "slug": "borrowdale-heights"}, owner), http.StatusConflict, developments.CodeSlugTaken)
	d := devOf(t, e.post(devPath(org, ""), obj{"name": "Borrowdale Heights"}, owner), http.StatusCreated)
	if d.Slug != "borrowdale-heights-2" {
		t.Fatalf("slug = %q", d.Slug)
	}
	other := devOf(t, e.post(devPath(org, ""), obj{"name": "Borrowdale Heights London", "slug": "borrowdale-heights", "market": "GB", "city": "london"}, owner), http.StatusCreated)
	if other.Slug != "borrowdale-heights" {
		t.Fatalf("another market: %+v", other)
	}
	wantError(t, e.patch(devPath(org, "/"+d.ID), obj{"slug": "borrowdale-heights"}, owner), http.StatusConflict, developments.CodeSlugTaken)
	wantError(t, e.patch(devPath(org, "/"+d.ID), obj{"slug": "Not A Slug"}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.patch(devPath(org, "/"+d.ID), obj{"market": "GB"}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	// The slug of a project that has been published is fixed.
	withdrawn := devOf(t, e.post(devPath(org, "/"+devseed.BorrowdaleHeightsID.String()+"/withdraw"), nil, owner), http.StatusOK)
	env := wantError(t, e.patch(devPath(org, "/"+withdrawn.ID), obj{"slug": "borrowdale-heights-new"}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["slug"] == "" {
		t.Fatalf("fields: %v", env.Error.Fields)
	}
}

func TestDevelopmentPhotosAreScanned(t *testing.T) {
	e, fake, _ := newScanningEnv(t)
	e.seedDevelopments()
	owner := e.developer()
	path := devPath(devOrg(), "/"+devseed.RuwaGardensID.String()+"/photos")
	before := e.storedFiles()
	wantError(t, e.uploadDevPhoto(path, infected(testPhoto(t, 1200, 900)), owner), http.StatusUnprocessableEntity, storage.CodeFileInfected)
	if e.storedFiles() != before {
		t.Fatal("an infected photo was stored")
	}
	if n, _ := fake.Calls(); n != 1 {
		t.Fatalf("scans = %d", n)
	}
	fake.SetDown(true)
	rec := e.uploadDevPhoto(path, testPhoto(t, 1200, 900), owner)
	wantError(t, rec, http.StatusServiceUnavailable, storage.CodeScannerUnavailable)
	if e.storedFiles() != before {
		t.Fatal("nothing is stored while the scanner is down")
	}
	fake.SetDown(false)
	d := devOf(t, e.uploadDevPhoto(path, testPhoto(t, 1200, 900), owner), http.StatusCreated)
	if len(d.Photos) != 2 || e.storedFiles() != before+3 {
		t.Fatalf("clean upload: %d photos, %d files", len(d.Photos), e.storedFiles()-before)
	}
	// The draft's uploaded photo is the organisation's to see, not the public's.
	var uploaded string
	for _, ph := range d.Photos {
		if ph.ID != "0195c0de-0000-7000-8000-00000000f644" {
			uploaded = ph.ID
		}
	}
	wantStatus(t, e.get("/v1/development-photos/"+uploaded+"/large"), http.StatusNotFound)
	file := e.get("/v1/development-photos/"+uploaded+"/small", owner)
	wantStatus(t, file, http.StatusOK)
	if file.Header().Get("Content-Type") != "image/jpeg" || !strings.Contains(file.Header().Get("Cache-Control"), "private") {
		t.Fatalf("photo headers: %v", file.Header())
	}
	// Not an image: refused before any scan.
	wantError(t, e.uploadDevPhoto(path, []byte("%PDF-1.4 not an image"), owner), http.StatusUnsupportedMediaType, listings.CodeUnsupportedFile)
}

func TestDevelopmentAccountDeletion(t *testing.T) {
	e := newEnv(t)
	e.seedDevelopments()
	// The project survives its creator's account deletion, unlinked.
	admin := e.asAdmin(devseed.PlatformAdminEmail)
	var creator uuid.UUID
	for _, d := range e.developments.All() {
		if d.ID == devseed.BorrowdaleHeightsID && d.CreatedBy != nil {
			creator = *d.CreatedBy
		}
	}
	if creator == uuid.Nil {
		t.Fatal("the sample project has a creator")
	}
	wantStatus(t, e.post("/v1/admin/users/"+creator.String()+"/delete", obj{"reason": "Asked to be forgotten (ticket 7)."}, admin), http.StatusOK)
	for _, d := range e.developments.All() {
		if d.CreatedBy != nil && *d.CreatedBy == creator || d.SubmittedBy != nil && *d.SubmittedBy == creator {
			t.Fatalf("the deleted account is still linked: %+v", d)
		}
	}
	// The organisation keeps its project, still public.
	wantStatus(t, e.get("/v1/developments/zw/borrowdale-heights"), http.StatusOK)
}
