package app

import (
	"bytes"
	"compress/zlib"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

type templateBody struct {
	ID          string  `json:"id"`
	FamilyID    string  `json:"family_id"`
	Version     int     `json:"version"`
	MarketCode  string  `json:"market_code"`
	Status      string  `json:"status"`
	LegalReview *obj    `json:"legal_review"`
	Description *string `json:"description"`
}

type templateEnvelope struct {
	Template   templateBody `json:"template"`
	NewVersion bool         `json:"new_version"`
}

type leaseBody struct {
	ID        string `json:"id"`
	Reference string `json:"reference"`
	Status    string `json:"status"`
	Template  struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	} `json:"template"`
	PropertyAddress string `json:"property_address"`
	Document        *struct {
		Kind   string `json:"kind"`
		SHA256 string `json:"sha256"`
	} `json:"document"`
	CanEdit bool `json:"can_edit"`
}

type leaseEnvelope struct {
	Lease leaseBody `json:"lease"`
}

type leasePage struct {
	Items []leaseBody `json:"items"`
	Total int         `json:"total"`
}

func (p leasePage) has(id string) bool {
	for _, l := range p.Items {
		if l.ID == id {
			return true
		}
	}
	return false
}

func (e *env) as(email string) reqOpt { return browser(e.loginCookie(email, seedPassword)) }

func (e *env) asAdmin(email string) reqOpt { return browser(e.adminLogin(email, seedPassword)) }

func pdfStreams(content []byte) string {
	var out strings.Builder
	re := regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	for _, m := range re.FindAllSubmatch(content, -1) {
		if r, err := zlib.NewReader(bytes.NewReader(m[1])); err == nil {
			raw, _ := io.ReadAll(r)
			out.Write(raw)
		}
	}
	return out.String()
}

func templateInput(market string, text string) obj {
	return obj{"market_code": market, "name": "Test lease " + market, "lease_kind": "RESIDENTIAL",
		"sections":         []obj{{"heading": "Rent", "text": text}},
		"optional_clauses": []obj{{"code": "NO_PETS", "title": "No pets", "text": "No pets at {{property_address}}."}}}
}

func leaseInput(templateID string) obj {
	return obj{
		"template_id": templateID, "listing_id": propertyrequests.SeedListingID(2).String(),
		"landlords":  []obj{{"name": "Zoë Müller", "email": "landlord@example.test"}},
		"tenants":    []obj{{"name": "Tariro Tenant", "email": "tenant@example.test", "phone": "+263 77 000 0000"}},
		"start_date": "2026-12-01", "end_date": "2027-11-30", "rent_amount": "950.00", "rent_currency": "USD",
		"rent_period": "MONTHLY", "payment_day": 1, "deposit_amount": "950", "notice_period_days": 30,
		"optional_clauses": []string{"NO_SMOKING"},
	}
}

const sampleZW = "0195c0de-0000-7000-8000-00000000e301"
const sampleGB = "0195c0de-0000-7000-8000-00000000e302"

