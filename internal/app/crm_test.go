package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/outbox"
)

func contactsPath(org uuid.UUID, rest string) string {
	return "/v1/agent/organisations/" + org.String() + "/contacts" + rest
}

type contactBody struct {
	Contact struct {
		ID             string   `json:"id"`
		Name           string   `json:"name"`
		Emails         []string `json:"emails"`
		Phones         []string `json:"phones"`
		Type           string   `json:"type"`
		TypeLabel      string   `json:"type_label"`
		Stage          string   `json:"stage"`
		StageLabel     string   `json:"stage_label"`
		OwnerUserID    *string  `json:"owner_user_id"`
		OwnerName      *string  `json:"owner_name"`
		MergedInto     *string  `json:"merged_into"`
		RemindersDue   int      `json:"reminders_due"`
		LinkedListings []struct {
			ID string `json:"id"`
		} `json:"linked_listings"`
	} `json:"contact"`
	LinkedItems []struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
		Auto bool   `json:"auto"`
	} `json:"linked_items"`
}

type contactPage struct {
	Items []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Stage        string `json:"stage"`
		RemindersDue int    `json:"reminders_due"`
	} `json:"items"`
	Total       int            `json:"total"`
	StageCounts map[string]int `json:"stage_counts"`
}

// multipartFile builds a multipart body with one file field.
func multipartFile(t *testing.T, field, name, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(fw, content)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func (e *env) raw(method, path string, body io.Reader, contentType string, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// Every endpoint answers another organisation's member, a consumer and an
// administrator 404 CONTACT_NOT_FOUND, and never data.
func TestContactsWrongOrganisation(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	cid := devseed.ContactTariroID.String()
	rid := uuid.NewString()
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	admin := browser(e.adminLogin(identity.SeedAdminEmail, seedPassword))
	file, ct := multipartFile(t, "file", "contacts.csv", "name\nIntruder\n")
	type call struct {
		method, path string
		body         any
	}
	calls := []call{
		{http.MethodGet, contactsPath(org, ""), nil},
		{http.MethodPost, contactsPath(org, ""), obj{"name": "Intruder", "type": "buyer"}},
		{http.MethodGet, contactsPath(org, "/reminders?due=week"), nil},
		{http.MethodGet, contactsPath(org, "/tags"), nil},
		{http.MethodGet, contactsPath(org, "/assignees"), nil},
		{http.MethodGet, contactsPath(org, "/export.csv"), nil},
		{http.MethodPost, contactsPath(org, "/merge"), obj{"keep_id": devseed.ContactMarufuID, "merge_ids": []uuid.UUID{devseed.ContactMarufuDupID}}},
		{http.MethodPost, contactsPath(org, "/from-request/"+devseed.SampleAppointmentID(6).String()), nil},
		{http.MethodGet, contactsPath(org, "/"+cid), nil},
		{http.MethodPatch, contactsPath(org, "/"+cid), obj{"name": "Changed"}},
		{http.MethodDelete, contactsPath(org, "/"+cid), nil},
		{http.MethodGet, contactsPath(org, "/"+cid+"/notes"), nil},
		{http.MethodPost, contactsPath(org, "/"+cid+"/notes"), obj{"text": "Hello"}},
		{http.MethodPost, contactsPath(org, "/"+cid+"/reminders"), obj{"due_at": e.now, "text": "Hello"}},
		{http.MethodPatch, contactsPath(org, "/"+cid+"/reminders/"+rid), obj{"done": true}},
		{http.MethodPost, contactsPath(org, "/"+cid+"/stage"), obj{"stage": "CLOSED"}},
		{http.MethodPost, contactsPath(org, "/"+cid+"/owner"), obj{"user_id": nil}},
		{http.MethodPost, contactsPath(org, "/"+cid+"/listings"), obj{"listing_id": propertyrequests.SeedListingID(1)}},
		{http.MethodDelete, contactsPath(org, "/"+cid+"/listings/"+propertyrequests.SeedListingID(1).String()), nil},
		{http.MethodGet, contactsPath(org, "/"+cid+"/timeline"), nil},
	}
	for who, as := range map[string]reqOpt{"another organisation": blake, "consumer": consumer, "administrator": admin} {
		for _, c := range calls {
			rec := e.do(c.method, c.path, c.body, as)
			env := wantError(t, rec, http.StatusNotFound, crm.CodeContactNotFound)
			if strings.Contains(rec.Body.String(), "Tariro") || env.Error.Code != crm.CodeContactNotFound {
				t.Fatalf("%s %s %s: %s", who, c.method, c.path, rec.Body.String())
			}
		}
		rec := e.raw(http.MethodPost, contactsPath(org, "/import?dry_run=true"), bytes.NewReader(file.Bytes()), ct, as)
		wantError(t, rec, http.StatusNotFound, crm.CodeContactNotFound)
	}
	// Sample Estates' owner cannot reach Second Street Realty's contacts,
	// neither through Second Street's book nor through their own.
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	theirs := devseed.SecondStreetContactIDs[1].String()
	wantError(t, e.get(contactsPath(identity.SeedSecondOrganisationID, ""), owner), http.StatusNotFound, crm.CodeContactNotFound)
	wantError(t, e.get(contactsPath(identity.SeedSecondOrganisationID, "/"+theirs), owner), http.StatusNotFound, crm.CodeContactNotFound)
	wantError(t, e.get(contactsPath(org, "/"+theirs), owner), http.StatusNotFound, crm.CodeContactNotFound)
	wantError(t, e.get(contactsPath(org, "/"+theirs+"/timeline"), owner), http.StatusNotFound, crm.CodeContactNotFound)
	wantError(t, e.post(contactsPath(org, "/"+theirs+"/notes"), obj{"text": "x"}, owner), http.StatusNotFound, crm.CodeContactNotFound)
	// The two books are separate: the same email is no duplicate across them.
	page := decode[contactPage](t, e.get(contactsPath(identity.SeedSecondOrganisationID, ""), blake))
	if page.Total != 2 {
		t.Fatalf("Second Street sees %d contacts", page.Total)
	}
	if body := e.get(contactsPath(identity.SeedSecondOrganisationID, ""), blake).Body.String(); strings.Contains(body, "Nyasha") {
		t.Fatal("Sample Estates data in Second Street's book")
	}
}

func TestContactsRoutes(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	manager := browser(e.loginCookie(devseed.AgencyAdminEmail, seedPassword))
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))

	// The seeded book: 8 contacts across the stages; STAFF read it.
	rec := e.get(contactsPath(org, "?page_size=50"), staff)
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("cache-control = %q", rec.Header().Get("Cache-Control"))
	}
	page := decode[contactPage](t, rec)
	if page.Total != 8 || page.StageCounts["CONTACTED"] != 2 || page.StageCounts["UNSUCCESSFUL"] != 1 {
		t.Fatalf("seeded book: %d, %v", page.Total, page.StageCounts)
	}
	got := decode[contactPage](t, e.get(contactsPath(org, "?has_reminder_due=true"), staff))
	if got.Total != 1 || got.Items[0].ID != devseed.ContactNyashaID.String() || got.Items[0].RemindersDue != 1 {
		t.Fatalf("reminder due: %+v", got)
	}
	wantError(t, e.get(contactsPath(org, "?sort=price"), staff), http.StatusBadRequest, "VALIDATION_FAILED")
	// The auto-linked contact.
	tariro := decode[contactBody](t, e.get(contactsPath(org, "/"+devseed.ContactTariroID.String()), staff))
	if tariro.Contact.Name != "Tariro Moyo" || tariro.Contact.TypeLabel != "Tenant" || tariro.Contact.StageLabel != "Viewing requested" ||
		tariro.Contact.OwnerName == nil || *tariro.Contact.OwnerName != "Chipo Dube" {
		t.Fatalf("tariro: %+v", tariro.Contact)
	}

	// STAFF may not write.
	wantError(t, e.post(contactsPath(org, ""), obj{"name": "Staff made", "type": "buyer"}, staff), http.StatusForbidden, "FORBIDDEN")
	// A duplicate: 409 with error.details.duplicates.
	rec = e.post(contactsPath(org, ""), obj{"name": "Tariro again", "type": "tenant", "phones": []string{"0771234567"}}, agent)
	wantStatus(t, rec, http.StatusConflict)
	dup := decode[struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Duplicates []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"duplicates"`
			} `json:"details"`
		} `json:"error"`
	}](t, rec)
	if dup.Error.Code != crm.CodeContactDuplicate || len(dup.Error.Details.Duplicates) != 1 || dup.Error.Details.Duplicates[0].ID != devseed.ContactTariroID.String() {
		t.Fatalf("duplicate: %s", rec.Body.String())
	}
	rec = e.post(contactsPath(org, "?allow_duplicate=true"), obj{"name": "Tariro again", "type": "tenant", "phones": []string{"0771234567"}}, agent)
	wantStatus(t, rec, http.StatusCreated)
	created := decode[contactBody](t, rec)
	if created.Contact.Phones[0] != "+263771234567" || created.Contact.OwnerUserID == nil {
		t.Fatalf("created: %+v", created.Contact)
	}
	cid := created.Contact.ID

	// Notes, reminders, stage, listings, timeline.
	wantStatus(t, e.post(contactsPath(org, "/"+cid+"/notes"), obj{"text": "Called"}, agent), http.StatusCreated)
	rec = e.post(contactsPath(org, "/"+cid+"/reminders"), obj{"due_at": e.now.Add(time.Hour), "text": "Follow up"}, agent)
	wantStatus(t, rec, http.StatusCreated)
	rem := decode[struct {
		ID string `json:"id"`
	}](t, rec)
	wantStatus(t, e.do(http.MethodPatch, contactsPath(org, "/"+cid+"/reminders/"+rem.ID), obj{"done": true}, agent), http.StatusOK)
	wantError(t, e.do(http.MethodPatch, contactsPath(org, "/"+cid+"/reminders/"+uuid.NewString()), obj{"done": true}, agent), http.StatusNotFound, crm.CodeReminderNotFound)
	wantStatus(t, e.post(contactsPath(org, "/"+cid+"/stage"), obj{"stage": "NEGOTIATING", "note": "Offer"}, agent), http.StatusOK)
	rec = e.post(contactsPath(org, "/"+cid+"/listings"), obj{"listing_id": propertyrequests.SeedListingID(1)}, agent)
	wantStatus(t, rec, http.StatusOK)
	if l := decode[contactBody](t, rec).Contact.LinkedListings; len(l) != 1 || l[0].ID != propertyrequests.SeedListingID(1).String() {
		t.Fatalf("linked listings: %+v", l)
	}
	// Listing 3 belongs to Sample Commercial.
	wantError(t, e.post(contactsPath(org, "/"+cid+"/listings"), obj{"listing_id": propertyrequests.SeedListingID(3)}, agent), http.StatusNotFound, crm.CodeListingNotFound)
	rec = e.get(contactsPath(org, "/"+cid+"/timeline"), agent)
	wantStatus(t, rec, http.StatusOK)
	tl := decode[struct {
		Items []struct {
			Kind string `json:"kind"`
		} `json:"items"`
	}](t, rec)
	if len(tl.Items) < 5 || tl.Items[0].Kind != "LISTING_LINKED" {
		t.Fatalf("timeline: %s", rec.Body.String())
	}

	// Reassignment: the agent owner may; it is in the organisation's audit trail.
	chipo := devseed.EstatesAgent2ID.String()
	rec = e.post(contactsPath(org, "/"+cid+"/owner"), obj{"user_id": chipo}, agent)
	wantStatus(t, rec, http.StatusOK)
	if *decode[contactBody](t, rec).Contact.OwnerUserID != chipo {
		t.Fatal("not reassigned")
	}
	wantError(t, e.post(contactsPath(org, "/"+cid+"/owner"), obj{"user_id": devseed.AgencyStaffID}, owner), http.StatusBadRequest, "VALIDATION_FAILED")
	assignees := e.get(contactsPath(org, "/assignees"), agent)
	wantStatus(t, assignees, http.StatusOK)
	if body := assignees.Body.String(); !strings.Contains(body, "Chipo Dube") || strings.Contains(body, "Kuda Staff") {
		t.Fatalf("assignees: %s", body)
	}
	wantError(t, e.get(contactsPath(org, "/assignees"), staff), http.StatusForbidden, "FORBIDDEN")
	if tags := e.get(contactsPath(org, "/tags"), staff).Body.String(); !strings.Contains(tags, `"tag":"rental"`) {
		t.Fatalf("tags: %s", tags)
	}

	// Merge the seeded duplicate pair.
	rec = e.post(contactsPath(org, "/merge"), obj{"keep_id": devseed.ContactMarufuID, "merge_ids": []uuid.UUID{devseed.ContactMarufuDupID}}, agent)
	wantStatus(t, rec, http.StatusOK)
	merged := decode[contactBody](t, rec)
	if len(merged.Contact.Phones) != 2 {
		t.Fatalf("merged: %+v", merged.Contact)
	}
	tomb := decode[contactBody](t, e.get(contactsPath(org, "/"+devseed.ContactMarufuDupID.String()), agent))
	if tomb.Contact.MergedInto == nil || *tomb.Contact.MergedInto != devseed.ContactMarufuID.String() {
		t.Fatalf("tombstone: %+v", tomb.Contact)
	}
	wantError(t, e.post(contactsPath(org, "/"+devseed.ContactMarufuDupID.String()+"/notes"), obj{"text": "x"}, agent), http.StatusConflict, crm.CodeContactMerged)

	// Import: OWNER and MANAGER only, a dry run first.
	csvText := "name,email,phone,type\nNew Person,new.person@example.com,0779000111,buyer\nDupe,tariro@example.com,,tenant\nBroken,bad,,\n"
	file, ct := multipartFile(t, "file", "contacts.csv", csvText)
	wantError(t, e.raw(http.MethodPost, contactsPath(org, "/import?dry_run=true"), bytes.NewReader(file.Bytes()), ct, agent), http.StatusForbidden, "FORBIDDEN")
	rec = e.raw(http.MethodPost, contactsPath(org, "/import?dry_run=true"), bytes.NewReader(file.Bytes()), ct, manager)
	wantStatus(t, rec, http.StatusOK)
	type importBody struct {
		Rows []struct {
			Row    int    `json:"row"`
			Status string `json:"status"`
		} `json:"rows"`
		Summary struct {
			Create, Duplicate, Error int
			Created                  *int
		} `json:"summary"`
	}
	dry := decode[importBody](t, rec)
	if dry.Summary.Create != 1 || dry.Summary.Duplicate != 1 || dry.Summary.Error != 1 || dry.Summary.Created != nil {
		t.Fatalf("dry run: %s", rec.Body.String())
	}
	rec = e.raw(http.MethodPost, contactsPath(org, "/import"), bytes.NewReader(file.Bytes()), ct, manager)
	wantStatus(t, rec, http.StatusCreated)
	if real := decode[importBody](t, rec); real.Summary.Created == nil || *real.Summary.Created != 1 || real.Rows[1].Status != crm.OutcomeSkipped {
		t.Fatalf("import: %s", rec.Body.String())
	}
	wantStatus(t, e.raw(http.MethodPost, contactsPath(org, "/import"), strings.NewReader("{}"), "application/json", manager), http.StatusUnsupportedMediaType)

	// Export: OWNER and MANAGER only, a CSV attachment, audited.
	wantError(t, e.get(contactsPath(org, "/export.csv"), agent), http.StatusForbidden, "FORBIDDEN")
	rec = e.get(contactsPath(org, "/export.csv"), manager)
	wantStatus(t, rec, http.StatusOK)
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="contacts-`) ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("headers: %v", rec.Header())
	}
	records, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil || len(records) != 1+9 || records[0][0] != "id" { // 8 seeded + 1 created + 1 imported - 1 merged
		t.Fatalf("export: %d rows %v", len(records), err)
	}

	// The organisation's audit trail has the reassignment, merge, import and export.
	audit := decode[auditBody](t, e.get("/v1/agent/organisations/"+org.String()+"/audit", owner))
	seen := map[string]bool{}
	for _, it := range audit.Items {
		seen[it.Action] = true
	}
	for _, a := range []string{orgaudit.ActionContactOwnerChanged, orgaudit.ActionContactsMerged, orgaudit.ActionContactsImported, orgaudit.ActionContactsExported} {
		if !seen[a] {
			t.Fatalf("audit trail lacks %s: %+v", a, audit.Items)
		}
	}

	// Delete: soft, then 404.
	rec = e.do(http.MethodDelete, contactsPath(org, "/"+cid), nil, agent)
	wantStatus(t, rec, http.StatusNoContent)
	wantError(t, e.get(contactsPath(org, "/"+cid), agent), http.StatusNotFound, crm.CodeContactNotFound)

	// Reminders for the dashboard.
	rec = e.get(contactsPath(org, "/reminders?due=today"), agent)
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "Borrowdale stands") {
		t.Fatalf("reminders: %s", rec.Body.String())
	}
	wantError(t, e.get(contactsPath(org, "/reminders?due=year"), agent), http.StatusBadRequest, "VALIDATION_FAILED")
}

