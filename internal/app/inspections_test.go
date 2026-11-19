package app

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/inspections"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

func inspectionPath(book uuid.UUID, rest string) string {
	return "/v1/agent/organisations/" + book.String() + "/inspections" + rest
}

func myInspectionPath(tenancy uuid.UUID, rest string) string {
	return "/v1/me/tenancies/" + tenancy.String() + "/inspections" + rest
}

type inspectionItemBody struct {
	ID     string  `json:"id"`
	Label  string  `json:"label"`
	Status *string `json:"status"`
	Note   string  `json:"note"`
	Photos []struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"photos"`
	Baseline *struct {
		Status *string `json:"status"`
		Note   string  `json:"note"`
	} `json:"baseline"`
}

type inspectionBody struct {
	ID        string `json:"id"`
	Reference string `json:"reference"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	TenancyID string `json:"tenancy_id"`
	Template  struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	} `json:"template"`
	Inspector struct {
		Name string `json:"name"`
	} `json:"inspector"`
	Progress inspections.Progress `json:"progress"`
	Areas    []struct {
		ID    string               `json:"id"`
		Label string               `json:"label"`
		Items []inspectionItemBody `json:"items"`
	} `json:"areas"`
	Summary  string `json:"summary"`
	Baseline *struct {
		Reference string `json:"reference"`
	} `json:"baseline"`
	ReportURL string `json:"report_url"`
	Can       struct {
		Edit           bool `json:"edit"`
		RecordFindings bool `json:"record_findings"`
		Start          bool `json:"start"`
		Complete       bool `json:"complete"`
		Cancel         bool `json:"cancel"`
	} `json:"can"`
}

type inspectionOne struct {
	Inspection inspectionBody `json:"inspection"`
}

func (b inspectionBody) itemIDs() []string {
	var out []string
	for _, a := range b.Areas {
		for _, it := range a.Items {
			out = append(out, it.ID)
		}
	}
	return out
}

func newInspection(t *testing.T, e *env, as reqOpt, body obj) inspectionBody {
	t.Helper()
	rec := e.post(inspectionPath(devseed.SampleEstates.ID, ""), body, as)
	wantStatus(t, rec, http.StatusCreated)
	return decode[inspectionOne](t, rec).Inspection
}

func flat1Inspection(typ string) obj {
	return obj{"property_id": devseed.PropertyAvondaleID.String(), "tenancy_id": devseed.TenancyFlat1ID.String(), "type": typ,
		"scheduled_for": "2026-12-01T09:00:00Z", "checklist_template": "residential"}
}

// fillAll records a finding on every item.
func fillAll(t *testing.T, e *env, as reqOpt, path string, in inspectionBody, status string) inspectionBody {
	t.Helper()
	var lines []obj
	for _, id := range in.itemIDs() {
		lines = append(lines, obj{"item_id": id, "status": status})
	}
	rec := e.do(http.MethodPut, path+"/findings", obj{"items": lines}, as)
	wantStatus(t, rec, http.StatusOK)
	return decode[inspectionOne](t, rec).Inspection
}

