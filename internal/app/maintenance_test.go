package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

func maintenancePath(book uuid.UUID, rest string) string {
	return "/v1/agent/organisations/" + book.String() + "/maintenance" + rest
}

func myMaintenancePath(tenancy uuid.UUID, rest string) string {
	return "/v1/me/tenancies/" + tenancy.String() + "/maintenance" + rest
}

type maintenanceBody struct {
	ID          string `json:"id"`
	Reference   string `json:"reference"`
	Status      string `json:"status"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	} `json:"category"`
	RaisedBy struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		You  bool   `json:"you"`
	} `json:"raised_by"`
	Provider *struct {
		Name string `json:"name"`
	} `json:"provider"`
	Assignment *struct {
		Enquiry *struct {
			ID        string `json:"id"`
			Reference string `json:"reference"`
		} `json:"enquiry"`
	} `json:"assignment"`
	CostTotals []struct {
		Currency string `json:"currency"`
		Amount   string `json:"amount"`
		Count    int    `json:"count"`
	} `json:"cost_totals"`
	Costs []struct {
		ID            string `json:"id"`
		LedgerEntryID string `json:"ledger_entry_id"`
		Reversed      bool   `json:"reversed"`
	} `json:"costs"`
	Photos []struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"photos"`
	Timeline []struct {
		Type     string  `json:"type"`
		Text     *string `json:"text"`
		Internal bool    `json:"internal"`
	} `json:"timeline"`
	Can struct {
		Transitions []string `json:"transitions"`
		Assign      bool     `json:"assign"`
	} `json:"can"`
}

type maintenanceOne struct {
	Request maintenanceBody `json:"request"`
}

func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for x := range 64 {
		img.Set(x, 10, color.RGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func maintenancePNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 40, 30))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func (e *env) uploadTo(path string, content []byte, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "photo.jpg")
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = part.Write(content)
	_ = w.Close()
	return e.raw(http.MethodPost, path, &body, w.FormDataContentType(), opts...)
}