// A property request that arrives for Sample Estates is linked (by the
// outbox handler the worker runs) to the contact that shares its email; a
// one-click contact can be made from it.
func TestContactsAutoLinkAndFromRequest(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	var mu sync.Mutex
	var events []outbox.Event
	capture := func(ev outbox.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	e.requests.SetOutboxSink(capture)
	e.enquiries.SetOutboxSink(capture)
	svc := CRMService(e.crm, e.crmItems, e.listings, e.identity, e.reference, e.team, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return e.now })

	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(1)), http.StatusCreated) // Tendai's listing
	wantStatus(t, e.post("/v1/business-enquiries", e.enquiry("agencies", "sample-estates")), http.StatusCreated)
	mu.Lock()
	pending := append([]outbox.Event(nil), events...)
	mu.Unlock()
	handled := 0
	for _, ev := range pending {
		if !strings.HasSuffix(ev.Type, "_CREATED") {
			continue
		}
		handled++
		for range 2 { // at-least-once: a redelivery changes nothing
			if err := svc.HandleOutbox(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	if handled != 2 {
		t.Fatalf("%d creation events", handled)
	}
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	tariro := decode[contactBody](t, e.get(contactsPath(org, "/"+devseed.ContactTariroID.String()), owner))
	kinds := map[string]int{}
	for _, it := range tariro.LinkedItems {
		kinds[it.Kind]++
	}
	if kinds[crm.KindPropertyRequest] != 1 || kinds[crm.KindBusinessEnquiry] != 1 {
		t.Fatalf("linked: %+v", tariro.LinkedItems)
	}
	// Second Street's contact with the same email was not linked.
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))
	theirs := decode[contactBody](t, e.get(contactsPath(identity.SeedSecondOrganisationID, "/"+devseed.SecondStreetContactIDs[0].String()), blake))
	if len(theirs.LinkedItems) != 0 {
		t.Fatalf("linked across organisations: %+v", theirs.LinkedItems)
	}

	// From the request: it is linked to the existing contact (200).
	var requestID string
	for _, it := range tariro.LinkedItems {
		if it.Kind == crm.KindPropertyRequest {
			requestID = it.ID
		}
	}
	rec := e.post(contactsPath(org, "/from-request/"+requestID), nil, owner)
	wantStatus(t, rec, http.StatusOK)
	if decode[contactBody](t, rec).Contact.ID != devseed.ContactTariroID.String() {
		t.Fatal("from-request did not find the existing contact")
	}
	wantError(t, e.post(contactsPath(org, "/from-request/"+uuid.NewString()), nil, owner), http.StatusNotFound, crm.CodeRequestNotFound)
	// Another organisation cannot use Sample Estates' request.
	wantError(t, e.post(contactsPath(identity.SeedSecondOrganisationID, "/from-request/"+requestID), nil, blake), http.StatusNotFound, crm.CodeRequestNotFound)

	// A request from somebody new: one click makes the contact (201), linked.
	body := e.propertyRequest(2)
	body["guest_contact"] = obj{"name": "New Customer", "email": "new.customer@example.com", "phone": "+263779555666"}
	rec = e.post("/v1/property-requests", body)
	wantStatus(t, rec, http.StatusCreated)
	newID := decode[struct {
		RequestID string `json:"request_id"`
	}](t, rec).RequestID
	rec = e.post(contactsPath(org, "/from-request/"+newID+"?type=buyer"), nil, owner)
	wantStatus(t, rec, http.StatusCreated)
	made := decode[contactBody](t, rec)
	if made.Contact.Name != "New Customer" || made.Contact.Type != "buyer" || made.Contact.Phones[0] != "+263779555666" ||
		len(made.LinkedItems) != 1 || made.LinkedItems[0].ID != newID {
		t.Fatalf("made: %s", rec.Body.String())
	}
}
