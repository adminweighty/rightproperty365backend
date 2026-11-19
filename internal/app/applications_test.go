package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/analytics"
	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/outbox"
)

type screeningBody struct {
	Application struct {
		ID        string  `json:"id"`
		Reference *string `json:"reference"`
		Status    string  `json:"status"`
		Form      struct {
			Version  int `json:"version"`
			Sections []struct {
				Key    string `json:"key"`
				Fields []struct {
					Key   string `json:"key"`
					Label string `json:"label"`
				} `json:"fields"`
			} `json:"sections"`
		} `json:"form"`
		Applicant struct {
			Name string `json:"name"`
		} `json:"applicant"`
		Rendered []struct {
			Fields []struct {
				Key     string  `json:"key"`
				Display *string `json:"display"`
				Visible bool    `json:"visible"`
			} `json:"fields"`
		} `json:"rendered"`
		Erased bool `json:"erased"`
	} `json:"application"`
}

func (e *env) uploadFile(path, name string, content []byte, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = fw.Write(content)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// The seeded questionnaire and applications through the whole stack.
func TestApplicationsEndToEnd(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID.String()
	rent := devseed.RentListings(e.now)
	if len(rent) != 2 {
		t.Fatalf("%d sample rent listings", len(rent))
	}
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))

	// The public listing says it takes applications; the preview is version 2.
	listing := e.get("/v1/listings/" + rent[0].Slug).Body.String()
	if !strings.Contains(listing, `"applications":{"open":true,"closes_at":null,"form_name":"Rental application"}`) {
		t.Fatalf("listing: %s", listing)
	}
	rec := e.get("/v1/listings/" + rent[0].Slug + "/application-form")
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"version":2`) || !strings.Contains(rec.Body.String(), `"status":"OPEN"`) {
		t.Fatalf("preview: %s", rec.Body)
	}
	wantError(t, e.get("/v1/listings/"+devseed.Sample().Listings[0].Slug+"/application-form"), http.StatusNotFound, applications.CodeApplicationFormNotFound)

	// The tenant's two applications; the submitted one keeps version 1's questions.
	rec = e.get("/v1/me/applications", tenant)
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"total":2`) || rec.Header().Get("Cache-Control") != "private, no-store" || rec.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("mine: %s", rec.Body)
	}
	sub := decode[screeningBody](t, e.get("/v1/me/applications/"+devseed.SubmittedApplicationID.String(), tenant)).Application
	if sub.Status != applications.StatusSubmitted || *sub.Reference != devseed.SubmittedApplicationReference || sub.Form.Version != 1 ||
		len(sub.Form.Sections) != 3 || len(sub.Form.Sections[1].Fields) != 2 {
		t.Fatalf("submitted: %+v", sub)
	}
	// Another customer sees neither.
	other := browser(e.loginCookie(devseed.SellerEmail, seedPassword))
	wantError(t, e.get("/v1/me/applications/"+devseed.SubmittedApplicationID.String(), other), http.StatusNotFound, applications.CodeApplicationNotFound)

	// The agency reads it, rendered against version 1, and the read is audited.
	got := decode[screeningBody](t, e.get("/v1/agent/organisations/"+org+"/applications/"+devseed.SubmittedApplicationID.String(), owner)).Application
	if got.Applicant.Name != "Tariro Tenant" || got.Form.Version != 1 || *got.Rendered[1].Fields[0].Display != "Yes" || !got.Rendered[1].Fields[1].Visible {
		t.Fatalf("agency view: %+v", got)
	}
	audit := decode[auditBody](t, e.get("/v1/agent/organisations/"+org+"/audit", owner))
	if audit.Items[0].Action != "APPLICATION_VIEWED" || audit.Items[0].Target.Label != devseed.SubmittedApplicationReference {
		t.Fatalf("audit: %+v", audit.Items[0])
	}
	rec = e.get("/v1/agent/organisations/"+org+"/applications/"+devseed.SubmittedApplicationID.String()+"/files/"+uuid.NewString(), owner)
	wantError(t, rec, http.StatusNotFound, applications.CodeFileNotFound)
	// Another organisation: 404 on every agent route.
	second := identity.SeedSecondOrganisationID.String()
	for _, path := range []string{"/v1/agent/organisations/" + org + "/applications/" + devseed.SubmittedApplicationID.String(),
		"/v1/agent/organisations/" + second + "/applications/" + devseed.SubmittedApplicationID.String()} {
		wantError(t, e.get(path, blake), http.StatusNotFound, applications.CodeApplicationNotFound)
	}
	wantError(t, e.get("/v1/agent/organisations/"+org+"/applications", blake), http.StatusNotFound, applications.CodeOrganisationNotFound)
	wantError(t, e.get("/v1/agent/organisations/"+org+"/forms", blake), http.StatusNotFound, applications.CodeFormNotFound)
	wantError(t, e.get("/v1/agent/organisations/"+second+"/forms/"+devseed.RentalFormV2ID.String(), blake), http.StatusNotFound, applications.CodeFormNotFound)
	rec = e.get("/v1/agent/organisations/"+org+"/forms", owner)
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"status":"RETIRED"`) || !strings.Contains(rec.Body.String(), `"submission_count":1`) {
		t.Fatalf("forms: %s", rec.Body)
	}

	// A disallowed file type is 400 (FR-018).
	draft := devseed.DraftApplicationID.String()
	wantError(t, e.uploadFile("/v1/me/applications/"+draft+"/files?field=proof_of_income", "payslip.pdf", []byte("just text, not a PDF"), tenant),
		http.StatusBadRequest, applications.CodeUnsupportedFileType)
	rec = e.uploadFile("/v1/me/applications/"+draft+"/files?field=proof_of_income", "payslip.pdf", []byte("%PDF-1.4\n%%EOF\n"), tenant)
	wantStatus(t, rec, http.StatusCreated)

	// Submit after close: 409 with the explanation (FR-019).
	rec = e.do(http.MethodPut, "/v1/me/listings/"+rent[1].ID.String()+"/applications-intake",
		obj{"form_id": devseed.RentalFormV2ID.String(), "status": "CLOSED"}, owner)
	wantStatus(t, rec, http.StatusOK)
	rec = e.post("/v1/me/applications/"+draft+"/submit", nil, tenant)
	wantStatus(t, rec, http.StatusConflict)
	var closed struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &closed); err != nil || closed.Error.Code != applications.CodeApplicationsClosed ||
		closed.Error.Details["reason"] != applications.ReasonClosed || !strings.Contains(closed.Error.Message, "closed") {
		t.Fatalf("closed: %s", rec.Body)
	}
	wantError(t, e.post("/v1/me/applications", obj{"listing_id": rent[1].ID.String()}, other), http.StatusConflict, applications.CodeApplicationsClosed)
	// Short stays are not eligible.
	for _, l := range devseed.ListingRecords(e.now) {
		if l.Category == "short-stay" && l.OrganisationID != nil && *l.OrganisationID == devseed.SampleEstates.ID {
			wantError(t, e.do(http.MethodPut, "/v1/me/listings/"+l.ID.String()+"/applications-intake",
				obj{"form_id": devseed.RentalFormV2ID.String(), "status": "OPEN"}, owner), http.StatusBadRequest, applications.CodeListingNotEligible)
			break
		}
	}
}

// A submitted application links to the matching contact and shows on its
// timeline; analytics counts it.
func TestApplicationCRMAndAnalytics(t *testing.T) {
	e := newEnv(t)
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	// The sample contact Kudzai Sithole (the sample tenancy's tenant) already
	// has this email: keep both.
	rec := e.post(contactsPath(devseed.SampleEstates.ID, "/?allow_duplicate=true"), obj{"name": "Tariro Tenant", "type": "tenant", "emails": []string{devseed.TenantEmail}}, owner)
	wantStatus(t, rec, http.StatusCreated)
	id := decode[contactBody](t, rec).Contact.ID
	timeline := e.get(contactsPath(devseed.SampleEstates.ID, "/"+id+"/timeline"), owner).Body.String()
	if !strings.Contains(timeline, `"kind":"APPLICATION"`) || !strings.Contains(timeline, devseed.SubmittedApplicationReference) {
		t.Fatalf("timeline: %s", timeline)
	}
	// The outbox handler links a new submission (idempotent).
	var events []outbox.Event
	e.apps.SetOutboxSink(func(ev outbox.Event) { events = append(events, ev) })
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	draft := devseed.DraftApplicationID.String()
	rec = e.do(http.MethodPatch, "/v1/me/applications/"+draft, obj{"answers": obj{"move_in_date": "2026-12-15", "employment_status": "student",
		"smoker": "no", "occupants": 1, "has_pets": "no"}}, tenant)
	wantStatus(t, rec, http.StatusOK)
	wantStatus(t, e.uploadFile("/v1/me/applications/"+draft+"/files?field=proof_of_income", "p.pdf", []byte("%PDF-1.4\n%%EOF\n"), tenant), http.StatusCreated)
	rec = e.post("/v1/me/applications/"+draft+"/submit", nil, tenant)
	wantStatus(t, rec, http.StatusOK)
	if len(events) != 1 || events[0].Type != applications.EventTypeSubmitted {
		t.Fatalf("events %+v", events)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	crmSvc := CRMService(e.crm, e.crmItems, e.listings, e.identity, e.reference, e.team, logger, func() time.Time { return e.now })
	for range 2 {
		if err := crmSvc.HandleOutbox(context.Background(), events[0]); err != nil {
			t.Fatal(err)
		}
	}
	links, _ := e.crm.Links(context.Background(), uuid.MustParse(id))
	n := 0
	for _, l := range links {
		if l.Kind == crm.KindApplication {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d application links", n)
	}
	svc := AnalyticsService(e.analytics, e.listings, e.marketStore, 90, logger, func() time.Time { return e.now })
	if err := analytics.NewRecorder(svc, AppointmentListing(e.appts)).Handle(context.Background(), events[0]); err != nil {
		t.Fatal(err)
	}
	if got := e.analytics.Events(); len(got) != 1 || got[0].Type != analytics.TypeApplicationSubmit || got[0].Origin != analytics.OriginServer {
		t.Fatalf("analytics %+v", got)
	}
}

// Account deletion anonymises the applicant's submitted application and
// deletes the draft.
func TestApplicantAccountDeletion(t *testing.T) {
	e := newEnv(t)
	cookie := e.loginCookie(devseed.TenantEmail, seedPassword)
	wantStatus(t, e.del("/v1/me", obj{"password": seedPassword}, browser(cookie)), http.StatusOK)
	if _, err := e.apps.Application(context.Background(), devseed.DraftApplicationID); err == nil {
		t.Fatal("the draft survived the account")
	}
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	got := decode[screeningBody](t, e.get("/v1/agent/organisations/"+devseed.SampleEstates.ID.String()+"/applications/"+
		devseed.SubmittedApplicationID.String(), owner)).Application
	if got.Applicant.Name != applications.DeletedApplicantName || got.Status != applications.StatusWithdrawn || !got.Erased {
		t.Fatalf("after deletion: %+v", got)
	}
}