// Every agent endpoint answers another organisation's member, a consumer,
// the tenant and an administrator 404 BOOK_NOT_FOUND, and never data.
func TestInspectionsWrongOrganisation(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	iid := devseed.InspectionRoutineID.String()
	photo, item := uuid.NewString(), uuid.NewString()
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	admin := browser(e.adminLogin(identity.SeedAdminEmail, seedPassword))
	calls := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, inspectionPath(org, ""), nil},
		{http.MethodGet, inspectionPath(org, "?status=bad"), nil},
		{http.MethodGet, inspectionPath(org, "/options"), nil},
		{http.MethodPost, inspectionPath(org, ""), flat1Inspection("ROUTINE")},
		{http.MethodPost, inspectionPath(org, ""), obj{"type": 1}},
		{http.MethodGet, inspectionPath(org, "/"+iid), nil},
		{http.MethodGet, inspectionPath(org, "/not-a-uuid"), nil},
		{http.MethodPatch, inspectionPath(org, "/"+iid), obj{"scheduled_for": "2026-12-02T09:00:00Z"}},
		{http.MethodPut, inspectionPath(org, "/"+iid+"/checklist"), obj{"areas": []obj{{"label": "x", "items": []obj{{"label": "y"}}}}}},
		{http.MethodPut, inspectionPath(org, "/"+iid+"/findings"), obj{"items": []obj{{"item_id": item, "status": "OK"}}}},
		{http.MethodPost, inspectionPath(org, "/"+iid+"/start"), nil},
		{http.MethodPost, inspectionPath(org, "/"+iid+"/complete"), obj{}},
		{http.MethodPost, inspectionPath(org, "/"+iid+"/cancel"), obj{"reason": "x"}},
		{http.MethodGet, inspectionPath(org, "/"+iid+"/report.pdf"), nil},
		{http.MethodGet, inspectionPath(org, "/"+iid+"/photos/"+photo), nil},
		{http.MethodDelete, inspectionPath(org, "/"+iid+"/photos/"+photo), nil},
	}
	for who, as := range map[string]reqOpt{"another organisation": blake, "consumer": consumer, "tenant": tenant, "administrator": admin} {
		for _, c := range calls {
			rec := e.do(c.method, c.path, c.body, as)
			wantError(t, rec, http.StatusNotFound, inspections.CodeBookNotFound)
			if body := rec.Body.String(); strings.Contains(body, "Avondale") || strings.Contains(body, "RPI-") || strings.Contains(body, "%PDF") {
				t.Fatalf("%s %s %s: %s", who, c.method, c.path, body)
			}
		}
		wantError(t, e.uploadTo(inspectionPath(org, "/"+iid+"/items/"+item+"/photos"), testJPEG(t), as), http.StatusNotFound,
			inspections.CodeBookNotFound)
	}
	wantError(t, e.get(inspectionPath(org, "")), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	// Blake's own book does not reach Sample Estates' inspections, property or tenancy.
	second := devseed.SecondStreetRealty.ID
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, inspectionPath(second, "/"+iid), nil},
		{http.MethodGet, inspectionPath(second, "/"+iid+"/report.pdf"), nil},
		{http.MethodPost, inspectionPath(second, "/"+iid+"/start"), nil},
		{http.MethodPost, inspectionPath(second, "/"+iid+"/cancel"), obj{"reason": "x"}},
		{http.MethodPut, inspectionPath(second, "/"+iid+"/findings"), obj{"items": []obj{{"item_id": item, "status": "OK"}}}},
	} {
		wantError(t, e.do(c.method, c.path, c.body, blake), http.StatusNotFound, inspections.CodeNotFound)
	}
	wantError(t, e.uploadTo(inspectionPath(second, "/"+iid+"/items/"+item+"/photos"), testJPEG(t), blake), http.StatusNotFound,
		inspections.CodeNotFound)
	if body := e.get(inspectionPath(second, ""), blake).Body.String(); !strings.Contains(body, `"total":0`) {
		t.Fatalf("second street: %s", body)
	}
	env := wantError(t, e.post(inspectionPath(second, ""), flat1Inspection("ROUTINE"), blake), http.StatusBadRequest,
		httpx.CodeValidationFailed)
	if env.Error.Fields["property_id"] == "" {
		t.Fatalf("cross-book property: %+v", env.Error.Fields)
	}
	body := flat1Inspection("ROUTINE")
	body["property_id"] = devseed.PropertySecondStreetID.String()
	env = wantError(t, e.post(inspectionPath(second, ""), body, blake), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["tenancy_id"] == "" {
		t.Fatalf("cross-book tenancy: %+v", env.Error.Fields)
	}
}