func TestLeaseTemplateLibrary(t *testing.T) {
	e := newEnv(t)
	content, moderator := e.asAdmin(devseed.ContentAdminEmail), e.asAdmin(devseed.ModeratorEmail)
	agent := e.as("agent@example.test")

	wantError(t, e.post("/v1/admin/lease-templates", templateInput("ZW", "Rent {{rent_amount}}"), moderator), http.StatusForbidden, "FORBIDDEN")
	env := wantError(t, e.post("/v1/admin/lease-templates", templateInput("ZW", "Rent {{rent_amount}} and {{pet_name}}"), content), http.StatusBadRequest, "VALIDATION_FAILED")
	if !strings.Contains(env.Error.Fields["sections[0].text"], "{{pet_name}}") {
		t.Fatalf("unknown placeholder must be named: %v", env.Error.Fields)
	}
	wantError(t, e.post("/v1/admin/lease-templates", templateInput("XX", "Rent"), content), http.StatusBadRequest, "VALIDATION_FAILED")

	rec := e.post("/v1/admin/lease-templates", templateInput("ZW", "Rent {{rent_currency}} {{rent_amount}} per {{rent_period}}."), content)
	wantStatus(t, rec, http.StatusCreated)
	v1 := decode[templateEnvelope](t, rec).Template
	if v1.Status != "DRAFT" || v1.Version != 1 || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("created %+v", v1)
	}
	offered := func() string { return e.get("/v1/me/lease-templates?market=ZW", agent).Body.String() }
	if strings.Contains(offered(), `"id":"`+v1.ID) {
		t.Fatal("a draft template is never offered")
	}
	// A draft template cannot be used.
	wantError(t, e.post("/v1/me/leases", leaseInput(v1.ID), agent), http.StatusUnprocessableEntity, "LEASE_TEMPLATE_NOT_AVAILABLE")

	wantError(t, e.post("/v1/admin/lease-templates/"+v1.ID+"/approve", obj{}, content), http.StatusBadRequest, "VALIDATION_FAILED")
	review := obj{"legal_review": obj{"reviewer_name": "A. Advocate", "organisation": "Sample Chambers", "reviewed_on": "2026-10-30"}}
	rec = e.post("/v1/admin/lease-templates/"+v1.ID+"/approve", review, content)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[templateEnvelope](t, rec).Template; got.Status != "APPROVED" || got.LegalReview == nil {
		t.Fatalf("approved %+v", got)
	}
	if !strings.Contains(offered(), `"id":"`+v1.ID) || strings.Contains(e.get("/v1/me/lease-templates?market=GB", agent).Body.String(), v1.ID) {
		t.Fatal("an approved template is offered in its own country only")
	}

	// Editing an approved template creates the next draft version.
	rec = e.patch("/v1/admin/lease-templates/"+v1.ID, obj{"name": "Test lease ZW (2027)"}, content)
	wantStatus(t, rec, http.StatusCreated)
	v2 := decode[templateEnvelope](t, rec).Template
	if v2.Version != 2 || v2.Status != "DRAFT" || v2.FamilyID != v1.FamilyID || v2.ID == v1.ID {
		t.Fatalf("new version %+v", v2)
	}
	wantError(t, e.patch("/v1/admin/lease-templates/"+v1.ID, obj{"name": "Again"}, content), http.StatusConflict, "LEASE_TEMPLATE_DRAFT_EXISTS")
	wantError(t, e.patch("/v1/admin/lease-templates/"+v2.ID, obj{"market_code": "GB"}, content), http.StatusBadRequest, "VALIDATION_FAILED")
	wantStatus(t, e.patch("/v1/admin/lease-templates/"+v2.ID, obj{"description": "Updated"}, content), http.StatusOK)
	wantStatus(t, e.post("/v1/admin/lease-templates/"+v2.ID+"/approve", review, content), http.StatusOK)
	rec = e.get("/v1/admin/lease-templates/"+v1.ID, content)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[templateEnvelope](t, rec).Template; got.Status != "RETIRED" {
		t.Fatalf("approving v2 retires v1, got %s", got.Status)
	}
	if strings.Contains(offered(), `"id":"`+v1.ID) || !strings.Contains(offered(), `"id":"`+v2.ID) {
		t.Fatal("only the approved version is offered")
	}
	wantError(t, e.patch("/v1/admin/lease-templates/"+v1.ID, obj{"name": "x"}, content), http.StatusConflict, "LEASE_TEMPLATE_NOT_EDITABLE")
	wantError(t, e.post("/v1/me/leases", leaseInput(v1.ID), agent), http.StatusUnprocessableEntity, "LEASE_TEMPLATE_NOT_AVAILABLE")
	// A template of another country than the listing's.
	wantError(t, e.post("/v1/me/leases", leaseInput(sampleGB), agent), http.StatusUnprocessableEntity, "LEASE_MARKET_MISMATCH")
	forV2 := leaseInput(v2.ID)
	forV2["optional_clauses"] = []string{"NO_PETS"}
	wantStatus(t, e.post("/v1/me/leases", forV2, agent), http.StatusCreated)

	// The audit trail of an approval names the reviewer.
	if body := e.get("/v1/admin/lease-templates/"+v2.ID, content).Body.String(); !strings.Contains(body, `"APPROVED"`) || !strings.Contains(body, "A. Advocate") {
		t.Fatalf("approval not audited: %s", body)
	}
	// SUPER_ADMIN may too; a customer session may not.
	wantStatus(t, e.get("/v1/admin/lease-templates", e.asAdmin("admin@example.test")), http.StatusOK)
	wantError(t, e.get("/v1/admin/lease-templates", agent), http.StatusForbidden, "FORBIDDEN")
	// The sample templates say they were not reviewed.
	if body := offered(); !strings.Contains(body, "SAMPLE — not reviewed by a lawyer") {
		t.Fatalf("the sample template must say it is a sample: %s", body)
	}
}

