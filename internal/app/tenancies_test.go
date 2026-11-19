package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

func propertiesPath(book uuid.UUID, rest string) string {
	return "/v1/agent/organisations/" + book.String() + "/properties" + rest
}

func tenanciesPath(book uuid.UUID, rest string) string {
	return "/v1/agent/organisations/" + book.String() + "/tenancies" + rest
}

type tenancyBody struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
	Property    struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"property"`
	Unit *struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"unit"`
	Tenants []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Live bool   `json:"live"`
	} `json:"tenants"`
	Landlords []struct {
		Name string `json:"name"`
	} `json:"landlords"`
	Lease *struct {
		Reference string `json:"reference"`
	} `json:"lease"`
	Rent struct {
		Amount    string `json:"amount"`
		Currency  string `json:"currency"`
		MinorUnit int    `json:"minor_unit"`
		Frequency string `json:"frequency"`
		DueDay    int    `json:"due_day"`
	} `json:"rent"`
	Deposit *struct {
		Amount string `json:"amount"`
	} `json:"deposit"`
	EndDate   *string `json:"end_date"`
	EndedOn   *string `json:"ended_on"`
	CreatedBy struct {
		UserID *string `json:"user_id"`
		Name   string  `json:"name"`
	} `json:"created_by"`
	ManagedBy struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"managed_by"`
	Can struct {
		Edit     bool `json:"edit"`
		Activate bool `json:"activate"`
		End      bool `json:"end"`
	} `json:"can"`
}

type tenancyPage struct {
	Items        []tenancyBody  `json:"items"`
	Total        int            `json:"total"`
	StatusCounts map[string]int `json:"status_counts"`
}

type propertyBody struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Units []struct {
		ID              string  `json:"id"`
		Label           string  `json:"label"`
		ActiveTenancyID *string `json:"active_tenancy_id"`
	} `json:"units"`
	UnitCount       int     `json:"unit_count"`
	LettableUnits   int     `json:"lettable_units"`
	OccupiedUnits   int     `json:"occupied_units"`
	ActiveTenancyID *string `json:"active_tenancy_id"`
	OpenTenancies   int     `json:"open_tenancies"`
	Listing         *struct {
		Reference string `json:"reference"`
	} `json:"listing"`
	Can struct {
		Edit   bool `json:"edit"`
		Delete bool `json:"delete"`
	} `json:"can"`
}

// The seed, over HTTP; the tenant reads their own tenancy and nothing else.
func TestTenanciesSeedAndTenantView(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	rec := e.get(propertiesPath(org, ""), staff)
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Cache-Control") != "private, no-store" || rec.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("headers: %v", rec.Header())
	}
	props := decode[struct {
		Items []propertyBody `json:"items"`
		Total int            `json:"total"`
	}](t, rec)
	if props.Total != 2 {
		t.Fatalf("properties: %+v", props)
	}
	for _, p := range props.Items {
		switch p.ID {
		case devseed.PropertyAvondaleID.String():
			if p.UnitCount != 3 || p.LettableUnits != 3 || p.OccupiedUnits != 1 || p.OpenTenancies != 1 || p.Can.Edit || p.Can.Delete ||
				p.Units[0].ActiveTenancyID == nil || *p.Units[0].ActiveTenancyID != devseed.TenancyFlat1ID.String() || p.Listing == nil {
				t.Fatalf("avondale: %+v", p)
			}
		case devseed.PropertyCottageID.String():
			if p.UnitCount != 0 || p.LettableUnits != 1 || p.OccupiedUnits != 1 || p.ActiveTenancyID == nil {
				t.Fatalf("cottage: %+v", p)
			}
		default:
			t.Fatalf("unexpected property %s", p.Name)
		}
	}
	page := decode[tenancyPage](t, e.get(tenanciesPath(org, ""), staff))
	if page.Total != 3 || page.StatusCounts["ACTIVE"] != 2 || page.StatusCounts["ENDED"] != 1 || page.StatusCounts["DRAFT"] != 0 {
		t.Fatalf("tenancies: %+v", page)
	}
	flat1 := decode[struct {
		Tenancy tenancyBody `json:"tenancy"`
	}](t, e.get(tenanciesPath(org, "/"+devseed.TenancyFlat1ID.String()), staff)).Tenancy
	if flat1.Rent.Amount != "950.00" || flat1.Rent.MinorUnit != 2 || flat1.Unit == nil || flat1.Unit.Label != "Flat 1" || flat1.Lease == nil ||
		flat1.Lease.Reference != devseed.SampleDraftLeaseReference || len(flat1.Tenants) != 1 || flat1.Tenants[0].Name != "Kudzai Sithole" ||
		len(flat1.Landlords) != 1 || flat1.Deposit == nil || flat1.Can.End {
		t.Fatalf("flat 1: %+v", flat1)
	}
	if opts := e.get(tenanciesPath(org, "/options"), staff); !strings.Contains(opts.Body.String(), `"write":false`) ||
		!strings.Contains(opts.Body.String(), `"TERMINATED"`) {
		t.Fatalf("options: %s", opts.Body)
	}

	// The sample tenant's verified email is one of Kudzai's: Flat 1 only.
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	rec = e.get("/v1/me/tenancies", tenant)
	wantStatus(t, rec, http.StatusOK)
	mine := decode[struct {
		Items []tenancyBody `json:"items"`
	}](t, rec)
	if len(mine.Items) != 1 || mine.Items[0].ID != devseed.TenancyFlat1ID.String() || mine.Items[0].ManagedBy.Name != "Sample Estates" ||
		mine.Items[0].ManagedBy.Kind != tenancies.ManagerOrganisation || mine.Items[0].Unit == nil || mine.Items[0].Unit.Label != "Flat 1" {
		t.Fatalf("mine: %s", rec.Body)
	}
	for _, leak := range []string{"trust account", devseed.ContactKudzaiID.String(), "Chipo Dube", `"notes"`, `"can"`, "+263"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("the tenant view leaks %q: %s", leak, rec.Body)
		}
	}
	wantStatus(t, e.get("/v1/me/tenancies/"+devseed.TenancyFlat1ID.String(), tenant), http.StatusOK)
	// Another tenant's tenancy (Tariro's) is 404, as a malformed ID.
	wantError(t, e.get("/v1/me/tenancies/"+devseed.TenancyCottageID.String(), tenant), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	wantError(t, e.get("/v1/me/tenancies/nope", tenant), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	// Somebody else: nothing, and 404.
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	if body := e.get("/v1/me/tenancies", consumer).Body.String(); !strings.Contains(body, `"items":[]`) {
		t.Fatalf("consumer: %s", body)
	}
	wantError(t, e.get("/v1/me/tenancies/"+devseed.TenancyFlat1ID.String(), consumer), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	wantError(t, e.get("/v1/me/tenancies"), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// An account whose email is not verified gets nothing, even when a tenant
	// contact has its email (unverified accounts cannot sign in, so the
	// service is asked directly).
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	rec = e.post(contactsPath(org, "/"), obj{"name": "Pending Tenant", "type": "tenant", "emails": []string{"pending.tenant@example.test"}}, owner)
	wantStatus(t, rec, http.StatusCreated)
	pending := decode[contactBody](t, rec).Contact.ID
	wantStatus(t, e.do(http.MethodPatch, tenanciesPath(org, "/"+devseed.TenancyFlat1ID.String()),
		obj{"tenants": []string{devseed.ContactKudzaiID.String(), pending}}, owner), http.StatusOK)
	unverified := uuid.New()
	if err := e.identity.CreateUser(context.Background(), identity.NewUser{ID: unverified, Name: "Pending Tenant", Email: "pending.tenant@example.test",
		PasswordHash: "x", CreatedAt: e.now}); err != nil {
		t.Fatal(err)
	}
	svc := TenanciesService(e.tenancies, e.crm, e.listings, e.leases, e.marketStore, e.identity, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return e.now })
	if list, err := svc.MyTenancies(context.Background(), &auth.Principal{UserID: unverified}); err != nil || len(list) != 0 {
		t.Fatalf("unverified: %+v %v", list, err)
	}
	if _, err := svc.MyTenancy(context.Background(), &auth.Principal{UserID: unverified}, devseed.TenancyFlat1ID); err == nil {
		t.Fatal("unverified account read a tenancy")
	}
	if list, err := svc.MyTenancies(context.Background(), &auth.Principal{UserID: devseed.TenantID}); err != nil || len(list) != 1 {
		t.Fatalf("verified control: %+v %v", list, err)
	}
}

// Every endpoint answers another organisation's member, a consumer, the
// tenant and an administrator 404 BOOK_NOT_FOUND, and never data.
func TestTenanciesWrongOrganisation(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	pid, uid, tid := devseed.PropertyAvondaleID.String(), devseed.UnitFlat3ID.String(), devseed.TenancyFlat1ID.String()
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	admin := browser(e.adminLogin(identity.SeedAdminEmail, seedPassword))
	tenancy := obj{"property_id": pid, "unit_id": uid, "tenants": []string{devseed.ContactKudzaiID.String()},
		"rent": obj{"amount": "1", "currency": "USD", "frequency": "MONTHLY", "due_day": 1}, "start_date": "2026-12-01"}
	type call struct {
		method, path string
		body         any
	}
	calls := []call{
		{http.MethodGet, propertiesPath(org, ""), nil},
		{http.MethodGet, propertiesPath(org, "?page=bad"), nil},
		{http.MethodPost, propertiesPath(org, ""), obj{"name": "Intruder", "address": "x", "market_code": "ZW"}},
		{http.MethodPost, propertiesPath(org, ""), obj{"name": 1}},
		{http.MethodGet, propertiesPath(org, "/"+pid), nil},
		{http.MethodGet, propertiesPath(org, "/not-a-uuid"), nil},
		{http.MethodPatch, propertiesPath(org, "/"+pid), obj{"name": "Intruder"}},
		{http.MethodDelete, propertiesPath(org, "/"+pid), nil},
		{http.MethodPost, propertiesPath(org, "/"+pid+"/units"), obj{"label": "Flat 9"}},
		{http.MethodPatch, propertiesPath(org, "/"+pid+"/units/"+uid), obj{"label": "Flat 9"}},
		{http.MethodDelete, propertiesPath(org, "/"+pid+"/units/"+uid), nil},
		{http.MethodGet, tenanciesPath(org, "/options"), nil},
		{http.MethodGet, tenanciesPath(org, ""), nil},
		{http.MethodGet, tenanciesPath(org, "?status=bad"), nil},
		{http.MethodPost, tenanciesPath(org, ""), tenancy},
		{http.MethodGet, tenanciesPath(org, "/"+tid), nil},
		{http.MethodPatch, tenanciesPath(org, "/"+tid), obj{"notes": "intruder"}},
		{http.MethodDelete, tenanciesPath(org, "/"+tid), nil},
		{http.MethodPost, tenanciesPath(org, "/"+tid+"/activate"), nil},
		{http.MethodPost, tenanciesPath(org, "/"+tid+"/end"), obj{"end_date": "2026-12-31", "reason": "x"}},
	}
	for who, as := range map[string]reqOpt{"another organisation": blake, "consumer": consumer, "tenant": tenant, "administrator": admin} {
		for _, c := range calls {
			rec := e.do(c.method, c.path, c.body, as)
			wantError(t, rec, http.StatusNotFound, tenancies.CodeBookNotFound)
			if strings.Contains(rec.Body.String(), "Avondale") || strings.Contains(rec.Body.String(), "Kudzai") {
				t.Fatalf("%s %s %s: %s", who, c.method, c.path, rec.Body.String())
			}
		}
	}
	// A malformed book ID is the same 404; signed out is 401.
	wantError(t, e.get("/v1/agent/organisations/nope/tenancies", blake), http.StatusNotFound, tenancies.CodeBookNotFound)
	wantError(t, e.get(propertiesPath(org, "")), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	// Blake's own book does not reach Sample Estates' records.
	second := devseed.SecondStreetRealty.ID
	wantError(t, e.get(propertiesPath(second, "/"+pid), blake), http.StatusNotFound, tenancies.CodePropertyNotFound)
	wantError(t, e.get(tenanciesPath(second, "/"+tid), blake), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	wantError(t, e.post(tenanciesPath(second, "/"+tid+"/activate"), nil, blake), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	if body := e.get(propertiesPath(second, ""), blake).Body.String(); !strings.Contains(body, "Second Street House") || strings.Contains(body, "Avondale") {
		t.Fatalf("second street: %s", body)
	}
	if body := e.get(tenanciesPath(second, ""), blake).Body.String(); !strings.Contains(body, `"total":0`) {
		t.Fatalf("second street tenancies: %s", body)
	}
	// Nor can it use Sample Estates' contacts, property or lease in its own book.
	env := wantError(t, e.post(tenanciesPath(second, ""), obj{"property_id": devseed.PropertySecondStreetID.String(),
		"tenants": []string{devseed.ContactKudzaiID.String()}, "lease_id": devseed.SampleDraftLeaseID.String(),
		"rent": obj{"amount": "1", "currency": "USD", "frequency": "MONTHLY", "due_day": 1}, "start_date": "2026-12-01"}, blake),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["tenants[0]"] == "" || env.Error.Fields["lease_id"] == "" {
		t.Fatalf("cross-book links: %+v", env.Error.Fields)
	}
	env = wantError(t, e.post(tenanciesPath(second, ""), tenancy, blake), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["property_id"] == "" {
		t.Fatalf("cross-book property: %+v", env.Error.Fields)
	}
}

// Writing: STAFF 403; an agent creates, activates and ends; the guards and
// the audit trail.
func TestTenanciesRoutes(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, propertiesPath(org, ""), obj{"name": "x", "address": "y", "market_code": "ZW"}},
		{http.MethodPatch, propertiesPath(org, "/"+devseed.PropertyCottageID.String()), obj{"name": "x"}},
		{http.MethodPost, propertiesPath(org, "/"+devseed.PropertyAvondaleID.String()+"/units"), obj{"label": "Flat 4"}},
		{http.MethodPatch, tenanciesPath(org, "/"+devseed.TenancyFlat1ID.String()), obj{"notes": "x"}},
		{http.MethodPost, tenanciesPath(org, "/"+devseed.TenancyFlat1ID.String()+"/end"), obj{"end_date": "2026-12-31", "reason": "x"}},
	} {
		wantError(t, e.do(c.method, c.path, c.body, staff), http.StatusForbidden, httpx.CodeForbidden)
	}
	// A property with units; a JSON number as an amount is INVALID_JSON.
	rec := e.post(propertiesPath(org, ""), obj{"name": "Mount Pleasant Flats", "address": "3 Arcturus Road, Harare", "market_code": "ZW",
		"units": []obj{{"label": "A"}, {"label": "B"}}}, agent)
	wantStatus(t, rec, http.StatusCreated)
	prop := decode[struct {
		Property propertyBody `json:"property"`
	}](t, rec).Property
	if prop.UnitCount != 2 || !prop.Can.Delete {
		t.Fatalf("property: %+v", prop)
	}
	base := obj{"property_id": prop.ID, "unit_id": prop.Units[0].ID, "tenants": []string{devseed.ContactTariroID.String()},
		"landlords": []string{devseed.ContactTapiwaID.String()},
		"rent":      obj{"amount": 1200, "currency": "USD", "frequency": "MONTHLY", "due_day": 1}, "start_date": "2026-12-01"}
	wantError(t, e.post(tenanciesPath(org, ""), base, agent), http.StatusBadRequest, httpx.CodeInvalidJSON)
	base["rent"] = obj{"amount": "1200", "currency": "XYZ", "frequency": "MONTHLY", "due_day": 1}
	if env := wantError(t, e.post(tenanciesPath(org, ""), base, agent), http.StatusBadRequest, httpx.CodeValidationFailed); env.Error.Fields["rent.currency"] == "" {
		t.Fatalf("currency: %+v", env.Error.Fields)
	}
	base["rent"] = obj{"amount": "1200", "currency": "USD", "frequency": "MONTHLY", "due_day": 1}
	base["end_date"] = "2027-11-30"
	rec = e.post(tenanciesPath(org, ""), base, agent)
	wantStatus(t, rec, http.StatusCreated)
	ten := decode[struct {
		Tenancy tenancyBody `json:"tenancy"`
	}](t, rec).Tenancy
	if ten.Status != "DRAFT" || !ten.Can.Activate || ten.CreatedBy.Name != "Tendai Ncube" {
		t.Fatalf("created: %+v", ten)
	}
	// The property is in use: neither it nor the unit can be archived.
	wantError(t, e.del(propertiesPath(org, "/"+prop.ID), nil, agent), http.StatusConflict, tenancies.CodePropertyInUse)
	wantError(t, e.del(propertiesPath(org, "/"+prop.ID+"/units/"+prop.Units[0].ID), nil, agent), http.StatusConflict, tenancies.CodeUnitInUse)
	wantError(t, e.del(propertiesPath(org, "/"+prop.ID+"/units/"+uuid.NewString()), nil, agent), http.StatusNotFound, tenancies.CodeUnitNotFound)
	// Activate; then end early: TERMINATED.
	rec = e.post(tenanciesPath(org, "/"+ten.ID+"/activate"), nil, agent)
	wantStatus(t, rec, http.StatusOK)
	wantError(t, e.post(tenanciesPath(org, "/"+ten.ID+"/activate"), nil, agent), http.StatusConflict, tenancies.CodeInvalidState)
	wantError(t, e.del(tenanciesPath(org, "/"+ten.ID), nil, agent), http.StatusConflict, tenancies.CodeInvalidState)
	rec = e.post(tenanciesPath(org, "/"+ten.ID+"/end"), obj{"end_date": "2027-03-31", "reason": "Transferred for work"}, agent)
	wantStatus(t, rec, http.StatusOK)
	ended := decode[struct {
		Tenancy tenancyBody `json:"tenancy"`
	}](t, rec).Tenancy
	if ended.Status != "TERMINATED" || ended.EndedOn == nil || *ended.EndedOn != "2027-03-31" || ended.EndDate == nil || *ended.EndDate != "2027-11-30" {
		t.Fatalf("ended: %+v", ended)
	}
	// Flat 1 is occupied by the sample tenancy: a second one cannot be activated.
	rec = e.post(tenanciesPath(org, ""), obj{"property_id": devseed.PropertyAvondaleID.String(), "unit_id": devseed.UnitFlat1ID.String(),
		"tenants": []string{devseed.ContactTariroID.String()}, "rent": obj{"amount": "900", "currency": "USD", "frequency": "MONTHLY", "due_day": 1},
		"start_date": "2027-01-01"}, agent)
	wantStatus(t, rec, http.StatusCreated)
	second := decode[struct {
		Tenancy tenancyBody `json:"tenancy"`
	}](t, rec).Tenancy
	wantError(t, e.post(tenanciesPath(org, "/"+second.ID+"/activate"), nil, agent), http.StatusConflict, tenancies.CodeUnitOccupied)
	if body := e.post(tenanciesPath(org, "/"+second.ID+"/activate"), nil, agent).Body.String(); !strings.Contains(body, devseed.TenancyFlat1ID.String()) {
		t.Fatalf("occupied details: %s", body)
	}
	wantStatus(t, e.del(tenanciesPath(org, "/"+second.ID), nil, agent), http.StatusNoContent)
	wantError(t, e.get(tenanciesPath(org, "/"+second.ID), agent), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	wantError(t, e.del(propertiesPath(org, "/"+devseed.PropertyAvondaleID.String()), nil, agent), http.StatusConflict, tenancies.CodePropertyInUse)
	// Archive the now free property.
	wantStatus(t, e.del(propertiesPath(org, "/"+prop.ID), nil, agent), http.StatusNoContent)
	wantError(t, e.get(propertiesPath(org, "/"+prop.ID), agent), http.StatusNotFound, tenancies.CodePropertyNotFound)
	// The archived property still shows on its tenancy.
	if body := e.get(tenanciesPath(org, "/"+ten.ID), agent).Body.String(); !strings.Contains(body, `"archived":true`) {
		t.Fatalf("archived on the tenancy: %s", body)
	}

	// Audit: created, activated, ended, deleted, in the organisation's trail.
	audit := decode[auditBody](t, e.get("/v1/agent/organisations/"+org.String()+"/audit?page_size=100", owner))
	seen := map[string]int{}
	for _, it := range audit.Items {
		if it.Target.Type == orgaudit.TargetTenancy {
			seen[it.Action]++
		}
	}
	if seen[orgaudit.ActionTenancyCreated] != 2 || seen[orgaudit.ActionTenancyActivated] != 1 || seen[orgaudit.ActionTenancyEnded] != 1 ||
		seen[orgaudit.ActionTenancyDeleted] != 1 {
		t.Fatalf("audit: %v", seen)
	}
}

// Account deletion: the users row is anonymised, so the tenancy rows are
// unlinked explicitly.
func TestTenancyAccountDeletion(t *testing.T) {
	e := newEnv(t)
	chipo := browser(e.loginCookie(devseed.EstatesAgent2Email, seedPassword))
	wantStatus(t, e.del("/v1/me", obj{"password": seedPassword}, chipo), http.StatusOK)
	got, err := e.tenancies.Tenancy(context.Background(), devseed.TenancyFlat1ID)
	if err != nil || got.CreatedBy != nil || got.ActivatedBy != nil || got.Status != tenancies.StatusActive {
		t.Fatalf("after deletion: %+v %v", got, err)
	}
	if p, _ := e.tenancies.Property(context.Background(), devseed.PropertyCottageID); p.CreatedBy != nil {
		t.Fatal("property creator kept")
	}
}