// The seed, STAFF read only, the state machine, partial findings, the
// completion rule, and the freeze: every write on a completed inspection
// is 409.
func TestInspectionsLifecycle(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))

	page := decode[struct {
		Items        []inspectionBody `json:"items"`
		Total        int              `json:"total"`
		StatusCounts map[string]int   `json:"status_counts"`
	}](t, e.get(inspectionPath(org, ""), staff))
	if page.Total != 2 || page.StatusCounts["SCHEDULED"] != 1 || page.StatusCounts["COMPLETED"] != 1 || len(page.StatusCounts) != 4 ||
		page.Items[0].Reference != "RPI-2026-900002" {
		t.Fatalf("seed list: %d %+v", page.Total, page.StatusCounts)
	}
	moveIn := decode[inspectionOne](t, e.get(inspectionPath(org, "/"+devseed.InspectionMoveInID.String()), staff)).Inspection
	if moveIn.Status != "COMPLETED" || moveIn.Progress.Items != 32 || moveIn.Progress.Done != 32 || moveIn.Progress.Attention != 2 ||
		moveIn.Progress.Damaged != 1 || moveIn.Progress.NA != 3 || moveIn.Can.Edit || moveIn.Inspector.Name != "Chipo Dube" {
		t.Fatalf("seeded move-in: %+v", moveIn)
	}
	opts := e.get(inspectionPath(org, "/options"), staff).Body.String()
	for _, want := range []string{`"write":false`, `"code":"residential"`, `"code":"commercial"`, `"residential-kitchen-sink"`, `"NA"`,
		`"max_photos_per_item":6`} {
		if !strings.Contains(opts, want) {
			t.Fatalf("options lack %s: %s", want, opts)
		}
	}
	// STAFF may not write.
	wantError(t, e.post(inspectionPath(org, ""), flat1Inspection("ROUTINE"), staff), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.post(inspectionPath(org, "/"+devseed.InspectionRoutineID.String()+"/start"), nil, staff), http.StatusForbidden,
		httpx.CodeForbidden)
	// Validation.
	env := wantError(t, e.post(inspectionPath(org, ""), obj{"property_id": devseed.PropertyAvondaleID.String(), "unit_id": uuid.NewString(),
		"tenancy_id": devseed.TenancyCottageID.String(), "type": "ANNUAL", "scheduled_for": "tomorrow", "checklist_template": "nope",
		"inspector_user_id": identity.SeedAgentBUserID.String()}, agent), http.StatusBadRequest, httpx.CodeValidationFailed)
	for _, f := range []string{"unit_id", "type", "scheduled_for", "checklist_template", "inspector_user_id"} {
		if env.Error.Fields[f] == "" {
			t.Fatalf("validation %s: %+v", f, env.Error.Fields)
		}
	}

	in := newInspection(t, e, agent, flat1Inspection("ROUTINE"))
	if in.Status != "SCHEDULED" || !strings.HasPrefix(in.Reference, "RPI-2026-") || in.Template.Code != "residential" ||
		in.Progress.Items != 32 || in.Progress.Done != 0 || !in.Can.Start || in.Can.Complete || in.TenancyID != devseed.TenancyFlat1ID.String() {
		t.Fatalf("created: %+v", in)
	}
	path := inspectionPath(org, "/"+in.ID)
	// Complete from SCHEDULED is not a move; the error names the allowed ones.
	rec := e.post(path+"/complete", obj{}, agent)
	wantError(t, rec, http.StatusConflict, inspections.CodeInvalidTransition)
	if !strings.Contains(rec.Body.String(), `"allowed":["IN_PROGRESS","CANCELLED"]`) {
		t.Fatalf("transition error: %s", rec.Body.String())
	}
	// The checklist: drop the second bedroom, rename the kitchen, add an item.
	var areas []obj
	for _, a := range in.Areas {
		if a.Label == "Second bedroom" {
			continue
		}
		label := a.Label
		if label == "Kitchen" {
			label = "Kitchen and scullery"
		}
		var items []obj
		for _, it := range a.Items {
			items = append(items, obj{"id": it.ID, "label": it.Label})
		}
		if a.Label == "Kitchen" {
			items = append(items, obj{"label": "Dishwasher"})
		}
		areas = append(areas, obj{"id": a.ID, "label": label, "items": items})
	}
	env = wantError(t, e.do(http.MethodPut, path+"/checklist", obj{"areas": []obj{{"id": uuid.NewString(), "label": "x",
		"items": []obj{{"id": moveIn.Areas[0].Items[0].ID, "label": "y"}}}}}, agent), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["areas[0].id"] == "" || env.Error.Fields["areas[0].items[0].id"] == "" {
		t.Fatalf("checklist ids: %+v", env.Error.Fields)
	}
	rec = e.do(http.MethodPut, path+"/checklist", obj{"areas": areas}, agent)
	wantStatus(t, rec, http.StatusOK)
	in = decode[inspectionOne](t, rec).Inspection
	if in.Progress.Items != 30 || len(in.Areas) != 7 || in.Areas[2].Label != "Kitchen and scullery" || in.Status != "SCHEDULED" {
		t.Fatalf("restructured: %+v", in.Progress)
	}
	// Partial findings: one item, and the inspection starts.
	first, second := in.Areas[0].Items[0].ID, in.Areas[0].Items[1].ID
	rec = e.do(http.MethodPut, path+"/findings", obj{"items": []obj{{"item_id": first, "status": "ATTENTION", "note": "Lock is stiff"}}}, agent)
	wantStatus(t, rec, http.StatusOK)
	in = decode[inspectionOne](t, rec).Inspection
	if in.Status != "IN_PROGRESS" || in.Progress.Done != 1 || *in.Areas[0].Items[0].Status != "ATTENTION" || !in.Can.Complete || in.Can.Start {
		t.Fatalf("after one finding: %+v", in)
	}
	// An absent note keeps it; null clears a status; bad values are 400.
	rec = e.do(http.MethodPut, path+"/findings", obj{"items": []obj{{"item_id": first, "status": "DAMAGED"},
		{"item_id": second, "status": "OK"}}}, agent)
	in = decode[inspectionOne](t, rec).Inspection
	if in.Areas[0].Items[0].Note != "Lock is stiff" || *in.Areas[0].Items[0].Status != "DAMAGED" || in.Progress.Done != 2 {
		t.Fatalf("partial update: %+v", in.Areas[0].Items[:2])
	}
	rec = e.do(http.MethodPut, path+"/findings", obj{"items": []obj{{"item_id": second, "status": nil}}}, agent)
	if in = decode[inspectionOne](t, rec).Inspection; in.Areas[0].Items[1].Status != nil {
		t.Fatalf("null status: %+v", in.Areas[0].Items[1])
	}
	env = wantError(t, e.do(http.MethodPut, path+"/findings", obj{"items": []obj{{"item_id": first, "status": "BROKEN"},
		{"item_id": uuid.NewString(), "status": "OK"}}}, agent), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["items[0].status"] == "" || env.Error.Fields["items[1].item_id"] == "" {
		t.Fatalf("findings validation: %+v", env.Error.Fields)
	}
	// Completing needs every item: the answer names the missing ones.
	rec = e.post(path+"/complete", obj{"summary": "Fine."}, agent)
	wantError(t, rec, http.StatusConflict, inspections.CodeIncomplete)
	if !strings.Contains(rec.Body.String(), `"missing":29`) {
		t.Fatalf("incomplete: %s", rec.Body.String())
	}
	fillAll(t, e, agent, path, in, "OK")
	wantError(t, e.post(path+"/start", nil, agent), http.StatusConflict, inspections.CodeInvalidTransition)
	rec = e.post(path+"/complete", obj{"summary": "All in order.", "tenant_sign_off_name": "Kudzai Sithole",
		"agent_sign_off_name": "Tendai Ncube"}, agent)
	wantStatus(t, rec, http.StatusOK)
	done := decode[inspectionOne](t, rec).Inspection
	if done.Status != "COMPLETED" || done.Summary != "All in order." || done.Can.Edit || done.Can.Cancel || done.Can.Complete {
		t.Fatalf("completed: %+v", done)
	}
	if !e.auditHas(org, orgaudit.ActionInspectionCompleted) {
		t.Fatal("no INSPECTION_COMPLETED audit record")
	}
	// Frozen: every write is 409 INSPECTION_CLOSED, and nothing changed.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, path, obj{"scheduled_for": "2026-12-05T09:00:00Z"}},
		{http.MethodPut, path + "/checklist", obj{"areas": []obj{{"label": "x", "items": []obj{{"label": "y"}}}}}},
		{http.MethodPut, path + "/findings", obj{"items": []obj{{"item_id": first, "status": "NA"}}}},
		{http.MethodPost, path + "/start", nil},
		{http.MethodPost, path + "/complete", obj{}},
		{http.MethodPost, path + "/cancel", obj{"reason": "x"}},
		{http.MethodDelete, path + "/photos/" + uuid.NewString(), nil},
	} {
		rec := e.do(c.method, c.path, c.body, agent)
		wantError(t, rec, http.StatusConflict, inspections.CodeClosed)
		if !strings.Contains(rec.Body.String(), `"status":"COMPLETED"`) {
			t.Fatalf("%s %s: %s", c.method, c.path, rec.Body.String())
		}
	}
	wantError(t, e.uploadTo(path+"/items/"+first+"/photos", testJPEG(t), agent), http.StatusConflict, inspections.CodeClosed)
	after := decode[inspectionOne](t, e.get(path, agent)).Inspection
	if after.Summary != "All in order." || *after.Areas[0].Items[0].Status != "OK" || after.Progress.Items != 30 {
		t.Fatalf("a frozen inspection changed: %+v", after)
	}
	// The PDF renders.
	pdf := e.get(path+"/report.pdf", staff)
	wantStatus(t, pdf, http.StatusOK)
	if !bytes.HasPrefix(pdf.Body.Bytes(), []byte("%PDF")) || pdf.Body.Len() < 1000 || pdf.Header().Get("Content-Type") != "application/pdf" ||
		!strings.Contains(pdf.Header().Get("Content-Disposition"), "inspection-"+done.Reference+".pdf") {
		t.Fatalf("report: %d bytes, %v", pdf.Body.Len(), pdf.Header())
	}

	// Cancel needs a reason, is audited, and freezes too.
	other := newInspection(t, e, agent, flat1Inspection("ROUTINE"))
	opath := inspectionPath(org, "/"+other.ID)
	env = wantError(t, e.post(opath+"/cancel", obj{"reason": ""}, agent), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["reason"] == "" {
		t.Fatalf("cancel without reason: %+v", env.Error.Fields)
	}
	wantStatus(t, e.do(http.MethodPatch, opath, obj{"scheduled_for": "2026-12-03T10:00:00Z"}, agent), http.StatusOK)
	wantStatus(t, e.post(opath+"/cancel", obj{"reason": "The tenant asked for another week."}, agent), http.StatusOK)
	if !e.auditHas(org, orgaudit.ActionInspectionCancelled) {
		t.Fatal("no INSPECTION_CANCELLED audit record")
	}
	wantError(t, e.post(opath+"/start", nil, agent), http.StatusConflict, inspections.CodeClosed)
	wantStatus(t, e.get(opath+"/report.pdf", agent), http.StatusOK)

	// A stale write (decided on an older version) is 409 INSPECTION_CHANGED.
	third := newInspection(t, e, agent, flat1Inspection("ROUTINE"))
	stored, err := e.inspections.Inspection(context.Background(), uuid.MustParse(third.ID))
	if err != nil {
		t.Fatal(err)
	}
	next := stored
	next.Version++
	if err := e.inspections.Apply(context.Background(), inspections.Change{Expect: map[uuid.UUID]inspections.Expect{stored.ID: {
		Status: stored.Status, Version: stored.Version}}, Inspections: []inspections.Inspection{next}}); err != nil {
		t.Fatal(err)
	}
	if err := e.inspections.Apply(context.Background(), inspections.Change{Expect: map[uuid.UUID]inspections.Expect{stored.ID: {
		Status: stored.Status, Version: stored.Version}}, Inspections: []inspections.Inspection{next}}); err != inspections.ErrConflict {
		t.Fatalf("stale write: %v", err)
	}
}

// The checklist is a copy: editing the template's reference rows never
// changes an existing inspection.
func TestInspectionTemplateEditsLeaveInspectionsAlone(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	before := newInspection(t, e, agent, flat1Inspection("ROUTINE"))
	// An administrator relabels an area, deactivates an item and adds one.
	parent := "residential"
	e.reference.Put(reference.SetInspectionChecklistTemplates, reference.Item{Code: "residential-kitchen", Label: "Cooking area",
		SortOrder: 30, ParentCode: &parent, Active: true})
	if !e.reference.SetActive(reference.SetInspectionChecklistTemplates, "residential-kitchen-sink", false) {
		t.Fatal("no sink row")
	}
	area := "residential-bathroom"
	e.reference.Put(reference.SetInspectionChecklistTemplates, reference.Item{Code: "residential-bathroom-mirror", Label: "Mirror",
		SortOrder: 60, ParentCode: &area, Active: true})
	after := decode[inspectionOne](t, e.get(inspectionPath(org, "/"+before.ID), agent)).Inspection
	a, _ := json.Marshal(before.Areas)
	b, _ := json.Marshal(after.Areas)
	if string(a) != string(b) {
		t.Fatalf("the template edit changed an existing inspection:\n%s\n%s", a, b)
	}
	if !strings.Contains(string(b), `"Kitchen"`) || !strings.Contains(string(b), `"Sink and taps"`) || strings.Contains(string(b), "Mirror") {
		t.Fatalf("the existing checklist: %s", b)
	}
	// A new inspection copies the edited template.
	fresh := newInspection(t, e, agent, flat1Inspection("ROUTINE"))
	c, _ := json.Marshal(fresh.Areas)
	if !strings.Contains(string(c), `"Cooking area"`) || strings.Contains(string(c), `"Sink and taps"`) || !strings.Contains(string(c), "Mirror") {
		t.Fatalf("the new checklist: %s", c)
	}
}

// Photos are images only (decided by the bytes), per item, capped, stored
// as JPEG without metadata and served privately.
func TestInspectionPhotos(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	in := newInspection(t, e, agent, flat1Inspection("ROUTINE"))
	base := inspectionPath(org, "/"+in.ID)
	item := in.Areas[0].Items[0].ID
	path := base + "/items/" + item + "/photos"
	for name, content := range map[string][]byte{
		"pdf":  []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\n"),
		"gif":  []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"),
		"svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"html": []byte("<html><body>hi</body></html>"),
		"fake": append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte("not really a jpeg")...),
	} {
		wantError(t, e.uploadTo(path, content, agent), http.StatusUnsupportedMediaType, inspections.CodeUnsupportedFile)
		_ = name
	}
	wantError(t, e.uploadTo(path, bytes.Repeat([]byte{0xff}, inspections.MaxPhotoBytes+10), agent), http.StatusRequestEntityTooLarge,
		inspections.CodeFileTooLarge)
	wantError(t, e.uploadTo(base+"/items/"+uuid.NewString()+"/photos", testJPEG(t), agent), http.StatusNotFound, inspections.CodeItemNotFound)
	// Nothing was stored by the refusals, and the inspection is still SCHEDULED.
	if got := decode[inspectionOne](t, e.get(base, agent)).Inspection; got.Status != "SCHEDULED" {
		t.Fatalf("after refused uploads: %s", got.Status)
	}
	rec := e.uploadTo(path, maintenancePNG(t), agent)
	wantStatus(t, rec, http.StatusCreated)
	var ph struct {
		Photo struct {
			ID          string `json:"id"`
			ItemID      string `json:"item_id"`
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
		} `json:"photo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ph); err != nil || ph.Photo.ContentType != "image/jpeg" || ph.Photo.ItemID != item {
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
	view := decode[inspectionOne](t, e.get(base, agent)).Inspection
	if view.Status != "IN_PROGRESS" || len(view.Areas[0].Items[0].Photos) != 1 {
		t.Fatalf("after upload: %s %+v", view.Status, view.Areas[0].Items[0])
	}
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))
	wantError(t, e.get(ph.Photo.URL, blake), http.StatusNotFound, inspections.CodeBookNotFound)
	wantError(t, e.get(inspectionPath(org, "/"+devseed.InspectionMoveInID.String()+"/photos/"+ph.Photo.ID), agent), http.StatusNotFound,
		inspections.CodePhotoNotFound)
	// The per-item limit.
	for i := 1; i < inspections.MaxPhotosPerItem; i++ {
		wantStatus(t, e.uploadTo(path, testJPEG(t), agent), http.StatusCreated)
	}
	wantError(t, e.uploadTo(path, testJPEG(t), agent), http.StatusConflict, inspections.CodePhotoLimit)
	wantStatus(t, e.do(http.MethodDelete, ph.Photo.URL, nil, agent), http.StatusNoContent)
	wantError(t, e.get(ph.Photo.URL, agent), http.StatusNotFound, inspections.CodePhotoNotFound)
	wantStatus(t, e.uploadTo(path, testJPEG(t), agent), http.StatusCreated)
	// The report embeds the thumbnails.
	pdf := e.get(base+"/report.pdf", agent)
	wantStatus(t, pdf, http.StatusOK)
	if !bytes.HasPrefix(pdf.Body.Bytes(), []byte("%PDF")) || !bytes.Contains(pdf.Body.Bytes(), []byte("/DCTDecode")) {
		t.Fatalf("report without the photos: %d bytes", pdf.Body.Len())
	}
	// Removing the item from the checklist removes its photos.
	var areas []obj
	for i, a := range view.Areas {
		var items []obj
		for j, it := range a.Items {
			if i == 0 && j == 0 {
				continue
			}
			items = append(items, obj{"id": it.ID, "label": it.Label})
		}
		areas = append(areas, obj{"id": a.ID, "label": a.Label, "items": items})
	}
	wantStatus(t, e.do(http.MethodPut, base+"/checklist", obj{"areas": areas}, agent), http.StatusOK)
	if left, _ := e.inspections.Photos(context.Background(), uuid.MustParse(in.ID)); len(left) != 0 {
		t.Fatalf("%d photos left on a removed item", len(left))
	}
}

// The tenant: the COMPLETED inspections of their own tenancy only,
// read-only, with the report and the photos; nothing else.
func TestInspectionsTenant(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	flat1, cottage := devseed.TenancyFlat1ID, devseed.TenancyCottageID

	list := decode[struct {
		Items []inspectionBody `json:"items"`
	}](t, e.get(myInspectionPath(flat1, ""), tenant))
	if len(list.Items) != 1 || list.Items[0].Reference != "RPI-2026-900001" || list.Items[0].Status != "COMPLETED" {
		t.Fatalf("tenant list: %+v", list.Items)
	}
	rec := e.get(myInspectionPath(flat1, "/"+devseed.InspectionMoveInID.String()), tenant)
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	for _, banned := range []string{`"user_id"`, `"can"`, `"created_by"`, `"completed_by"`, devseed.EstatesAgent2ID.String(), "cancel_reason"} {
		if strings.Contains(body, banned) {
			t.Fatalf("tenant view shows %q: %s", banned, body)
		}
	}
	if !strings.Contains(body, "Cracked tile next to the bath") || !strings.Contains(body, `"report_url":"/v1/me/tenancies/`) {
		t.Fatalf("tenant view: %s", body)
	}
	pdf := e.get(myInspectionPath(flat1, "/"+devseed.InspectionMoveInID.String()+"/report.pdf"), tenant)
	wantStatus(t, pdf, http.StatusOK)
	if !bytes.HasPrefix(pdf.Body.Bytes(), []byte("%PDF")) {
		t.Fatal("the tenant's report is not a PDF")
	}
	// Not completed: 404, also its report.
	routine := devseed.InspectionRoutineID.String()
	wantError(t, e.get(myInspectionPath(flat1, "/"+routine), tenant), http.StatusNotFound, inspections.CodeNotFound)
	wantError(t, e.get(myInspectionPath(flat1, "/"+routine+"/report.pdf"), tenant), http.StatusNotFound, inspections.CodeNotFound)
	// Another tenancy, another tenant, nobody.
	wantError(t, e.get(myInspectionPath(cottage, ""), tenant), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	wantError(t, e.get(myInspectionPath(flat1, ""), consumer), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	wantError(t, e.get(myInspectionPath(flat1, "/"+devseed.InspectionMoveInID.String()+"/report.pdf"), consumer), http.StatusNotFound,
		tenancies.CodeTenancyNotFound)
	wantError(t, e.get(myInspectionPath(flat1, "")), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	// No write route exists for the tenant.
	wantStatus(t, e.post(myInspectionPath(flat1, ""), flat1Inspection("ROUTINE"), tenant), http.StatusMethodNotAllowed)
	wantStatus(t, e.do(http.MethodPut, myInspectionPath(flat1, "/"+routine+"/findings"), obj{"items": []obj{}}, tenant), http.StatusNotFound)

	// A cottage inspection completed by the manager is not on Flat 1.
	cot := obj{"property_id": devseed.PropertyCottageID.String(), "tenancy_id": cottage.String(), "type": "ROUTINE",
		"scheduled_for": "2026-11-18T09:00:00Z", "checklist_template": "commercial"}
	other := newInspection(t, e, agent, cot)
	opath := inspectionPath(org, "/"+other.ID)
	fillAll(t, e, agent, opath, other, "OK")
	wantStatus(t, e.post(opath+"/complete", obj{}, agent), http.StatusOK)
	wantError(t, e.get(myInspectionPath(flat1, "/"+other.ID), tenant), http.StatusNotFound, inspections.CodeNotFound)

	// A move-out of Flat 1 is compared with the move-in, and once
	// completed the tenant sees it with the photos.
	out := newInspection(t, e, agent, flat1Inspection("MOVE_OUT"))
	if out.Baseline == nil || out.Baseline.Reference != "RPI-2026-900001" {
		t.Fatalf("move-out baseline: %+v", out.Baseline)
	}
	var tile *inspectionItemBody
	for i, a := range out.Areas {
		for j, it := range a.Items {
			if a.Label == "Bathroom" && it.Label == "Tiles and grout" {
				tile = &out.Areas[i].Items[j]
			}
		}
	}
	if tile == nil || tile.Baseline == nil || tile.Baseline.Status == nil || *tile.Baseline.Status != "DAMAGED" ||
		tile.Baseline.Note != "Cracked tile next to the bath" {
		t.Fatalf("baseline finding: %+v", tile)
	}
	upath := inspectionPath(org, "/"+out.ID)
	wantStatus(t, e.uploadTo(upath+"/items/"+tile.ID+"/photos", testJPEG(t), agent), http.StatusCreated)
	fillAll(t, e, agent, upath, out, "OK")
	wantStatus(t, e.post(upath+"/complete", obj{}, agent), http.StatusOK)
	mine := decode[inspectionOne](t, e.get(myInspectionPath(flat1, "/"+out.ID), tenant)).Inspection
	if mine.Baseline == nil || mine.Baseline.Reference != "RPI-2026-900001" {
		t.Fatalf("tenant baseline: %+v", mine.Baseline)
	}
	var url string
	for _, a := range mine.Areas {
		for _, it := range a.Items {
			if it.ID == tile.ID && len(it.Photos) == 1 {
				url = it.Photos[0].URL
			}
		}
	}
	if !strings.HasPrefix(url, "/v1/me/tenancies/"+flat1.String()+"/inspections/") {
		t.Fatalf("tenant photo url: %q", url)
	}
	wantStatus(t, e.get(url, tenant), http.StatusOK)
	wantError(t, e.get(url, consumer), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	if n := len(decode[struct {
		Items []inspectionBody `json:"items"`
	}](t, e.get(myInspectionPath(flat1, ""), tenant)).Items); n != 2 {
		t.Fatalf("tenant list after the move-out: %d", n)
	}
}

// Scheduling and completing an inspection with a tenancy queue an event
// for the tenants, with ids, the reference, the type and status only.
func TestInspectionNotificationsQueued(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	before := len(e.inspections.Outbox())
	in := newInspection(t, e, agent, flat1Inspection("ROUTINE"))
	path := inspectionPath(org, "/"+in.ID)
	wantStatus(t, e.do(http.MethodPatch, path, obj{"scheduled_for": "2026-12-04T08:30:00Z"}, agent), http.StatusOK)
	wantStatus(t, e.do(http.MethodPut, path+"/findings", obj{"items": []obj{{"item_id": in.Areas[0].Items[0].ID, "status": "DAMAGED",
		"note": "Kudzai says the lock was forced; call +263771234567"}}}, agent), http.StatusOK)
	fillAll(t, e, agent, path, in, "OK")
	wantStatus(t, e.post(path+"/complete", obj{"summary": "Tenant Kudzai Sithole present", "tenant_sign_off_name": "Kudzai Sithole"}, agent),
		http.StatusOK)
	// Without a tenancy: nothing to tell.
	newInspection(t, e, agent, obj{"property_id": devseed.PropertyAvondaleID.String(), "type": "ROUTINE",
		"scheduled_for": "2026-12-01T09:00:00Z", "checklist_template": "residential"})
	events := e.inspections.Outbox()[before:]
	if len(events) != 3 || events[0].Type != inspections.OutboxScheduled || events[1].Type != inspections.OutboxScheduled ||
		events[2].Type != inspections.OutboxCompleted {
		t.Fatalf("events: %+v", events)
	}
	for _, ev := range events {
		if ev.AggregateID.String() != in.ID || ev.AggregateType != inspections.AggregateInspection {
			t.Fatalf("event: %+v", ev)
		}
		payload := string(ev.Payload)
		for _, banned := range []string{"Kudzai", "Sithole", "+263", "@", "lock", "Avondale", "Flat", "Tenant", "note", "summary"} {
			if strings.Contains(payload, banned) {
				t.Fatalf("payload carries %q: %s", banned, payload)
			}
		}
	}
	var p inspections.EventPayload
	if err := json.Unmarshal(events[1].Payload, &p); err != nil || !p.Rescheduled || p.Reference != in.Reference || p.Type != "ROUTINE" {
		t.Fatalf("payload: %+v %v", p, err)
	}
	// The source the notifications read: the tenant with a verified account
	// and the market's time zone.
	info, err := InspectionNotices(e.inspections, e.tenancies, e.crm, e.identity, e.marketStore)(context.Background(),
		uuid.MustParse(in.ID))
	if err != nil {
		t.Fatal(err)
	}
	if info.TenancyID == nil || len(info.Tenants) != 1 || info.Tenants[0].Email != devseed.TenantEmail || info.Zone != "Africa/Harare" ||
		!info.ScheduledFor.Equal(time.Date(2026, 12, 4, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("notice: %+v", info)
	}
}