// Every agent endpoint answers another organisation's member, a consumer,
// the tenant and an administrator 404 BOOK_NOT_FOUND, and never data.
func TestMaintenanceWrongOrganisation(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	rid, pid := devseed.MaintenanceTapID.String(), devseed.PropertyAvondaleID.String()
	photo := uuid.NewString()
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	admin := browser(e.adminLogin(identity.SeedAdminEmail, seedPassword))
	calls := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, maintenancePath(org, ""), nil},
		{http.MethodGet, maintenancePath(org, "?status=bad"), nil},
		{http.MethodGet, maintenancePath(org, "/options"), nil},
		{http.MethodGet, maintenancePath(org, "/providers?q=plumb"), nil},
		{http.MethodPost, maintenancePath(org, ""), obj{"property_id": pid, "category": "plumbing", "priority": "low", "title": "x"}},
		{http.MethodPost, maintenancePath(org, ""), obj{"title": 1}},
		{http.MethodGet, maintenancePath(org, "/"+rid), nil},
		{http.MethodGet, maintenancePath(org, "/not-a-uuid"), nil},
		{http.MethodPatch, maintenancePath(org, "/"+rid), obj{"title": "x"}},
		{http.MethodPost, maintenancePath(org, "/"+rid+"/status"), obj{"status": "IN_PROGRESS"}},
		{http.MethodPost, maintenancePath(org, "/"+rid+"/comments"), obj{"text": "x"}},
		{http.MethodPost, maintenancePath(org, "/"+rid+"/assign"), obj{"directory_profile_id": devseed.PlumbersProfile().ID.String()}},
		{http.MethodPost, maintenancePath(org, "/"+rid+"/costs"), obj{"amount": "1.00", "currency": "USD", "description": "x"}},
		{http.MethodGet, maintenancePath(org, "/"+rid+"/photos/"+photo), nil},
		{http.MethodDelete, maintenancePath(org, "/"+rid+"/photos/"+photo), nil},
	}
	for who, as := range map[string]reqOpt{"another organisation": blake, "consumer": consumer, "tenant": tenant, "administrator": admin} {
		for _, c := range calls {
			rec := e.do(c.method, c.path, c.body, as)
			wantError(t, rec, http.StatusNotFound, maintenance.CodeBookNotFound)
			if body := rec.Body.String(); strings.Contains(body, "Avondale") || strings.Contains(body, "RPM-") || strings.Contains(body, "tap") {
				t.Fatalf("%s %s %s: %s", who, c.method, c.path, body)
			}
		}
		wantError(t, e.uploadTo(maintenancePath(org, "/"+rid+"/photos"), testJPEG(t), as), http.StatusNotFound, maintenance.CodeBookNotFound)
	}
	wantError(t, e.get(maintenancePath(org, "")), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	// Blake's own book does not reach Sample Estates' requests or property.
	second := devseed.SecondStreetRealty.ID
	wantError(t, e.get(maintenancePath(second, "/"+rid), blake), http.StatusNotFound, maintenance.CodeNotFound)
	wantError(t, e.post(maintenancePath(second, "/"+rid+"/status"), obj{"status": "IN_PROGRESS"}, blake), http.StatusNotFound,
		maintenance.CodeNotFound)
	wantError(t, e.post(maintenancePath(second, "/"+rid+"/costs"), obj{"amount": "1.00", "currency": "USD", "description": "x"}, blake),
		http.StatusNotFound, maintenance.CodeNotFound)
	if body := e.get(maintenancePath(second, ""), blake).Body.String(); !strings.Contains(body, `"total":0`) {
		t.Fatalf("second street: %s", body)
	}
	env := wantError(t, e.post(maintenancePath(second, ""), obj{"property_id": pid, "tenancy_id": devseed.TenancyFlat1ID.String(),
		"category": "plumbing", "priority": "low", "title": "x"}, blake), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["property_id"] == "" {
		t.Fatalf("cross-book property: %+v", env.Error.Fields)
	}
	env = wantError(t, e.post(maintenancePath(second, ""), obj{"property_id": devseed.PropertySecondStreetID.String(),
		"tenancy_id": devseed.TenancyFlat1ID.String(), "category": "plumbing", "priority": "low", "title": "x"}, blake),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["tenancy_id"] == "" {
		t.Fatalf("cross-book tenancy: %+v", env.Error.Fields)
	}
}

// The seed, STAFF read only, a manager's request through the status flow,
// edits and comments.
func TestMaintenanceManagerFlow(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))

	page := decode[struct {
		Items        []maintenanceBody `json:"items"`
		Total        int               `json:"total"`
		StatusCounts map[string]int    `json:"status_counts"`
	}](t, e.get(maintenancePath(org, ""), staff))
	// Five with RPM-2026-900005, the closed drain job of the review seed (API 0.36.4).
	if page.Total != 5 || page.StatusCounts["ASSIGNED"] != 1 || page.StatusCounts["IN_PROGRESS"] != 1 || page.StatusCounts["NEW"] != 1 ||
		page.StatusCounts["CLOSED"] != 2 || len(page.StatusCounts) != 7 {
		t.Fatalf("seed list: %d %+v", page.Total, page.StatusCounts)
	}
	tap := decode[maintenanceOne](t, e.get(maintenancePath(org, "/"+devseed.MaintenanceTapID.String()), staff)).Request
	if tap.Reference != "RPM-2026-900001" || tap.Status != "ASSIGNED" || tap.Provider == nil || tap.Provider.Name != "Sample Plumbers" ||
		tap.Assignment == nil || tap.Assignment.Enquiry == nil || len(tap.CostTotals) != 1 || tap.CostTotals[0].Amount != "85.00" ||
		tap.RaisedBy.Kind != "TENANT" || len(tap.Can.Transitions) != 0 || tap.Can.Assign {
		t.Fatalf("seeded tap: %+v", tap)
	}
	if opts := e.get(maintenancePath(org, "/options"), staff).Body.String(); !strings.Contains(opts, `"write":false`) ||
		!strings.Contains(opts, `"plumbing"`) || !strings.Contains(opts, `"urgent"`) {
		t.Fatalf("options: %s", opts)
	}
	// STAFF may not write.
	create := obj{"property_id": devseed.PropertyAvondaleID.String(), "unit_id": devseed.UnitFlat3ID.String(), "category": "pests",
		"priority": "normal", "title": "Ants in the stairwell", "description": "Near the bins."}
	wantError(t, e.post(maintenancePath(org, ""), create, staff), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.post(maintenancePath(org, "/"+devseed.MaintenanceOvenID.String()+"/status"), obj{"status": "TRIAGED"}, staff),
		http.StatusForbidden, httpx.CodeForbidden)

	// Validation.
	env := wantError(t, e.post(maintenancePath(org, ""), obj{"property_id": devseed.PropertyAvondaleID.String(), "unit_id": uuid.NewString(),
		"tenancy_id": devseed.TenancyCottageID.String(), "category": "nope", "priority": "", "title": ""}, agent),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	for _, f := range []string{"unit_id", "category", "priority", "title"} {
		if env.Error.Fields[f] == "" {
			t.Fatalf("validation %s: %+v", f, env.Error.Fields)
		}
	}

	rec := e.post(maintenancePath(org, ""), create, agent)
	wantStatus(t, rec, http.StatusCreated)
	req := decode[maintenanceOne](t, rec).Request
	if req.Status != "NEW" || !strings.HasPrefix(req.Reference, "RPM-2026-") || req.RaisedBy.Kind != "MANAGER" ||
		strings.Join(req.Can.Transitions, ",") != "TRIAGED,CANCELLED" || !req.Can.Assign {
		t.Fatalf("created: %+v", req)
	}
	path := maintenancePath(org, "/"+req.ID)
	// Not every move is allowed; the error names the allowed ones.
	rec = e.post(path+"/status", obj{"status": "RESOLVED"}, agent)
	wantError(t, rec, http.StatusConflict, maintenance.CodeInvalidTransition)
	if !strings.Contains(rec.Body.String(), `"allowed":["TRIAGED","CANCELLED"]`) {
		t.Fatalf("transition error: %s", rec.Body.String())
	}
	wantError(t, e.post(path+"/status", obj{"status": "ASSIGNED"}, agent), http.StatusConflict, maintenance.CodeInvalidTransition)
	for _, to := range []string{"TRIAGED", "IN_PROGRESS", "RESOLVED", "IN_PROGRESS", "RESOLVED", "CLOSED"} {
		wantStatus(t, e.post(path+"/status", obj{"status": to, "note": "step"}, agent), http.StatusOK)
	}
	wantError(t, e.post(path+"/status", obj{"status": "CANCELLED", "note": "x"}, agent), http.StatusConflict, maintenance.CodeInvalidTransition)
	wantError(t, e.do(http.MethodPatch, path, obj{"priority": "high"}, agent), http.StatusConflict, maintenance.CodeClosed)
	// Comments are allowed on a closed request; an internal one is flagged.
	wantStatus(t, e.post(path+"/comments", obj{"text": "Invoice filed.", "internal": true}, agent), http.StatusCreated)
	done := decode[maintenanceOne](t, e.get(path, agent)).Request
	if done.Status != "CLOSED" || len(done.Timeline) != 8 || !done.Timeline[7].Internal {
		t.Fatalf("closed: %+v", done.Timeline)
	}

	// Cancel needs a reason, and is audited.
	rec = e.post(maintenancePath(org, ""), create, agent)
	other := decode[maintenanceOne](t, rec).Request
	env = wantError(t, e.post(maintenancePath(org, "/"+other.ID+"/status"), obj{"status": "CANCELLED"}, agent), http.StatusBadRequest,
		httpx.CodeValidationFailed)
	if env.Error.Fields["note"] == "" {
		t.Fatalf("cancel without reason: %+v", env.Error.Fields)
	}
	wantStatus(t, e.do(http.MethodPatch, maintenancePath(org, "/"+other.ID), obj{"priority": "urgent", "title": "Ants everywhere"}, agent),
		http.StatusOK)
	wantStatus(t, e.post(maintenancePath(org, "/"+other.ID+"/status"), obj{"status": "CANCELLED", "note": "Duplicate."}, agent), http.StatusOK)
	if !e.auditHas(org, orgaudit.ActionMaintenanceCancelled) {
		t.Fatal("no MAINTENANCE_CANCELLED audit record")
	}
}

func (e *env) auditHas(org uuid.UUID, action string) bool {
	for _, ev := range e.team.AuditLog() {
		if ev.OrganisationID == org && ev.Action == action {
			return true
		}
	}
	return false
}

// The tenant: their own tenancy's requests only; raising, commenting and
// photos; never another tenancy's, never internal comments, never status,
// assignment or costs.
func TestMaintenanceTenant(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	flat1, cottage := devseed.TenancyFlat1ID, devseed.TenancyCottageID

	list := decode[struct {
		Items []maintenanceBody `json:"items"`
	}](t, e.get(myMaintenancePath(flat1, ""), tenant))
	if len(list.Items) != 2 {
		t.Fatalf("tenant list: %+v", list.Items)
	}
	// The cottage is not the sample tenant's tenancy; its request is not on Flat 1.
	wantError(t, e.get(myMaintenancePath(cottage, ""), tenant), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	wantError(t, e.get(myMaintenancePath(flat1, ""), consumer), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	wantError(t, e.get(myMaintenancePath(flat1, "/"+devseed.MaintenancePowerID.String()), tenant), http.StatusNotFound, maintenance.CodeNotFound)
	wantError(t, e.get(myMaintenancePath(flat1, "/"+devseed.MaintenanceHedgeID.String()), tenant), http.StatusNotFound, maintenance.CodeNotFound)
	wantError(t, e.post(myMaintenancePath(flat1, "/"+devseed.MaintenancePowerID.String()+"/comments"), obj{"text": "hello"}, tenant),
		http.StatusNotFound, maintenance.CodeNotFound)
	wantError(t, e.get(myMaintenancePath(flat1, "")), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// The seeded tap shows the provider's name, but no costs, ids or internal comments.
	wantStatus(t, e.post(maintenancePath(org, "/"+devseed.MaintenanceTapID.String()+"/comments"),
		obj{"text": "Landlord approved up to 200.", "internal": true}, agent), http.StatusCreated)
	rec := e.get(myMaintenancePath(flat1, "/"+devseed.MaintenanceTapID.String()), tenant)
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	for _, banned := range []string{"Landlord approved", "85.00", "ledger", "cost", devseed.MaintenanceEnquiryID.String(),
		devseed.PlumbersProfile().ID.String(), "Chipo Dube", `"user_id"`, "BE-"} {
		if strings.Contains(body, banned) {
			t.Fatalf("tenant view shows %q: %s", banned, body)
		}
	}
	if !strings.Contains(body, `"name":"Sample Plumbers"`) || !strings.Contains(body, `"name":"Sample Estates"`) {
		t.Fatalf("tenant view: %s", body)
	}

	// Raising a request.
	rec = e.post(myMaintenancePath(flat1, ""), obj{"category": "plumbing", "priority": "high", "title": "Shower drain blocked",
		"description": "Call me on +263771234567"}, tenant)
	wantStatus(t, rec, http.StatusCreated)
	mine := decode[maintenanceOne](t, rec).Request
	if mine.Status != "NEW" || mine.RaisedBy.Kind != "TENANT" || !mine.RaisedBy.You {
		t.Fatalf("raised: %+v", mine)
	}
	wantStatus(t, e.post(myMaintenancePath(flat1, "/"+mine.ID+"/comments"), obj{"text": "It is getting worse."}, tenant), http.StatusCreated)
	wantStatus(t, e.uploadTo(myMaintenancePath(flat1, "/"+mine.ID+"/photos"), testJPEG(t), tenant), http.StatusCreated)
	// The managers see it, with the tenant as the raiser.
	seen := decode[maintenanceOne](t, e.get(maintenancePath(org, "/"+mine.ID), agent)).Request
	if seen.RaisedBy.Kind != "TENANT" || seen.RaisedBy.Name != "Tariro Tenant" || len(seen.Photos) != 1 || len(seen.Timeline) != 3 {
		t.Fatalf("manager view of the tenant's request: %+v", seen)
	}
	// The tenant cannot change the status, assign or record a cost: no tenant
	// route exists, and the agent routes are not their book.
	for _, p := range []string{"/status", "/assign", "/costs"} {
		wantStatus(t, e.post(myMaintenancePath(flat1, "/"+mine.ID+p), obj{"status": "CLOSED"}, tenant), http.StatusNotFound)
		wantError(t, e.post(maintenancePath(org, "/"+mine.ID+p), obj{"status": "CLOSED"}, tenant), http.StatusNotFound, maintenance.CodeBookNotFound)
	}
	wantStatus(t, e.do(http.MethodPatch, myMaintenancePath(flat1, "/"+mine.ID), obj{"title": "x"}, tenant), http.StatusMethodNotAllowed)
	// A closed request takes no more tenant comments.
	wantStatus(t, e.post(maintenancePath(org, "/"+mine.ID+"/status"), obj{"status": "CANCELLED", "note": "Fixed by the tenant."}, agent),
		http.StatusOK)
	wantError(t, e.post(myMaintenancePath(flat1, "/"+mine.ID+"/comments"), obj{"text": "x"}, tenant), http.StatusConflict, maintenance.CodeClosed)
}

// Assigning sends exactly one business enquiry (idempotent on retry, also
// after a failure between the enquiry and the link), with no tenant data.
func TestMaintenanceAssignment(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	plumbers := devseed.PlumbersProfile().ID.String()
	path := maintenancePath(org, "/"+devseed.MaintenanceOvenID.String())

	providers := e.get(maintenancePath(org, "/providers?q=plumb"), agent).Body.String()
	if !strings.Contains(providers, plumbers) || strings.Contains(providers, "agencies") {
		t.Fatalf("providers: %s", providers)
	}
	env := wantError(t, e.post(path+"/assign", obj{"directory_profile_id": devseed.Profiles()[0].ID.String()}, agent),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["directory_profile_id"] == "" {
		t.Fatalf("an agency as provider: %+v", env.Error.Fields)
	}
	before := len(e.enquiries.Enquiries())
	// The link fails after the enquiry was sent: nothing is linked.
	e.maintenance.FailNext(errors.New("disk full"))
	wantStatus(t, e.post(path+"/assign", obj{"directory_profile_id": plumbers, "message": "Please call the office."}, agent),
		http.StatusInternalServerError)
	if n := len(e.enquiries.Enquiries()); n != before+1 {
		t.Fatalf("%d enquiries after the failed link, want %d", n, before+1)
	}
	// The retry replays the same enquiry and links it.
	rec := e.post(path+"/assign", obj{"directory_profile_id": plumbers, "message": "Please call the office."}, agent)
	wantStatus(t, rec, http.StatusOK)
	res := decode[struct {
		Request maintenanceBody `json:"request"`
		Enquiry struct {
			ID        string `json:"id"`
			Reference string `json:"reference"`
			Replayed  bool   `json:"replayed"`
		} `json:"enquiry"`
	}](t, rec)
	if !res.Enquiry.Replayed || res.Request.Status != "ASSIGNED" || res.Request.Assignment == nil ||
		res.Request.Assignment.Enquiry.ID != res.Enquiry.ID {
		t.Fatalf("assign: %+v", res)
	}
	// Again: still exactly one.
	wantStatus(t, e.post(path+"/assign", obj{"directory_profile_id": plumbers}, agent), http.StatusOK)
	all := e.enquiries.Enquiries()
	if len(all) != before+1 {
		t.Fatalf("%d enquiries, want %d", len(all), before+1)
	}
	enq := all[len(all)-1]
	if !strings.Contains(enq.Message, "RPM-2026-900003") || !strings.Contains(enq.Message, "Please call the office.") ||
		enq.ProfileSlug != "sample-plumbers" {
		t.Fatalf("enquiry: %+v", enq)
	}
	for _, banned := range []string{"Oven door", "hinge", "Tariro", "Kudzai", devseed.TenantEmail, "tenant"} {
		if strings.Contains(enq.Message, banned) || strings.Contains(enq.Service, banned) {
			t.Fatalf("the enquiry carries %q: %+v", banned, enq)
		}
	}
	if !e.auditHas(org, orgaudit.ActionMaintenanceAssigned) {
		t.Fatal("no MAINTENANCE_ASSIGNED audit record")
	}
	// Not from IN_PROGRESS.
	wantStatus(t, e.post(path+"/status", obj{"status": "IN_PROGRESS"}, agent), http.StatusOK)
	wantError(t, e.post(path+"/assign", obj{"directory_profile_id": plumbers}, agent), http.StatusConflict, maintenance.CodeInvalidTransition)
}

// A cost writes exactly one EXPENSE ledger entry (category maintenance,
// linked to the property), in the same transaction.
func TestMaintenanceCosts(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	path := maintenancePath(org, "/"+devseed.MaintenancePowerID.String())
	count := func() int {
		_, n, err := e.ledger.Entries(context.Background(), ledger.OrganisationBook(org), ledger.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	env := wantError(t, e.post(path+"/costs", obj{"amount": "-5", "currency": "XXX", "description": "", "incurred_on": "2099-01-01"}, agent),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["currency"] == "" || env.Error.Fields["description"] == "" || env.Error.Fields["incurred_on"] == "" {
		t.Fatalf("cost validation: %+v", env.Error.Fields)
	}
	wantError(t, e.post(path+"/costs", obj{"amount": 5, "currency": "USD", "description": "x"}, agent), http.StatusBadRequest, httpx.CodeInvalidJSON)
	// A failure after the checks keeps neither the cost nor the entry.
	e.maintenance.FailNext(errors.New("disk full"))
	wantStatus(t, e.post(path+"/costs", obj{"amount": "120.00", "currency": "USD", "description": "Electrician call-out"}, agent),
		http.StatusInternalServerError)
	if count() != before {
		t.Fatal("a ledger entry was kept without its cost")
	}
	rec := e.post(path+"/costs", obj{"amount": "120.00", "currency": "USD", "description": "Electrician call-out", "method": "CASH"}, agent)
	wantStatus(t, rec, http.StatusCreated)
	res := decode[struct {
		Cost struct {
			LedgerEntryID string `json:"ledger_entry_id"`
			LedgerStatus  string `json:"ledger_status"`
			Amount        string `json:"amount"`
		} `json:"cost"`
		Request maintenanceBody `json:"request"`
	}](t, rec)
	if count() != before+1 || res.Cost.LedgerStatus != ledger.StatusUnverified || res.Cost.Amount != "120.00" ||
		len(res.Request.CostTotals) != 1 || res.Request.CostTotals[0].Amount != "120.00" {
		t.Fatalf("cost: %+v", res)
	}
	entry, err := e.ledger.Entry(context.Background(), uuid.MustParse(res.Cost.LedgerEntryID))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Type != ledger.TypeExpense || entry.Category != "maintenance" || entry.PropertyID == nil ||
		*entry.PropertyID != devseed.PropertyCottageID || entry.Reference != "RPM-2026-900002" || entry.Method != "CASH" {
		t.Fatalf("ledger entry: %+v", entry)
	}
	ledgerView := e.get("/v1/agent/organisations/"+org.String()+"/ledger/entries/"+res.Cost.LedgerEntryID, agent).Body.String()
	if !strings.Contains(ledgerView, `"property_id":"`+devseed.PropertyCottageID.String()+`"`) {
		t.Fatalf("ledger view: %s", ledgerView)
	}
	if !e.auditHas(org, orgaudit.ActionMaintenanceCostRecorded) {
		t.Fatal("no MAINTENANCE_COST_RECORDED audit record")
	}
	// Reversing the entry in the ledger takes the cost out of the totals.
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	wantStatus(t, e.post("/v1/agent/organisations/"+org.String()+"/ledger/entries/"+res.Cost.LedgerEntryID+"/reverse",
		obj{"reason": "Entered twice."}, owner), http.StatusCreated)
	after := decode[maintenanceOne](t, e.get(path, agent)).Request
	if len(after.CostTotals) != 0 || len(after.Costs) != 1 || !after.Costs[0].Reversed {
		t.Fatalf("after reversal: %+v %+v", after.CostTotals, after.Costs)
	}
}

// Photos are images only (decided by the bytes), stored as JPEG and served
// privately.
func TestMaintenancePhotos(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	path := maintenancePath(org, "/"+devseed.MaintenanceOvenID.String()+"/photos")
	for name, content := range map[string][]byte{
		"pdf":  []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n"),
		"gif":  []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"),
		"svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"html": []byte("<html><body>hi</body></html>"),
		"fake": append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte("not really a jpeg")...),
	} {
		if rec := e.uploadTo(path, content, agent); rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	wantError(t, e.uploadTo(path, bytes.Repeat([]byte{0xff}, maintenance.MaxPhotoBytes+10), agent), http.StatusRequestEntityTooLarge,
		maintenance.CodeFileTooLarge)
	rec := e.uploadTo(path, maintenancePNG(t), agent)
	wantStatus(t, rec, http.StatusCreated)
	var ph struct {
		Photo struct {
			ID          string `json:"id"`
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
		} `json:"photo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ph); err != nil || ph.Photo.ContentType != "image/jpeg" {
		t.Fatalf("photo: %s", rec.Body.String())
	}
	got := e.get(ph.Photo.URL, agent)
	wantStatus(t, got, http.StatusOK)
	if got.Header().Get("Content-Type") != "image/jpeg" || got.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("photo headers: %v", got.Header())
	}
	if _, err := jpeg.Decode(bytes.NewReader(got.Body.Bytes())); err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	// The tenant reads it through their own route; another book never.
	wantStatus(t, e.get(myMaintenancePath(devseed.TenancyFlat1ID, "/"+devseed.MaintenanceOvenID.String()+"/photos/"+ph.Photo.ID), tenant),
		http.StatusOK)
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))
	wantError(t, e.get(ph.Photo.URL, blake), http.StatusNotFound, maintenance.CodeBookNotFound)
	wantError(t, e.get(maintenancePath(org, "/"+devseed.MaintenanceTapID.String()+"/photos/"+ph.Photo.ID), agent), http.StatusNotFound,
		maintenance.CodePhotoNotFound)
	// The limit.
	for i := 1; i < maintenance.MaxPhotos; i++ {
		wantStatus(t, e.uploadTo(path, testJPEG(t), agent), http.StatusCreated)
	}
	wantError(t, e.uploadTo(path, testJPEG(t), agent), http.StatusConflict, maintenance.CodePhotoLimit)
	wantStatus(t, e.do(http.MethodDelete, ph.Photo.URL, nil, agent), http.StatusNoContent)
	wantStatus(t, e.uploadTo(path, testJPEG(t), agent), http.StatusCreated)
}

// New requests and status changes are queued for the notifications, with
// ids, the reference and statuses only.
func TestMaintenanceNotificationsQueued(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	before := len(e.maintenance.Outbox())
	rec := e.post(myMaintenancePath(devseed.TenancyFlat1ID, ""), obj{"category": "plumbing", "priority": "high",
		"title": "Geyser leaking", "description": "Ring Tariro on +263771234567 or tenant@example.test"}, tenant)
	wantStatus(t, rec, http.StatusCreated)
	id := decode[maintenanceOne](t, rec).Request.ID
	wantStatus(t, e.post(maintenancePath(org, "/"+id+"/status"), obj{"status": "TRIAGED", "note": "Calling Tariro now"}, agent), http.StatusOK)
	wantStatus(t, e.post(maintenancePath(org, "/"+id+"/comments"), obj{"text": "Comment, no event"}, agent), http.StatusCreated)
	events := e.maintenance.Outbox()[before:]
	if len(events) != 2 || events[0].Type != maintenance.OutboxRequestCreated || events[1].Type != maintenance.OutboxStatusChanged {
		t.Fatalf("events: %+v", events)
	}
	for _, ev := range events {
		if ev.AggregateID.String() != id || ev.AggregateType != maintenance.AggregateMaintenance {
			t.Fatalf("event: %+v", ev)
		}
		payload := string(ev.Payload)
		for _, banned := range []string{"Geyser", "Ring", "+263", "@", "Tariro", "Calling", "Comment", "Avondale"} {
			if strings.Contains(payload, banned) {
				t.Fatalf("payload carries %q: %s", banned, payload)
			}
		}
	}
	var p maintenance.EventPayload
	if err := json.Unmarshal(events[1].Payload, &p); err != nil || p.FromStatus != "NEW" || p.ToStatus != "TRIAGED" || p.Reference == "" {
		t.Fatalf("payload: %+v %v", p, err)
	}
	// The source the notifications read: managers by book, the tenant with a verified account.
	info, err := MaintenanceNotices(e.maintenance, e.tenancies, e.crm, e.identity)(context.Background(), uuid.MustParse(id))
	if err != nil {
		t.Fatal(err)
	}
	if info.OrganisationID == nil || *info.OrganisationID != org || info.TenancyID == nil || len(info.Tenants) != 1 ||
		info.Tenants[0].Email != devseed.TenantEmail {
		t.Fatalf("notice: %+v", info)
	}
}