func TestLeasePermissionsAndPDF(t *testing.T) {
	e := newEnv(t)
	agent := e.as("agent@example.test")
	rec := e.post("/v1/me/leases", leaseInput(sampleZW), agent)
	wantStatus(t, rec, http.StatusCreated)
	l := decode[leaseEnvelope](t, rec).Lease
	if l.Status != "DRAFT" || !regexp.MustCompile(`^RPL-2026-\d{6}$`).MatchString(l.Reference) || l.Document == nil || l.Document.Kind != "DRAFT" ||
		!strings.Contains(l.PropertyAddress, "Avondale") {
		t.Fatalf("created %+v", l)
	}
	path := "/v1/me/leases/" + l.ID

	// Who sees it.
	for email, want := range map[string]int{
		"agent@example.test": 200, "estates.owner@example.test": 200, "agency.admin@example.test": 200,
		"estates.agent2@example.test": 404, "agent-b@example.test": 404, "agency.staff@example.test": 403,
		"tenant@example.test": 403, "consumer@example.test": 403,
	} {
		if got := e.get(path, e.as(email)).Code; got != want {
			t.Fatalf("%s: GET = %d, want %d", email, got, want)
		}
	}
	if !decode[leasePage](t, e.get("/v1/me/leases", e.as("estates.owner@example.test"))).has(l.ID) ||
		decode[leasePage](t, e.get("/v1/me/leases", e.as("estates.agent2@example.test"))).has(l.ID) {
		t.Fatal("the owner lists every organisation lease; a colleague only their own")
	}
	// An agent cannot use a colleague's listing; staff cannot create.
	in := leaseInput(sampleZW)
	in["listing_id"] = propertyrequests.SeedListingID(7).String()
	wantError(t, e.post("/v1/me/leases", in, agent), http.StatusNotFound, "LISTING_NOT_FOUND")
	wantError(t, e.post("/v1/me/leases", leaseInput(sampleZW), e.as("agency.staff@example.test")), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.post("/v1/me/leases", leaseInput(sampleZW), e.as("tenant@example.test")), http.StatusForbidden, "FORBIDDEN")
	// The manager may change an agent's draft.
	wantStatus(t, e.patch(path, obj{"special_conditions": "The tenant may paint the second bedroom."}, e.as("agency.admin@example.test")), http.StatusOK)

	// The draft PDF: private, watermarked, carries the reference.
	rec = e.get(path+"/pdf", agent)
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Content-Type") != "application/pdf" || rec.Header().Get("Cache-Control") != "private, no-store" ||
		!bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
		t.Fatalf("headers %v", rec.Header())
	}
	text := pdfStreams(rec.Body.Bytes())
	if !strings.Contains(text, "(DRAFT) Tj") || !strings.Contains(text, "("+l.Reference+")") {
		t.Fatal("the draft must carry the watermark and the reference")
	}
	if leases.Checksum(rec.Body.Bytes()) != decode[leaseEnvelope](t, e.get(path, agent)).Lease.Document.SHA256 {
		t.Fatal("the recorded SHA-256 is the PDF's")
	}
	// Each save renders the draft again; only the newest object is kept.
	before := decode[leaseEnvelope](t, e.get(path, agent)).Lease.Document.SHA256
	rec = e.patch(path, obj{"rent_amount": "1000.00"}, agent)
	wantStatus(t, rec, http.StatusOK)
	after := decode[leaseEnvelope](t, rec).Lease.Document.SHA256
	if after == before {
		t.Fatal("an edit renders a new draft")
	}

	// Validation.
	bad := leaseInput(sampleZW)
	bad["end_date"], bad["rent_amount"], bad["optional_clauses"], bad["notice_period_days"] = "2026-11-01", "12.345", []string{"NOPE"}, 0
	bad["tenants"] = []obj{{"name": "", "email": "landlord@example.test"}}
	env := wantError(t, e.post("/v1/me/leases", bad, agent), http.StatusBadRequest, "VALIDATION_FAILED")
	for _, key := range []string{"end_date", "rent_amount", "optional_clauses[0]", "notice_period_days", "tenants[0].name", "tenants[0].email"} {
		if env.Error.Fields[key] == "" {
			t.Fatalf("missing message for %s: %v", key, env.Error.Fields)
		}
	}
	noListing := leaseInput(sampleZW)
	delete(noListing, "listing_id")
	wantError(t, e.post("/v1/me/leases", noListing, agent), http.StatusBadRequest, "VALIDATION_FAILED")
	noListing["property_address"] = "7 Example Lane, Harare"
	wantStatus(t, e.post("/v1/me/leases", noListing, agent), http.StatusCreated)

	// The audit trail.
	events := e.get(path+"/events", agent).Body.String()
	for _, action := range []string{`"CREATED"`, `"EDITED"`, `"PDF_GENERATED"`} {
		if !strings.Contains(events, action) {
			t.Fatalf("events lack %s: %s", action, events)
		}
	}
	// Administrators: SUPPORT reads every lease; a moderator does not.
	if !decode[leasePage](t, e.get("/v1/admin/leases?status=DRAFT", e.asAdmin(devseed.SupportEmail))).has(l.ID) {
		t.Fatal("support sees every lease")
	}
	wantError(t, e.get("/v1/admin/leases", e.asAdmin(devseed.ModeratorEmail)), http.StatusForbidden, "FORBIDDEN")

	// The dashboard.
	dash := e.get("/v1/me/dashboard", agent).Body.String()
	if !strings.Contains(dash, `"can_create_leases":true`) || !strings.Contains(dash, `"leases.drafts"`) {
		t.Fatalf("dashboard: %s", dash)
	}
	if strings.Contains(e.get("/v1/me/dashboard", e.as("agency.staff@example.test")).Body.String(), `"can_create_leases":true`) {
		t.Fatal("staff cannot create leases")
	}

	// Cancel, then delete a draft.
	rec = e.post(path+"/cancel", obj{"reason": "Wrong tenant"}, agent)
	wantStatus(t, rec, http.StatusOK)
	if decode[leaseEnvelope](t, rec).Lease.Status != "CANCELLED" {
		t.Fatal("cancelled")
	}
	wantError(t, e.patch(path, obj{"rent_amount": "1"}, agent), http.StatusConflict, "LEASE_NOT_EDITABLE")
	seed := "/v1/me/leases/" + devseed.SampleDraftLeaseID.String()
	wantStatus(t, e.get(seed, agent), http.StatusOK)
	wantStatus(t, e.deleteReq(seed, agent), http.StatusNoContent)
	wantError(t, e.get(seed, agent), http.StatusNotFound, "LEASE_NOT_FOUND")
}
