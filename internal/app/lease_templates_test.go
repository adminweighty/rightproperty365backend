package app

import (
	"bytes"
	"fmt"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

type orgTemplateBody struct {
	ID             string  `json:"id"`
	FamilyID       string  `json:"family_id"`
	Version        int     `json:"version"`
	OrganisationID *string `json:"organisation_id"`
	Source         string  `json:"source"`
	CanEdit        bool    `json:"can_edit"`
	Name           string  `json:"name"`
	Status         string  `json:"status"`
}

type orgTemplateList struct {
	Items []orgTemplateBody `json:"items"`
}

func (l orgTemplateList) find(id string) *orgTemplateBody {
	for i := range l.Items {
		if l.Items[i].ID == id {
			return &l.Items[i]
		}
	}
	return nil
}

// An organisation designs, publishes and retires its own templates: its
// owner and managers manage them, its agents read and use them, nobody else.
func TestOrganisationLeaseTemplates(t *testing.T) {
	e := newEnv(t)
	org := propertyrequests.SeedOrganisationID.String()
	base := "/v1/me/organisations/" + org + "/lease-templates"
	owner, manager := e.as("estates.owner@example.test"), e.as("agency.admin@example.test")
	agent, staff, other := e.as("agent@example.test"), e.as("agency.staff@example.test"), e.as("agent-b@example.test")

	in := templateInput("ZW", "Rent {{rent_currency}} {{rent_amount}} per {{rent_period}}.")
	in["name"] = "Sample Estates residential"
	delete(in, "optional_clauses")
	in["clauses"] = []obj{{"code": "NO_SMOKING", "title": "No smoking", "text": "No smoking at {{property_address}}."}}

	// Who may create.
	wantError(t, e.post(base, in, agent), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.post(base, in, staff), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.post(base, in, other), http.StatusNotFound, "ORGANISATION_NOT_FOUND")
	rec := e.post(base, in, owner)
	wantStatus(t, rec, http.StatusCreated)
	created := decode[struct {
		Template orgTemplateBody `json:"template"`
	}](t, rec).Template
	if created.Status != "DRAFT" || created.Source != "ORGANISATION" || created.OrganisationID == nil || *created.OrganisationID != org ||
		!created.CanEdit || created.Version != 1 {
		t.Fatalf("created %+v", created)
	}
	path := base + "/" + created.ID

	// Who may read.
	for who, want := range map[string]int{"estates.owner@example.test": 200, "agency.admin@example.test": 200,
		"agent@example.test": 200, "agency.staff@example.test": 403, "agent-b@example.test": 404} {
		if got := e.get(path, e.as(who)).Code; got != want {
			t.Fatalf("%s: GET = %d, want %d", who, got, want)
		}
	}
	list := decode[orgTemplateList](t, e.get(base, agent))
	if got := list.find(created.ID); got == nil || got.CanEdit {
		t.Fatalf("an agent reads the organisation's templates without editing them: %+v", got)
	}
	// A sample is not one of the organisation's templates.
	wantError(t, e.get(base+"/"+sampleZW, owner), http.StatusNotFound, "LEASE_TEMPLATE_NOT_FOUND")
	wantError(t, e.patch(base+"/"+sampleZW, obj{"name": "Mine now"}, owner), http.StatusNotFound, "LEASE_TEMPLATE_NOT_FOUND")

	// A draft is not offered; publishing needs no review; a manager may.
	if strings.Contains(e.get("/v1/me/lease-templates?market=ZW", agent).Body.String(), created.ID) {
		t.Fatal("a draft is never offered")
	}
	wantStatus(t, e.patch(path, obj{"description": "Our standard lease"}, manager), http.StatusOK)
	wantError(t, e.post(path+"/publish", nil, agent), http.StatusForbidden, "FORBIDDEN")
	rec = e.post(path+"/publish", nil, manager)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[struct {
		Template orgTemplateBody `json:"template"`
	}](t, rec).Template; got.Status != "APPROVED" {
		t.Fatalf("published %+v", got)
	}

	// Offered to the organisation's members, with every sample; not to others.
	offered := decode[orgTemplateList](t, e.get("/v1/me/lease-templates?market=ZW", agent))
	if offered.find(created.ID) == nil || offered.find(sampleZW) == nil || offered.find(sampleZW).Source != "SAMPLE" {
		t.Fatalf("offered to the agent: %+v", offered.Items)
	}
	if decode[orgTemplateList](t, e.get("/v1/me/lease-templates?market=ZW&organisation_id="+org, owner)).find(created.ID) == nil ||
		!decode[orgTemplateList](t, e.get("/v1/me/lease-templates?market=ZW", owner)).find(created.ID).CanEdit {
		t.Fatal("offered to the owner, who may edit it")
	}
	if decode[orgTemplateList](t, e.get("/v1/me/lease-templates?market=ZW", other)).find(created.ID) != nil {
		t.Fatal("another organisation's template is never offered")
	}
	wantError(t, e.get("/v1/me/lease-templates?organisation_id="+org, other), http.StatusBadRequest, "VALIDATION_FAILED")

	// The agent drafts a lease from it; another organisation's agent cannot.
	wantStatus(t, e.post("/v1/me/leases", leaseInput(created.ID), agent), http.StatusCreated)
	foreign := leaseInput(created.ID)
	delete(foreign, "listing_id")
	foreign["property_address"] = "7 Example Lane, Harare"
	env := wantError(t, e.post("/v1/me/leases", foreign, other), http.StatusBadRequest, "VALIDATION_FAILED")
	if env.Error.Fields["template_id"] == "" {
		t.Fatalf("fields %v", env.Error.Fields)
	}

	// Editing a published template makes the next draft version.
	rec = e.patch(path, obj{"name": "Sample Estates residential (2027)"}, owner)
	wantStatus(t, rec, http.StatusCreated)
	v2 := decode[struct {
		Template orgTemplateBody `json:"template"`
	}](t, rec).Template
	if v2.Version != 2 || v2.FamilyID != created.FamilyID || v2.Status != "DRAFT" {
		t.Fatalf("new version %+v", v2)
	}
	// A draft can be retired too; then the published one.
	wantStatus(t, e.post(base+"/"+v2.ID+"/retire", nil, owner), http.StatusOK)
	wantStatus(t, e.post(path+"/retire", nil, owner), http.StatusOK)
	wantError(t, e.post(path+"/retire", nil, owner), http.StatusConflict, "LEASE_TEMPLATE_STATE_CONFLICT")
	events := e.get(path+"/events", agent).Body.String()
	for _, action := range []string{`"CREATED"`, `"EDITED"`, `"APPROVED"`, `"RETIRED"`} {
		if !strings.Contains(events, action) {
			t.Fatalf("events lack %s: %s", action, events)
		}
	}

	// A copy of a sample is a new draft of the organisation.
	rec = e.post(base, obj{"copy_from_template_id": sampleZW}, manager)
	wantStatus(t, rec, http.StatusCreated)
	cp := decode[struct {
		Template orgTemplateBody `json:"template"`
	}](t, rec).Template
	if !strings.HasPrefix(cp.Name, "Copy of ") || cp.Status != "DRAFT" || cp.Source != "ORGANISATION" || cp.FamilyID == sampleZW {
		t.Fatalf("copy %+v", cp)
	}
	wantError(t, e.post(base, obj{"copy_from_template_id": "0195c0de-0000-7000-8000-0000000000ff"}, manager), http.StatusBadRequest, "VALIDATION_FAILED")
	// The sample library is the administrators' and stays as it was.
	if strings.Contains(e.get("/v1/admin/lease-templates", e.asAdmin("admin@example.test")).Body.String(), created.ID) {
		t.Fatal("organisation templates are not in the sample library")
	}
}

// The stamp preview is public and returns a PNG; the saved stamp lives with
// the saved signature; a lease can ask landlords for a stamp, which the
// landlord must apply before signing.
func TestStampPreviewSavedStampAndLandlordStamp(t *testing.T) {
	e := newEnv(t)
	stamp := obj{"shape": "ROUND", "colour": "BLUE", "top_text": "Lloyd Landlord Properties", "centre_lines": []string{"Harare"},
		"bottom_text": "Reg no 1234/2020", "include_date": true}

	rec := e.signer(http.MethodPost, "/v1/stamp-preview", obj{"stamp": stamp, "date": "2026-11-16"})
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("headers %v", rec.Header())
	}
	img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil || img.Bounds().Dx() != 600 {
		t.Fatalf("preview: %v %v", err, img)
	}
	env := wantError(t, e.signer(http.MethodPost, "/v1/stamp-preview", obj{"stamp": obj{"shape": "ROUND", "top_text": ""}, "date": "16/11/2026"}),
		http.StatusBadRequest, "VALIDATION_FAILED")
	if env.Error.Fields["stamp.top_text"] == "" || env.Error.Fields["date"] == "" {
		t.Fatalf("fields %v", env.Error.Fields)
	}

	// The saved stamp: kept when left out, cleared by null.
	landlord := e.as("landlord@example.test")
	wantStatus(t, e.do(http.MethodPut, "/v1/me/signature", obj{"signature": obj{"kind": "TYPED", "text": "Lloyd Landlord"}, "stamp": stamp}, landlord), http.StatusOK)
	if body := e.get("/v1/me/signature", landlord).Body.String(); !strings.Contains(body, `"top_text":"LLOYD LANDLORD PROPERTIES"`) {
		t.Fatalf("saved stamp: %s", body)
	}
	wantStatus(t, e.do(http.MethodPut, "/v1/me/signature", obj{"signature": obj{"kind": "TYPED", "text": "Lloyd Landlord"}}, landlord), http.StatusOK)
	if body := e.get("/v1/me/signature", landlord).Body.String(); !strings.Contains(body, `"top_text":"LLOYD LANDLORD PROPERTIES"`) {
		t.Fatalf("a PUT without the stamp keeps it: %s", body)
	}

	// A lease asking for the landlord's stamp.
	agent := e.as("agent@example.test")
	in := leaseInput(sampleZW)
	in["landlord_stamp"] = true
	rec = e.post("/v1/me/leases", in, agent)
	wantStatus(t, rec, http.StatusCreated)
	l := decode[leaseEnvelope](t, rec).Lease
	if !strings.Contains(rec.Body.String(), `"landlord_stamp":true`) {
		t.Fatal("the lease says it asks for the landlord's stamp")
	}
	rec = e.post("/v1/me/leases/"+l.ID+"/send", nil, agent)
	wantStatus(t, rec, http.StatusOK)
	agreementID := decode[struct {
		Lease struct {
			AgreementID string `json:"agreement_id"`
		} `json:"lease"`
	}](t, rec).Lease.AgreementID
	a := e.agreement(agreementID, agent)
	stamps := 0
	for _, f := range a.Fields {
		if f.Type == "STAMP" {
			stamps++
		}
	}
	if stamps != 1 {
		t.Fatalf("%d STAMP fields, want one for the landlord", stamps)
	}
	token := e.mail.signingFor("landlord@example.test")
	// Signed in, the landlord's signing page offers the saved stamp.
	rec = e.do(http.MethodGet, "/v1/signing/"+token, nil, landlord, withIP("203.0.200.1"))
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"saved_stamp":{`) || !strings.Contains(rec.Body.String(), `"type":"STAMP"`) {
		t.Fatalf("signing page: %s", rec.Body.String())
	}
	env = wantError(t, e.signWith(token, "landlord@example.test", nil), http.StatusBadRequest, "VALIDATION_FAILED")
	if env.Error.Fields["stamp"] == "" {
		t.Fatalf("the stamp is required: %v", env.Error.Fields)
	}
	wantStatus(t, e.signer(http.MethodPost, "/v1/signing/"+token+"/submit", obj{"code": e.mail.codeFor("landlord@example.test"), "consent": true,
		"signature": obj{"kind": "TYPED", "text": "Lloyd Landlord"}, "stamp": stamp}), http.StatusOK)
	rec = e.signWith(e.mail.signingFor("tenant@example.test"), "tenant@example.test", nil)
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"agreement_status":"COMPLETED"`) {
		t.Fatalf("completed? %s", rec.Body.String())
	}
	if events := e.get(fmt.Sprintf("/v1/me/agreements/%s/events", agreementID), agent).Body.String(); !strings.Contains(events, `"stamp_top_text":"LLOYD LANDLORD PROPERTIES"`) {
		t.Fatalf("the stamp is audited: %s", events)
	}

	wantStatus(t, e.do(http.MethodPut, "/v1/me/signature", obj{"stamp": nil}, landlord), http.StatusOK)
	if body := e.get("/v1/me/signature", landlord).Body.String(); !strings.Contains(body, `"stamp":null`) {
		t.Fatalf("null clears the saved stamp: %s", body)
	}
}
