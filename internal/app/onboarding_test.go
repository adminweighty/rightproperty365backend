package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func testPDF() []byte { return []byte("%PDF-1.4\n1 0 obj\n<< >>\nendobj\n%%EOF\n") }

func testPNG() []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return buf.Bytes()
}

// upload posts a multipart document the way a browser form does. The part's
// declared content type is deliberately wrong: the API must ignore it.
func (e *env) upload(c *http.Cookie, role, typeCode, fileName string, content []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if typeCode != "" {
		_ = w.WriteField("type", typeCode)
	}
	if fileName != "" {
		part, err := w.CreateFormFile("file", fileName)
		if err != nil {
			e.t.Fatal(err)
		}
		_, _ = part.Write(content)
	}
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/me/onboarding/"+role+"/documents", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	browser(c)(req)
	e.seq++
	req.RemoteAddr = fmt.Sprintf("198.51.100.%d:40000", e.seq%250) // spread over the upload rate limit
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *env) put(path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	return e.do(http.MethodPut, path, body, opts...)
}

type docBody struct {
	ID   string `json:"id"`
	Type struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	} `json:"type"`
	FileName    string `json:"file_name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

type applicationBody struct {
	ID        string         `json:"id"`
	Role      string         `json:"role"`
	RoleLabel string         `json:"role_label"`
	Status    string         `json:"status"`
	Details   map[string]any `json:"details"`
	Documents []docBody      `json:"documents"`
	Missing   struct {
		Fields    []string `json:"fields"`
		Documents []string `json:"documents"`
	} `json:"missing"`
	CanSubmit  bool `json:"can_submit"`
	CanEdit    bool `json:"can_edit"`
	CanReapply bool `json:"can_reapply"`
	Decision   struct {
		Reason    *string `json:"reason"`
		DecidedAt *string `json:"decided_at"`
	} `json:"decision"`
	Profile *struct {
		Kind string `json:"kind"`
		Slug string `json:"slug"`
	} `json:"profile"`
	SubmittedAt *string `json:"submitted_at"`
}

type oneApplication struct {
	Application applicationBody `json:"application"`
}

type myApplications struct {
	Applications []applicationBody `json:"applications"`
}

// myApp returns the caller's current application for a role.
func (e *env) myApp(c *http.Cookie, role string) applicationBody {
	e.t.Helper()
	for _, a := range e.myOnboarding(c) {
		if a.Role == role {
			return a
		}
	}
	e.t.Fatalf("no application for %s", role)
	return applicationBody{}
}

func (e *env) myOnboarding(c *http.Cookie) []applicationBody {
	e.t.Helper()
	rec := e.get("/v1/me/onboarding", withCookie(c))
	wantStatus(e.t, rec, http.StatusOK)
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		e.t.Fatalf("GET /v1/me/onboarding Cache-Control = %q", cc)
	}
	return decode[myApplications](e.t, rec).Applications
}

// detailsFor returns valid details for a role.
func detailsFor(role, name string) obj {
	switch role {
	case "PRIVATE_SELLER":
		return obj{"contact_phone": "+263770000002"}
	case "AGENT":
		return obj{"agency_slug": "sample-estates", "job_title": "Sales agent", "bio": "Ten years in Borrowdale.", "areas": []string{"Borrowdale"}, "contact_phone": "+263770000002"}
	case "AGENCY_STAFF", "AGENCY_ADMIN":
		return obj{"agency_slug": "sample-estates", "job_title": "Office", "contact_phone": "+263770000002"}
	}
	d := obj{
		"business_name": name, "tagline": "A tagline", "description": "What we do.\nIn two lines.", "city": "harare",
		"areas": []string{"Avondale"}, "services": []string{"Service one"}, "registration_number": "REG-12345", "contact_phone": "+263770000002",
	}
	if role == "SERVICE_PROVIDER" {
		d["trade"] = "plumbing"
	}
	return d
}

// requiredDocuments returns the document types a role must attach.
func requiredDocuments(role string) []string {
	var out []string
	for _, r := range onboarding.DefaultRequirements() {
		if r.Role == role && r.Required {
			out = append(out, r.Document)
		}
	}
	return out
}

// apply registers a new account with a role, fills the application in,
// uploads every required document and submits it.
func (e *env) apply(email, role, businessName string) (*http.Cookie, applicationBody) {
	e.t.Helper()
	c := e.selfRegister(email, "CONSUMER", role)
	wantStatus(e.t, e.put("/v1/me/onboarding/"+role, detailsFor(role, businessName), browser(c)), http.StatusOK)
	for _, doc := range requiredDocuments(role) {
		wantStatus(e.t, e.upload(c, role, doc, "scan.pdf", testPDF()), http.StatusCreated)
	}
	rec := e.post("/v1/me/onboarding/"+role+"/submit", nil, browser(c))
	wantStatus(e.t, rec, http.StatusOK)
	return c, decode[oneApplication](e.t, rec).Application
}

func (e *env) admin() *http.Cookie { return e.adminLogin(identity.SeedAdminEmail, seedPassword) }

// customerScopeAdmin returns a CUSTOMER-scope session of the seeded
// administrator. The API never issues one (a platform account cannot use the
// customer login), so it is built directly in the store: even then the
// platform role must not count.
func (e *env) customerScopeAdmin() *http.Cookie {
	e.t.Helper()
	e.seq++
	token := fmt.Sprintf("%043d", e.seq)
	sum := sha256.Sum256([]byte(token))
	if err := e.identity.CreateSession(e.t.Context(), identity.Session{
		ID: uuid.New(), UserID: identity.SeedAdminUserID, TokenHash: sum[:], Scope: auth.ScopeCustomer, Delivery: identity.DeliveryCookie,
		CreatedAt: e.now, LastSeenAt: e.now, ExpiresAt: e.now.Add(time.Hour),
	}); err != nil {
		e.t.Fatal(err)
	}
	return &http.Cookie{Name: auth.SessionCookieName, Value: token}
}

type approvedBody struct {
	Application adminApplicationBody `json:"application"`
	Created     struct {
		OrganisationID *string `json:"organisation_id"`
		Profile        *struct {
			Kind string `json:"kind"`
			Slug string `json:"slug"`
		} `json:"profile"`
		MembershipRole *string `json:"membership_role"`
	} `json:"created"`
}

type adminApplicationBody struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	Outcome   string `json:"outcome"`
	Applicant struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		CreatedAt     string `json:"created_at"`
	} `json:"applicant"`
	Details      map[string]any `json:"details"`
	Documents    []docBody      `json:"documents"`
	Requirements struct {
		Fields []struct {
			Name     string `json:"name"`
			Required bool   `json:"required"`
			Provided bool   `json:"provided"`
		} `json:"fields"`
		Documents []struct {
			Code     string `json:"code"`
			Required bool   `json:"required"`
			Provided bool   `json:"provided"`
		} `json:"documents"`
	} `json:"requirements"`
	History []struct {
		From      string  `json:"from"`
		To        string  `json:"to"`
		ActorName string  `json:"actor_name"`
		Reason    *string `json:"reason"`
		At        string  `json:"at"`
	} `json:"history"`
}

func (e *env) approve(admin *http.Cookie, id string) approvedBody {
	e.t.Helper()
	rec := e.post("/v1/admin/onboarding/"+id+"/approve", obj{"note": "Documents checked."}, browser(admin))
	wantStatus(e.t, rec, http.StatusOK)
	return decode[approvedBody](e.t, rec)
}

// approveSeededProvider approves the seeded SUBMITTED application and returns
// the slug of the profile it created.
func (e *env) approveSeededProvider() string {
	e.t.Helper()
	out := e.approve(e.admin(), devseed.PendingProviderApplicationID.String())
	if out.Created.Profile == nil {
		e.t.Fatal("approving a service provider created no profile")
	}
	return out.Created.Profile.Slug
}

func (e *env) onboardingEvents() map[string]int {
	out := map[string]int{}
	for _, ev := range e.onboard.Outbox() {
		out[ev.Type]++
	}
	return out
}

// storedFiles counts the files in the test's upload directory.
func (e *env) storedFiles() int {
	n := 0
	_ = filepath.Walk(e.files.Dir(), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func accountRole(u userBody, raw, role string) (status string, onboardingStatus string) {
	for _, r := range u.AccountRoles {
		if r.Role == role {
			status = r.Status
		}
	}
	if m := regexp.MustCompile(`"role":"` + role + `","status":"[A-Z_]+","onboarding_status":("([A-Z_]+)"|null)`).FindStringSubmatch(raw); m != nil {
		onboardingStatus = m[2]
	}
	return status, onboardingStatus
}

// ---------------------------------------------------------------------------
// Requirements
// ---------------------------------------------------------------------------

func TestOnboardingRequirements(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/v1/onboarding/requirements")
	wantStatus(t, rec, http.StatusOK)
	if cc := rec.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "public, max-age=") || rec.Header().Get("ETag") == "" {
		t.Fatalf("requirements must be public and cacheable: %q", cc)
	}
	var body struct {
		Roles []struct {
			Role             string  `json:"role"`
			Label            string  `json:"label"`
			Outcome          string  `json:"outcome"`
			OrganisationKind *string `json:"organisation_kind"`
			DirectoryKind    *string `json:"directory_kind"`
			Fields           []struct {
				Name     string `json:"name"`
				Required bool   `json:"required"`
			} `json:"fields"`
			Documents []struct {
				Code, Label, Description string
				Required                 bool
			} `json:"documents"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Roles) != 9 {
		t.Fatalf("%d onboarding roles, want 9", len(body.Roles))
	}
	seen := map[string]string{}
	for _, r := range body.Roles {
		if r.Role == "CONSUMER" || r.Role == "TENANT" || auth.IsPlatformRole(r.Role) {
			t.Fatalf("%s must have no onboarding", r.Role)
		}
		var docs, fields []string
		for _, d := range r.Documents {
			if d.Label == "" || d.Description == "" {
				t.Fatalf("%s: document %s has no label or description", r.Role, d.Code)
			}
			docs = append(docs, fmt.Sprintf("%s=%v", d.Code, d.Required))
		}
		for _, f := range r.Fields {
			fields = append(fields, fmt.Sprintf("%s=%v", f.Name, f.Required))
		}
		kind, dir := "-", "-"
		if r.OrganisationKind != nil {
			kind = *r.OrganisationKind
		}
		if r.DirectoryKind != nil {
			dir = *r.DirectoryKind
		}
		seen[r.Role] = r.Outcome + " " + kind + " " + dir + " | " + strings.Join(fields, ",") + " | " + strings.Join(docs, ",")
		if r.Label == "" || r.Label == r.Role {
			t.Fatalf("%s has no label", r.Role)
		}
	}
	const orgFields = "business_name=true,tagline=false,description=true,city=true,areas=false,services=false,"
	want := map[string]string{
		"PRIVATE_SELLER":   "NONE - - | contact_phone=true | PROOF_OF_IDENTITY=true,PROOF_OF_OWNERSHIP=true",
		"LANDLORD":         "ORGANISATION LANDLORD property-managers | " + orgFields + "registration_number=false,contact_phone=true | PROOF_OF_IDENTITY=true,BUSINESS_REGISTRATION=true,PROOF_OF_ADDRESS=true,TAX_CLEARANCE=false",
		"SERVICE_PROVIDER": "ORGANISATION SERVICE_PROVIDER service-providers | " + orgFields + "trade=true,registration_number=false,contact_phone=true | PROOF_OF_IDENTITY=true,BUSINESS_REGISTRATION=true,PROOF_OF_ADDRESS=true,TAX_CLEARANCE=false,TRADE_LICENCE=false",
		"AGENCY_OWNER":     "ORGANISATION AGENCY agencies | " + orgFields + "registration_number=false,contact_phone=true | PROOF_OF_IDENTITY=true,BUSINESS_REGISTRATION=true,PROOF_OF_ADDRESS=true,TAX_CLEARANCE=false,ESTATE_AGENTS_COUNCIL_CERTIFICATE=true",
		"AGENT":            "AGENCY_MEMBER - agents | agency_slug=true,job_title=false,bio=false,areas=false,contact_phone=true | PROOF_OF_IDENTITY=true,ESTATE_AGENTS_COUNCIL_CERTIFICATE=true",
		"AGENCY_STAFF":     "AGENCY_MEMBER - - | agency_slug=true,job_title=false,areas=false,contact_phone=true | PROOF_OF_IDENTITY=true,PROOF_OF_EMPLOYMENT=true",
		"AGENCY_ADMIN":     "AGENCY_MEMBER - - | agency_slug=true,job_title=false,areas=false,contact_phone=true | PROOF_OF_IDENTITY=true,PROOF_OF_EMPLOYMENT=true",
	}
	for role, w := range want {
		if seen[role] != w {
			t.Fatalf("%s:\n got %s\nwant %s", role, seen[role], w)
		}
	}

	// The tables are data: deactivating a document type or changing a
	// requirement changes the answer without a release.
	e.reference.SetActive(reference.SetOnboardingDocuments, "TAX_CLEARANCE", false)
	e.onboard.SetRequirement(onboarding.Requirement{Role: "PRIVATE_SELLER", Document: "PROOF_OF_ADDRESS", Required: true})
	after := e.get("/v1/onboarding/requirements").Body.String()
	if strings.Contains(after, "TAX_CLEARANCE") || strings.Count(after, `"code":"PROOF_OF_ADDRESS"`) != 6 {
		t.Fatalf("requirements are not read from the tables: %s", after)
	}
}

// The migration seeds the same outcome and requirement rows as the memory store.
func TestOnboardingMigrationSeedsTheBaseline(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "0009_onboarding.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	q := func(s string) string {
		if s == "" {
			return "NULL"
		}
		return "'" + s + "'"
	}
	for _, o := range onboarding.DefaultOutcomes() {
		want := fmt.Sprintf("VALUES ('%s', '%s', %s, %s, %s);", o.Role, o.Outcome, q(o.OrganisationKind), q(o.DirectoryKind), q(o.MembershipRole))
		if !strings.Contains(sql, want) {
			t.Fatalf("migration 0009 lacks the outcome row %s", want)
		}
	}
	for _, r := range onboarding.DefaultRequirements() {
		want := fmt.Sprintf("VALUES ('%s', '%s', %v);", r.Role, r.Document, r.Required)
		if !strings.Contains(sql, want) {
			t.Fatalf("migration 0009 lacks the requirement row %s", want)
		}
	}
	if got, want := strings.Count(sql, "INSERT INTO onboarding_role_outcomes"), len(onboarding.DefaultOutcomes()); got != want {
		t.Fatalf("%d outcome rows, want %d", got, want)
	}
	if got, want := strings.Count(sql, "INSERT INTO onboarding_role_documents"), len(onboarding.DefaultRequirements()); got != want {
		t.Fatalf("%d requirement rows, want %d", got, want)
	}
	for _, role := range append(auth.AllPlatformRoles(), "CONSUMER", "TENANT") {
		if strings.Contains(sql, "VALUES ('"+role+"'") {
			t.Fatalf("migration 0009 gives %s an onboarding outcome", role)
		}
	}
}

// ---------------------------------------------------------------------------
// Registration, access to one's own application
// ---------------------------------------------------------------------------

func TestRegistrationCreatesDraftApplications(t *testing.T) {
	e := newEnv(t)
	c := e.selfRegister("pro@example.com", "CONSUMER", "TENANT", "LANDLORD", "AGENT")
	apps := e.myOnboarding(c)
	if len(apps) != 2 || apps[0].Role != "LANDLORD" || apps[1].Role != "AGENT" {
		t.Fatalf("applications = %+v, want LANDLORD and AGENT only", apps)
	}
	for _, a := range apps {
		if a.Status != "DRAFT" || !a.CanEdit || a.CanSubmit || a.CanReapply || a.SubmittedAt != nil || a.Decision.Reason != nil || len(a.Documents) != 0 || a.RoleLabel == "" {
			t.Fatalf("new application = %+v", a)
		}
		// Every detail key is present; text is null and lists are arrays.
		if len(a.Details) != 12 || a.Details["business_name"] != nil || a.Details["areas"] == nil {
			t.Fatalf("details = %+v", a.Details)
		}
	}
	if got := strings.Join(apps[0].Missing.Fields, ","); got != "business_name,description,city,contact_phone" {
		t.Fatalf("landlord missing fields = %s", got)
	}
	if got := strings.Join(apps[0].Missing.Documents, ","); got != "PROOF_OF_IDENTITY,BUSINESS_REGISTRATION,PROOF_OF_ADDRESS" {
		t.Fatalf("landlord missing documents = %s", got)
	}

	// GET /v1/me shows each role's onboarding status; null for roles without onboarding.
	user, raw := e.me(c)
	for role, want := range map[string][2]string{
		"CONSUMER": {"ACTIVE", ""}, "TENANT": {"ACTIVE", ""}, "LANDLORD": {"PENDING_VERIFICATION", "DRAFT"}, "AGENT": {"PENDING_VERIFICATION", "DRAFT"},
	} {
		if status, ob := accountRole(user, raw, role); status != want[0] || ob != want[1] {
			t.Fatalf("%s: status %q onboarding %q, want %v (%s)", role, status, ob, want, raw)
		}
	}

	// Who may touch what.
	wantError(t, e.get("/v1/me/onboarding"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.put("/v1/me/onboarding/LANDLORD", obj{}), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	for _, role := range []string{"CONSUMER", "TENANT", "SUPER_ADMIN", "PLATFORM_ADMIN", "MODERATOR"} {
		wantError(t, e.put("/v1/me/onboarding/"+role, obj{}, browser(c)), http.StatusForbidden, httpx.CodeForbidden)
		wantError(t, e.post("/v1/me/onboarding/"+role+"/submit", nil, browser(c)), http.StatusForbidden, httpx.CodeForbidden)
		wantError(t, e.upload(c, role, "PROOF_OF_IDENTITY", "id.pdf", testPDF()), http.StatusForbidden, httpx.CodeForbidden)
	}
	for _, role := range []string{"HOST", "NOPE", "landlord"} { // not held, unknown, wrong case
		wantError(t, e.put("/v1/me/onboarding/"+role, obj{}, browser(c)), http.StatusNotFound, onboarding.CodeNotFound)
	}
	// The CSRF rule applies as everywhere: a cookie write needs the web's Origin.
	wantError(t, e.put("/v1/me/onboarding/LANDLORD", obj{}, withCookie(c)), http.StatusForbidden, httpx.CodeCSRFOrigin)

	// A consumer has no applications; the seeded professionals have none either.
	if apps := e.myOnboarding(e.loginCookie(identity.SeedConsumerEmail, seedPassword)); len(apps) != 0 {
		t.Fatalf("consumer applications = %+v", apps)
	}
	if apps := e.myOnboarding(e.loginCookie(identity.SeedAgentEmail, seedPassword)); len(apps) != 0 {
		t.Fatalf("seeded agent applications = %+v", apps)
	}
	// A platform role is never an onboarding role, whatever is registered.
	if n := len(e.onboardingEvents()); n != 0 {
		t.Fatalf("registration emitted onboarding events: %v", e.onboardingEvents())
	}
}

func TestOnboardingDetailsValidation(t *testing.T) {
	e := newEnv(t)
	c := e.selfRegister("pro@example.com", "SERVICE_PROVIDER", "AGENT", "PRIVATE_SELLER", "LANDLORD")
	save := func(role string, body obj) *httptest.ResponseRecorder {
		return e.put("/v1/me/onboarding/"+role, body, browser(c))
	}

	// Partial saves: only what is sent changes.
	rec := save("SERVICE_PROVIDER", obj{"business_name": "  Leak Fixers  "})
	wantStatus(t, rec, http.StatusOK)
	app := decode[oneApplication](t, rec).Application
	if app.Details["business_name"] != "Leak Fixers" || app.Status != "DRAFT" {
		t.Fatalf("after first save: %+v", app.Details)
	}
	wantStatus(t, save("SERVICE_PROVIDER", obj{"city": "harare", "trade": "plumbing", "areas": []string{"Avondale", "Avondale", " Borrowdale "}}), http.StatusOK)
	app = e.myApp(c, "SERVICE_PROVIDER")
	if app.Role != "SERVICE_PROVIDER" || app.Details["business_name"] != "Leak Fixers" || app.Details["city"] != "harare" ||
		fmt.Sprint(app.Details["areas"]) != "[Avondale Borrowdale]" {
		t.Fatalf("partial save lost or mangled data: %+v", app.Details)
	}
	// null (or an empty string) clears a text field.
	wantStatus(t, save("SERVICE_PROVIDER", obj{"city": nil, "trade": ""}), http.StatusOK)
	if app = e.myApp(c, "SERVICE_PROVIDER"); app.Details["city"] != nil || app.Details["trade"] != nil {
		t.Fatalf("null did not clear: %+v", app.Details)
	}

	cases := []struct {
		role  string
		body  obj
		field string
	}{
		{"SERVICE_PROVIDER", obj{"business_name": "X"}, "business_name"},
		{"SERVICE_PROVIDER", obj{"business_name": strings.Repeat("x", 121)}, "business_name"},
		{"SERVICE_PROVIDER", obj{"business_name": "<b>Bold</b> Plumbing"}, "business_name"},
		{"SERVICE_PROVIDER", obj{"tagline": strings.Repeat("x", 141)}, "tagline"},
		{"SERVICE_PROVIDER", obj{"description": strings.Repeat("x", 2001)}, "description"},
		{"SERVICE_PROVIDER", obj{"description": "Call us <script>alert(1)</script>"}, "description"},
		{"SERVICE_PROVIDER", obj{"city": "atlantis"}, "city"},
		{"SERVICE_PROVIDER", obj{"trade": "alchemy"}, "trade"},
		{"SERVICE_PROVIDER", obj{"areas": []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13"}}, "areas"},
		{"SERVICE_PROVIDER", obj{"services": []string{strings.Repeat("x", 61)}}, "services"},
		{"SERVICE_PROVIDER", obj{"services": "plumbing"}, "services"},
		{"SERVICE_PROVIDER", obj{"contact_phone": "0771234567"}, "contact_phone"},
		{"SERVICE_PROVIDER", obj{"contact_phone": "+263 77 123 4567"}, "contact_phone"},
		{"SERVICE_PROVIDER", obj{"business_name": 42}, "business_name"},
		// Fields of another outcome, and unknown fields.
		{"SERVICE_PROVIDER", obj{"agency_slug": "sample-estates"}, "agency_slug"},
		{"SERVICE_PROVIDER", obj{"bio": "x"}, "bio"},
		{"SERVICE_PROVIDER", obj{"status": "APPROVED"}, "status"},
		{"LANDLORD", obj{"trade": "plumbing"}, "trade"}, // a trade is for service providers only
		{"AGENT", obj{"business_name": "My Agency"}, "business_name"},
		{"AGENT", obj{"agency_slug": "no-such-agency"}, "agency_slug"},
		{"AGENT", obj{"agency_slug": "Sample Estates"}, "agency_slug"},
		{"AGENT", obj{"agency_slug": "sample-plumbers"}, "agency_slug"}, // a profile, but not an agency
		{"AGENT", obj{"bio": strings.Repeat("x", 1001)}, "bio"},
		{"PRIVATE_SELLER", obj{"business_name": "Mine"}, "business_name"},
		{"PRIVATE_SELLER", obj{"agency_slug": "sample-estates"}, "agency_slug"},
		{"PRIVATE_SELLER", obj{"contact_phone": "call me"}, "contact_phone"},
	}
	for _, tc := range cases {
		env := wantError(t, save(tc.role, tc.body), http.StatusBadRequest, httpx.CodeValidationFailed)
		if len(env.Error.Fields) != 1 || env.Error.Fields[tc.field] == "" {
			t.Fatalf("%s %v: fields = %v, want only %q", tc.role, tc.body, env.Error.Fields, tc.field)
		}
	}
	// Nothing invalid was stored.
	if app = e.myApp(c, "SERVICE_PROVIDER"); app.Details["business_name"] != "Leak Fixers" {
		t.Fatalf("a rejected save changed the application: %+v", app.Details)
	}
	// Several problems are reported together, and a bad body is refused.
	env := wantError(t, save("AGENT", obj{"agency_slug": "nope", "contact_phone": "x", "bio": "<i>x</i>"}), http.StatusBadRequest, httpx.CodeValidationFailed)
	if len(env.Error.Fields) != 3 {
		t.Fatalf("fields = %v", env.Error.Fields)
	}
	wantStatus(t, e.do(http.MethodPut, "/v1/me/onboarding/AGENT", []string{"x"}, browser(c)), http.StatusBadRequest)

	// An agency that is hidden or not verified cannot be joined.
	wantStatus(t, save("AGENT", obj{"agency_slug": "sample-estates"}), http.StatusOK)
	agency, err := e.directory.Get(t.Context(), directory.KindAgencies, "sample-realty")
	if err != nil {
		t.Fatal(err)
	}
	agency.Verified = false
	e.directory.Put(agency)
	wantError(t, save("AGENT", obj{"agency_slug": "sample-realty"}), http.StatusBadRequest, httpx.CodeValidationFailed)
}

// ---------------------------------------------------------------------------
// Documents
// ---------------------------------------------------------------------------

func TestOnboardingDocuments(t *testing.T) {
	e := newEnv(t)
	c := e.selfRegister("pro@example.com", "SERVICE_PROVIDER")
	seeded := e.storedFiles() // the demo application's sample documents

	// Happy path.
	rec := e.upload(c, "SERVICE_PROVIDER", "BUSINESS_REGISTRATION", `C:\Users\me\Documents\..\My <b>Certificate (2026).PDF`, testPDF())
	wantStatus(t, rec, http.StatusCreated)
	doc := decode[struct {
		Document docBody `json:"document"`
	}](t, rec).Document
	if doc.ContentType != "application/pdf" || doc.Type.Code != "BUSINESS_REGISTRATION" || doc.Type.Label != "Business registration" ||
		doc.FileName != "My bCertificate (2026).pdf" || doc.SizeBytes != int64(len(testPDF())) {
		t.Fatalf("document = %+v", doc)
	}
	// The response never names where the file is stored or its checksum.
	for _, private := range []string{"object_key", "sha256", "onboarding/", e.files.Dir()} {
		if strings.Contains(rec.Body.String(), private) {
			t.Fatalf("the upload response exposes %q: %s", private, rec.Body)
		}
	}
	if e.storedFiles() != seeded+1 {
		t.Fatalf("%d files stored, want %d", e.storedFiles(), seeded+1)
	}
	// The stored key is random and contains nothing the user sent.
	apps, _ := e.onboard.Current(t.Context(), uuid.MustParse(func() string { u, _ := e.me(c); return u.ID }()))
	key := apps[0].Documents[0].ObjectKey
	if !regexp.MustCompile(`^onboarding/[0-9a-f]{32}$`).MatchString(key) || len(apps[0].Documents[0].SHA256) != 64 {
		t.Fatalf("object key %q / checksum %q", key, apps[0].Documents[0].SHA256)
	}

	// The type is decided by the content: a PNG named .pdf is accepted as a
	// PNG and shown with the right extension; the part's declared type and
	// the extension are ignored.
	rec = e.upload(c, "SERVICE_PROVIDER", "PROOF_OF_IDENTITY", "passport.pdf", testPNG())
	wantStatus(t, rec, http.StatusCreated)
	if d := decode[struct {
		Document docBody `json:"document"`
	}](t, rec).Document; d.ContentType != "image/png" || d.FileName != "passport.png" {
		t.Fatalf("a PNG named .pdf = %+v", d)
	}
	// Anything else is refused, whatever it is called.
	for name, content := range map[string][]byte{
		"letter.pdf":  []byte("just some text pretending to be a PDF"),
		"page.pdf":    []byte("<html><body><script>alert(1)</script></body></html>"),
		"macro.pdf":   []byte("PK\x03\x04 zip archive"),
		"program.png": []byte("MZ\x90\x00 executable"),
		"vector.png":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
	} {
		wantError(t, e.upload(c, "SERVICE_PROVIDER", "PROOF_OF_ADDRESS", name, content), http.StatusUnsupportedMediaType, onboarding.CodeUnsupportedFile)
	}
	// Empty, oversize, unknown or missing type.
	env := wantError(t, e.upload(c, "SERVICE_PROVIDER", "PROOF_OF_ADDRESS", "empty.pdf", nil), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["file"] == "" {
		t.Fatalf("fields = %v", env.Error.Fields)
	}
	big := append(testPDF(), bytes.Repeat([]byte{' '}, onboarding.MaxDocumentBytes)...)
	wantError(t, e.upload(c, "SERVICE_PROVIDER", "PROOF_OF_ADDRESS", "big.pdf", big), http.StatusRequestEntityTooLarge, onboarding.CodeFileTooLarge)
	exact := append(testPDF(), bytes.Repeat([]byte{' '}, onboarding.MaxDocumentBytes-len(testPDF()))...)
	wantStatus(t, e.upload(c, "SERVICE_PROVIDER", "PROOF_OF_ADDRESS", "exactly-10mb.pdf", exact), http.StatusCreated)
	for _, code := range []string{"", "NOT_A_DOCUMENT", "PROOF_OF_OWNERSHIP", "../../etc/passwd"} { // PROOF_OF_OWNERSHIP is another role's
		env := wantError(t, e.upload(c, "SERVICE_PROVIDER", code, "x.pdf", testPDF()), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields["type"] == "" {
			t.Fatalf("type %q: fields = %v", code, env.Error.Fields)
		}
	}
	if e.storedFiles() != seeded+3 {
		t.Fatalf("a refused upload left a file behind: %d files, want %d", e.storedFiles(), seeded+3)
	}
	// Other routes keep the small body limit.
	wantStatus(t, e.put("/v1/me/onboarding/SERVICE_PROVIDER", obj{"description": strings.Repeat("x", 70<<10)}, browser(c)), http.StatusRequestEntityTooLarge)

	// Replacing a document of the same type keeps the newest and deletes the old file.
	rec = e.upload(c, "SERVICE_PROVIDER", "BUSINESS_REGISTRATION", "newer.png", testPNG())
	wantStatus(t, rec, http.StatusCreated)
	newer := decode[struct {
		Document docBody `json:"document"`
	}](t, rec).Document
	app := e.myOnboarding(c)[0]
	if len(app.Documents) != 3 || e.storedFiles() != seeded+3 {
		t.Fatalf("after a replacement: %d documents, %d files", len(app.Documents), e.storedFiles())
	}
	if _, err := e.files.Open(t.Context(), key); err == nil {
		t.Fatal("the replaced file is still stored")
	}
	if newer.ID == doc.ID || newer.FileName != "newer.png" {
		t.Fatalf("replacement = %+v", newer)
	}

	// Reading it back: the owner only, as a download that is never cached or sniffed.
	file := e.get("/v1/me/onboarding/SERVICE_PROVIDER/documents/"+newer.ID+"/file", withCookie(c))
	wantStatus(t, file, http.StatusOK)
	h := file.Header()
	if !bytes.Equal(file.Body.Bytes(), testPNG()) || h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Cache-Control") != "private, no-store" || !strings.HasPrefix(h.Get("Content-Disposition"), `attachment; filename="newer.png"`) {
		t.Fatalf("download headers = %v", h)
	}
	wantError(t, e.get("/v1/me/onboarding/SERVICE_PROVIDER/documents/"+newer.ID+"/file"), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// Another user cannot see the application, read, replace or delete the document.
	other := e.selfRegister("other@example.com", "SERVICE_PROVIDER")
	if apps := e.myOnboarding(other); len(apps) != 1 || len(apps[0].Documents) != 0 || apps[0].ID == app.ID {
		t.Fatalf("another user sees %+v", apps)
	}
	wantError(t, e.get("/v1/me/onboarding/SERVICE_PROVIDER/documents/"+newer.ID+"/file", withCookie(other)), http.StatusNotFound, onboarding.CodeDocumentNotFound)
	wantError(t, e.do(http.MethodDelete, "/v1/me/onboarding/SERVICE_PROVIDER/documents/"+newer.ID, nil, browser(other)), http.StatusNotFound, onboarding.CodeDocumentNotFound)
	wantError(t, e.get("/v1/me/onboarding/SERVICE_PROVIDER/documents/not-a-uuid/file", withCookie(c)), http.StatusNotFound, onboarding.CodeDocumentNotFound)
	// A consumer and an organisation member cannot either.
	for _, email := range []string{identity.SeedConsumerEmail, identity.SeedAgentEmail} {
		wantStatus(t, e.get("/v1/me/onboarding/SERVICE_PROVIDER/documents/"+newer.ID+"/file", withCookie(e.loginCookie(email, seedPassword))), http.StatusNotFound)
	}

	// No public route serves a stored file, by key or by ID.
	stored, _ := e.onboard.Current(t.Context(), apps[0].UserID)
	for _, d := range stored[0].Documents {
		for _, path := range []string{"/" + d.ObjectKey, "/v1/" + d.ObjectKey, "/uploads/" + d.ObjectKey, "/tmp/uploads/" + d.ObjectKey,
			"/v1/onboarding/documents/" + d.ID.String(), "/v1/onboarding/documents/" + d.ID.String() + "/file",
			"/v1/admin/onboarding/" + app.ID + "/documents/" + d.ID.String() + "/file"} {
			if rec := e.get(path); rec.Code == http.StatusOK || bytes.Contains(rec.Body.Bytes(), testPNG()) {
				t.Fatalf("GET %s without a session answered %d", path, rec.Code)
			}
		}
	}

	// Deleting: the owner, while editable.
	wantStatus(t, e.do(http.MethodDelete, "/v1/me/onboarding/SERVICE_PROVIDER/documents/"+newer.ID, nil, browser(c)), http.StatusNoContent)
	wantError(t, e.do(http.MethodDelete, "/v1/me/onboarding/SERVICE_PROVIDER/documents/"+newer.ID, nil, browser(c)), http.StatusNotFound, onboarding.CodeDocumentNotFound)
	if app = e.myOnboarding(c)[0]; len(app.Documents) != 2 || e.storedFiles() != seeded+2 {
		t.Fatalf("after a delete: %d documents, %d files", len(app.Documents), e.storedFiles())
	}
}

func TestOnboardingDocumentLimit(t *testing.T) {
	e := newEnv(t)
	c := e.selfRegister("pro@example.com", "PRIVATE_SELLER")
	// Ten is the limit, so the role needs more than ten document types to hit it.
	for i := 1; i <= 11; i++ {
		code := fmt.Sprintf("EXTRA_%02d", i)
		desc := "An extra document."
		e.reference.Put(reference.SetOnboardingDocuments, reference.Item{Code: code, Label: "Extra " + code, Description: &desc, SortOrder: 900 + i, Active: true})
		e.onboard.SetRequirement(onboarding.Requirement{Role: "PRIVATE_SELLER", Document: code, Required: false})
	}
	for i := 1; i <= 10; i++ {
		wantStatus(t, e.upload(c, "PRIVATE_SELLER", fmt.Sprintf("EXTRA_%02d", i), "x.pdf", testPDF()), http.StatusCreated)
	}
	files := e.storedFiles()
	wantError(t, e.upload(c, "PRIVATE_SELLER", "EXTRA_11", "x.pdf", testPDF()), http.StatusConflict, onboarding.CodeTooManyDocuments)
	// Replacing one of the ten is still allowed.
	wantStatus(t, e.upload(c, "PRIVATE_SELLER", "EXTRA_03", "y.pdf", testPDF()), http.StatusCreated)
	if app := e.myOnboarding(c)[0]; len(app.Documents) != 10 || e.storedFiles() != files {
		t.Fatalf("%d documents, %d files (want 10 and %d)", len(app.Documents), e.storedFiles(), files)
	}
}

func TestOnboardingUploadRateLimit(t *testing.T) {
	e := newEnv(t)
	c := e.selfRegister("pro@example.com", "PRIVATE_SELLER")
	post := func() int {
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		_ = w.WriteField("type", "PROOF_OF_IDENTITY")
		part, _ := w.CreateFormFile("file", "id.pdf")
		_, _ = part.Write(testPDF())
		_ = w.Close()
		req := httptest.NewRequest(http.MethodPost, "/v1/me/onboarding/PRIVATE_SELLER/documents", &body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		browser(c)(req)
		req.RemoteAddr = "203.0.113.77:40000"
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < UploadRequestsBurst; i++ {
		if code := post(); code != http.StatusCreated {
			t.Fatalf("upload %d = %d", i+1, code)
		}
	}
	if code := post(); code != http.StatusTooManyRequests {
		t.Fatalf("upload beyond the limit = %d, want 429", code)
	}
}

// ---------------------------------------------------------------------------
// Submitting and the state rules
// ---------------------------------------------------------------------------

func TestOnboardingSubmitAndStateRules(t *testing.T) {
	e := newEnv(t)
	c := e.selfRegister("pro@example.com", "SERVICE_PROVIDER")
	submit := func() *httptest.ResponseRecorder {
		return e.post("/v1/me/onboarding/SERVICE_PROVIDER/submit", nil, browser(c))
	}

	// Everything missing is listed, fields and documents together.
	env := wantError(t, submit(), http.StatusBadRequest, httpx.CodeValidationFailed)
	want := []string{"details.business_name", "details.description", "details.city", "details.trade", "details.contact_phone",
		"documents.PROOF_OF_IDENTITY", "documents.BUSINESS_REGISTRATION", "documents.PROOF_OF_ADDRESS"}
	if len(env.Error.Fields) != len(want) {
		t.Fatalf("fields = %v, want %v", env.Error.Fields, want)
	}
	for _, f := range want {
		if env.Error.Fields[f] == "" {
			t.Fatalf("missing %q in %v", f, env.Error.Fields)
		}
	}
	// Partly complete: only what is still missing is listed.
	wantStatus(t, e.put("/v1/me/onboarding/SERVICE_PROVIDER", detailsFor("SERVICE_PROVIDER", "Leak Fixers"), browser(c)), http.StatusOK)
	wantStatus(t, e.upload(c, "SERVICE_PROVIDER", "PROOF_OF_IDENTITY", "id.png", testPNG()), http.StatusCreated)
	wantStatus(t, e.upload(c, "SERVICE_PROVIDER", "TRADE_LICENCE", "licence.pdf", testPDF()), http.StatusCreated) // optional
	env = wantError(t, submit(), http.StatusBadRequest, httpx.CodeValidationFailed)
	if len(env.Error.Fields) != 2 || env.Error.Fields["documents.BUSINESS_REGISTRATION"] == "" || env.Error.Fields["documents.PROOF_OF_ADDRESS"] == "" {
		t.Fatalf("fields = %v", env.Error.Fields)
	}
	if app := e.myOnboarding(c)[0]; app.CanSubmit || len(app.Missing.Fields) != 0 || len(app.Missing.Documents) != 2 {
		t.Fatalf("application = %+v", app)
	}
	wantStatus(t, e.upload(c, "SERVICE_PROVIDER", "BUSINESS_REGISTRATION", "reg.pdf", testPDF()), http.StatusCreated)
	wantStatus(t, e.upload(c, "SERVICE_PROVIDER", "PROOF_OF_ADDRESS", "bill.pdf", testPDF()), http.StatusCreated)
	if app := e.myOnboarding(c)[0]; !app.CanSubmit {
		t.Fatalf("a complete application cannot be submitted: %+v", app.Missing)
	}

	rec := submit()
	wantStatus(t, rec, http.StatusOK)
	app := decode[oneApplication](t, rec).Application
	if app.Status != "SUBMITTED" || app.CanEdit || app.CanSubmit || app.SubmittedAt == nil {
		t.Fatalf("submitted application = %+v", app)
	}
	if ev := e.onboardingEvents(); ev[onboarding.EventSubmitted] != 1 || len(ev) != 1 {
		t.Fatalf("events = %v, want one %s", ev, onboarding.EventSubmitted)
	}
	user, raw := e.me(c)
	if status, ob := accountRole(user, raw, "SERVICE_PROVIDER"); status != "PENDING_VERIFICATION" || ob != "SUBMITTED" {
		t.Fatalf("after submit: %s / %s", status, ob)
	}
	if len(user.Memberships) != 0 {
		t.Fatal("submitting must not create a membership")
	}

	// Nothing can be changed while it is being reviewed.
	doc := app.Documents[0].ID
	wantError(t, e.put("/v1/me/onboarding/SERVICE_PROVIDER", obj{"tagline": "New"}, browser(c)), http.StatusConflict, onboarding.CodeInvalidState)
	wantError(t, e.upload(c, "SERVICE_PROVIDER", "TAX_CLEARANCE", "tax.pdf", testPDF()), http.StatusConflict, onboarding.CodeInvalidState)
	wantError(t, e.do(http.MethodDelete, "/v1/me/onboarding/SERVICE_PROVIDER/documents/"+doc, nil, browser(c)), http.StatusConflict, onboarding.CodeInvalidState)
	wantError(t, submit(), http.StatusConflict, onboarding.CodeInvalidState)
	// ...but the owner can still read it and download the documents.
	wantStatus(t, e.get("/v1/me/onboarding/SERVICE_PROVIDER/documents/"+doc+"/file", withCookie(c)), http.StatusOK)
	// The applicant cannot decide their own application.
	wantError(t, e.post("/v1/admin/onboarding/"+app.ID+"/approve", obj{}, browser(c)), http.StatusForbidden, httpx.CodeForbidden)

	// Changes requested: editable again, then resubmitted.
	admin := e.admin()
	rec = e.post("/v1/admin/onboarding/"+app.ID+"/request-changes", obj{"reason": "The proof of address is too old."}, browser(admin))
	wantStatus(t, rec, http.StatusOK)
	app = e.myOnboarding(c)[0]
	if app.Status != "CHANGES_REQUESTED" || !app.CanEdit || !app.CanSubmit || app.Decision.Reason == nil || *app.Decision.Reason != "The proof of address is too old." || app.Decision.DecidedAt == nil {
		t.Fatalf("after changes requested: %+v", app)
	}
	wantStatus(t, e.put("/v1/me/onboarding/SERVICE_PROVIDER", obj{"tagline": "Now with a new tagline"}, browser(c)), http.StatusOK)
	wantStatus(t, e.upload(c, "SERVICE_PROVIDER", "PROOF_OF_ADDRESS", "new-bill.pdf", testPDF()), http.StatusCreated)
	wantStatus(t, submit(), http.StatusOK)
	if app = e.myOnboarding(c)[0]; app.Status != "SUBMITTED" || app.Decision.Reason != nil || app.Details["tagline"] != "Now with a new tagline" {
		t.Fatalf("after resubmitting: %+v", app)
	}
	if ev := e.onboardingEvents(); ev[onboarding.EventSubmitted] != 2 || ev[onboarding.EventChangesRequested] != 1 || len(ev) != 2 {
		t.Fatalf("events = %v", ev)
	}

	// Every transition is in the history, in order, with who did it.
	detail := decode[struct {
		Application adminApplicationBody `json:"application"`
	}](t, e.get("/v1/admin/onboarding/"+app.ID, withCookie(admin))).Application
	var steps []string
	for _, h := range detail.History {
		steps = append(steps, h.From+">"+h.To+" by "+h.ActorName)
	}
	if got := strings.Join(steps, "; "); got != "DRAFT>SUBMITTED by Rudo Ncube; SUBMITTED>CHANGES_REQUESTED by Sam Admin; CHANGES_REQUESTED>SUBMITTED by Rudo Ncube" {
		t.Fatalf("history = %s", got)
	}
	if detail.History[1].Reason == nil || *detail.History[1].Reason != "The proof of address is too old." {
		t.Fatalf("history reason = %+v", detail.History[1])
	}
	// And as audit lines: who, what, from and to. Never the reason's text or file names.
	logs := e.logs.String()
	for _, want := range []string{`"event":"onboarding.submitted"`, `"event":"onboarding.decided"`, `"to":"CHANGES_REQUESTED"`, `"audit":true`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("audit log lacks %s", want)
		}
	}
	for _, private := range []string{"too old", "new-bill", "+263770000002", "REG-12345"} {
		if strings.Contains(logs, private) {
			t.Fatalf("the log contains %q", private)
		}
	}
}

// ---------------------------------------------------------------------------
// Administrators
// ---------------------------------------------------------------------------

type adminListBody struct {
	Items []struct {
		ID        string `json:"id"`
		Role      string `json:"role"`
		RoleLabel string `json:"role_label"`
		Status    string `json:"status"`
		Applicant struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"applicant"`
		BusinessName *string `json:"business_name"`
		Agency       *struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"agency"`
		SubmittedAt   *string `json:"submitted_at"`
		DocumentCount int     `json:"document_count"`
	} `json:"items"`
	Page       int            `json:"page"`
	PageSize   int            `json:"page_size"`
	Total      int            `json:"total"`
	TotalPages int            `json:"total_pages"`
	Counts     map[string]int `json:"counts"`
}

// platformAdmin creates an administrator with one platform role and signs
// them in at the administrator endpoint.
func (e *env) platformAdmin(email, role, market string) *http.Cookie {
	e.t.Helper()
	hash, err := identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1}.Hash(seedPassword)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.identity.CreatePlatformAdmin(e.t.Context(), identity.NewPlatformAdmin{
		ID: uuid.New(), Name: "Admin " + role, Email: email, PasswordHash: hash, Role: role, MarketCode: market, CreatedAt: e.now,
	}); err != nil {
		e.t.Fatal(err)
	}
	e.seq++
	return e.adminLogin(email, seedPassword, withIP(fmt.Sprintf("192.0.2.%d", 100+e.seq%100)))
}

func TestOnboardingAdminAccess(t *testing.T) {
	e := newEnv(t)
	seeded := devseed.PendingProviderApplicationID.String()
	docs, _ := e.onboard.Get(t.Context(), devseed.PendingProviderApplicationID)
	docID := docs.Documents[0].ID.String()
	paths := []string{"/v1/admin/onboarding", "/v1/admin/onboarding/" + seeded, "/v1/admin/onboarding/" + seeded + "/documents/" + docID + "/file"}

	// Anonymous: 401. A customer, an organisation member, and an
	// administrator signed in at the CUSTOMER login: 403.
	for _, p := range paths {
		wantError(t, e.get(p), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		for _, email := range []string{identity.SeedConsumerEmail, identity.SeedAgentEmail, devseed.EstatesOwnerEmail, devseed.PendingProviderEmail} {
			wantError(t, e.get(p, withCookie(e.loginCookie(email, seedPassword))), http.StatusForbidden, httpx.CodeForbidden)
		}
		wantError(t, e.get(p, withCookie(e.customerScopeAdmin())), http.StatusForbidden, httpx.CodeForbidden)
	}
	customerScopeAdmin := e.customerScopeAdmin()
	for _, action := range []string{"approve", "reject", "request-changes"} {
		body := obj{"reason": "Trying it on."}
		if action == "approve" {
			body = obj{"note": "Trying it on."}
		}
		wantError(t, e.post("/v1/admin/onboarding/"+seeded+"/"+action, body), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		wantError(t, e.post("/v1/admin/onboarding/"+seeded+"/"+action, body, browser(customerScopeAdmin)), http.StatusForbidden, httpx.CodeForbidden)
	}
	// Platform roles that do not review applications: 403 even with an admin session.
	for _, role := range []string{auth.PlatformFinanceAdmin, auth.PlatformContentAdmin, auth.PlatformSupport} {
		c := e.platformAdmin(strings.ToLower(role)+"@example.test", role, "")
		for _, p := range paths {
			wantError(t, e.get(p, withCookie(c)), http.StatusForbidden, httpx.CodeForbidden)
		}
		wantError(t, e.post("/v1/admin/onboarding/"+seeded+"/approve", obj{}, browser(c)), http.StatusForbidden, httpx.CodeForbidden)
	}
	if app, _ := e.onboard.Get(t.Context(), devseed.PendingProviderApplicationID); app.Status != "SUBMITTED" {
		t.Fatalf("a refused request changed the application: %s", app.Status)
	}

	// SUPER_ADMIN, PLATFORM_ADMIN and MODERATOR see everything.
	super := e.admin()
	for _, c := range []*http.Cookie{super, e.platformAdmin("platform@example.test", auth.PlatformAdmin, ""), e.platformAdmin("moderator@example.test", auth.PlatformModerator, "")} {
		rec := e.get("/v1/admin/onboarding", withCookie(c))
		wantStatus(t, rec, http.StatusOK)
		list := decode[adminListBody](t, rec)
		// Default: SUBMITTED, oldest first. The seed has one SUBMITTED and one DRAFT.
		if list.Total != 1 || len(list.Items) != 1 || list.Items[0].ID != seeded || list.Page != 1 || list.PageSize != 20 || list.TotalPages != 1 {
			t.Fatalf("list = %+v", list)
		}
		it := list.Items[0]
		if it.Role != "SERVICE_PROVIDER" || it.RoleLabel != "Service provider" || it.Status != "SUBMITTED" || it.Applicant.Email != devseed.PendingProviderEmail ||
			it.Applicant.Name != "Pamela Provider" || it.BusinessName == nil || *it.BusinessName != "Sample Roofing Repairs" || it.Agency != nil ||
			it.SubmittedAt == nil || it.DocumentCount != 3 {
			t.Fatalf("item = %+v", it)
		}
		if fmt.Sprint(list.Counts) != "map[APPROVED:0 CHANGES_REQUESTED:0 DRAFT:1 REJECTED:0 SUBMITTED:1]" {
			t.Fatalf("counts = %v", list.Counts)
		}
		if rec.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("the admin list must not be cacheable")
		}
	}

	// Filters and paging.
	_, first := e.apply("a@example.com", "LANDLORD", "Alpha Lettings")
	_, second := e.apply("b@example.com", "AGENT", "")
	e.selfRegister("c@example.com", "HOST")
	list := decode[adminListBody](t, e.get("/v1/admin/onboarding", withCookie(super)))
	if list.Total != 3 || list.Items[0].ID != seeded || list.Items[1].ID != first.ID || list.Items[2].ID != second.ID {
		t.Fatalf("oldest first: %+v", list.Items)
	}
	if a := list.Items[2].Agency; a == nil || a.Slug != "sample-estates" || a.Name != "Sample Estates" || list.Items[2].BusinessName != nil {
		t.Fatalf("agent item = %+v", list.Items[2])
	}
	if fmt.Sprint(list.Counts) != "map[APPROVED:0 CHANGES_REQUESTED:0 DRAFT:2 REJECTED:0 SUBMITTED:3]" {
		t.Fatalf("counts = %v", list.Counts)
	}
	for query, want := range map[string]int{
		"status=DRAFT": 2, "role=LANDLORD": 1, "role=HOST": 0, "status=DRAFT&role=HOST": 1, "q=alpha": 1, "q=B%40EXAMPLE.com": 1, "q=pamela": 1,
		"q=nobody": 0, "status=APPROVED": 0, "page_size=2": 3, "page=2&page_size=2": 3,
	} {
		got := decode[adminListBody](t, e.get("/v1/admin/onboarding?"+query, withCookie(super)))
		if got.Total != want {
			t.Fatalf("?%s total = %d, want %d", query, got.Total, want)
		}
	}
	if page := decode[adminListBody](t, e.get("/v1/admin/onboarding?page=2&page_size=2", withCookie(super))); len(page.Items) != 1 || page.TotalPages != 2 {
		t.Fatalf("page 2 = %+v", page)
	}
	for _, bad := range []string{"status=NOPE", "role=CONSUMER", "role=SUPER_ADMIN", "page=0", "page_size=500", "q=" + strings.Repeat("x", 101)} {
		wantError(t, e.get("/v1/admin/onboarding?"+bad, withCookie(super)), http.StatusBadRequest, httpx.CodeValidationFailed)
	}

	// Detail: the applicant, the private details, the checklist, the history.
	rec := e.get("/v1/admin/onboarding/"+seeded, withCookie(super))
	wantStatus(t, rec, http.StatusOK)
	d := decode[struct {
		Application adminApplicationBody `json:"application"`
	}](t, rec).Application
	if d.Applicant.Email != devseed.PendingProviderEmail || d.Applicant.ID != devseed.PendingProviderID.String() || !d.Applicant.EmailVerified || d.Applicant.CreatedAt == "" ||
		d.Details["contact_phone"] != "+263770000001" || d.Details["registration_number"] != "SAMPLE-0001" || d.Outcome != "ORGANISATION" ||
		len(d.Documents) != 3 || len(d.History) != 1 || d.History[0].To != "SUBMITTED" {
		t.Fatalf("detail = %+v", d)
	}
	for _, f := range d.Requirements.Fields {
		if f.Required && !f.Provided {
			t.Fatalf("checklist field %+v", f)
		}
	}
	provided := 0
	for _, doc := range d.Requirements.Documents {
		if doc.Required != doc.Provided {
			t.Fatalf("checklist document %+v", doc)
		}
		if doc.Provided {
			provided++
		}
	}
	if provided != 3 || len(d.Requirements.Documents) != 5 {
		t.Fatalf("checklist = %+v", d.Requirements.Documents)
	}
	for _, private := range []string{"object_key", "sha256", "onboarding/devseed"} {
		if strings.Contains(rec.Body.String(), private) {
			t.Fatalf("the admin detail exposes %q", private)
		}
	}
	wantError(t, e.get("/v1/admin/onboarding/"+uuid.NewString(), withCookie(super)), http.StatusNotFound, onboarding.CodeNotFound)
	wantError(t, e.get("/v1/admin/onboarding/not-a-uuid", withCookie(super)), http.StatusNotFound, onboarding.CodeNotFound)

	// A document: streamed as a private download, and every access is audited.
	e.logs.Reset()
	file := e.get(paths[2], withCookie(super))
	wantStatus(t, file, http.StatusOK)
	if file.Header().Get("X-Content-Type-Options") != "nosniff" || file.Header().Get("Cache-Control") != "private, no-store" ||
		!strings.HasPrefix(file.Header().Get("Content-Disposition"), "attachment; ") || file.Body.Len() == 0 {
		t.Fatalf("admin download headers = %v", file.Header())
	}
	logs := e.logs.String()
	for _, want := range []string{`"audit":true`, `"event":"onboarding.document_accessed"`, `"document_id":"` + docID + `"`, `"actor_user_id":"` + identity.SeedAdminUserID.String() + `"`, `"at":"`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("the access was not audited (%s): %s", want, logs)
		}
	}
	if strings.Contains(logs, "sample-") || strings.Contains(logs, "%PDF") {
		t.Fatalf("the audit line carries the file name or content: %s", logs)
	}
	wantError(t, e.get("/v1/admin/onboarding/"+first.ID+"/documents/"+docID+"/file", withCookie(super)), http.StatusNotFound, onboarding.CodeDocumentNotFound)

	// MARKET_ADMIN: only the applications of its own market.
	zw := e.platformAdmin("zw@example.test", auth.PlatformMarketAdmin, "ZW")
	za := e.platformAdmin("za@example.test", auth.PlatformMarketAdmin, "ZA")
	if got := decode[adminListBody](t, e.get("/v1/admin/onboarding", withCookie(zw))); got.Total != 3 || got.Counts["DRAFT"] != 0 {
		t.Fatalf("ZW market admin: %+v", got) // drafts without a city or agency belong to no market yet
	}
	if got := decode[adminListBody](t, e.get("/v1/admin/onboarding", withCookie(za))); got.Total != 0 || got.Counts["SUBMITTED"] != 0 {
		t.Fatalf("ZA market admin sees ZW applications: %+v", got)
	}
	wantStatus(t, e.get("/v1/admin/onboarding/"+seeded, withCookie(zw)), http.StatusOK)
	wantError(t, e.get("/v1/admin/onboarding/"+seeded, withCookie(za)), http.StatusNotFound, onboarding.CodeNotFound)
	wantError(t, e.get(paths[2], withCookie(za)), http.StatusNotFound, onboarding.CodeNotFound)
	wantError(t, e.post("/v1/admin/onboarding/"+seeded+"/approve", obj{}, browser(za)), http.StatusNotFound, onboarding.CodeNotFound)
	wantError(t, e.post("/v1/admin/onboarding/"+seeded+"/reject", obj{"reason": "Not my market."}, browser(za)), http.StatusNotFound, onboarding.CodeNotFound)
	// An application that belongs to no market (a private seller) is for global administrators only.
	_, seller := e.apply("seller2@example.com", "PRIVATE_SELLER", "")
	wantError(t, e.get("/v1/admin/onboarding/"+seller.ID, withCookie(zw)), http.StatusNotFound, onboarding.CodeNotFound)
	wantStatus(t, e.get("/v1/admin/onboarding/"+seller.ID, withCookie(super)), http.StatusOK)
	// The market admin of the right market can decide.
	wantStatus(t, e.post("/v1/admin/onboarding/"+first.ID+"/request-changes", obj{"reason": "Please add a tagline."}, browser(zw)), http.StatusOK)
}

// ---------------------------------------------------------------------------
// Approval
// ---------------------------------------------------------------------------

func TestOnboardingApprovalCreatesAVerifiedProfile(t *testing.T) {
	e := newEnv(t)
	admin := e.admin()
	id := devseed.PendingProviderApplicationID.String()
	applicant := e.loginCookie(devseed.PendingProviderEmail, seedPassword)

	// Before: no profile, no membership, no inbox.
	wantError(t, e.get("/v1/directory/service-providers/sample-roofing-repairs"), http.StatusNotFound, directory.CodeProfileNotFound)
	wantError(t, e.get("/v1/inbox/business-enquiries", withCookie(applicant)), http.StatusForbidden, httpx.CodeForbidden)
	before := decode[struct {
		Total int `json:"total"`
	}](t, e.get("/v1/directory/service-providers")).Total

	out := e.approve(admin, id)
	c := out.Created
	if c.OrganisationID == nil || c.Profile == nil || c.Profile.Kind != "service-providers" || c.Profile.Slug != "sample-roofing-repairs" ||
		c.MembershipRole == nil || *c.MembershipRole != "OWNER" || out.Application.Status != "APPROVED" {
		t.Fatalf("approval = %+v", out)
	}
	if len(out.Application.History) != 2 || out.Application.History[1].To != "APPROVED" || out.Application.History[1].ActorName != "Sam Admin" {
		t.Fatalf("history = %+v", out.Application.History)
	}

	// The profile is public, verified, and carries public content only.
	rec := e.get("/v1/directory/service-providers/sample-roofing-repairs")
	wantStatus(t, rec, http.StatusOK)
	p := decode[struct {
		Profile struct {
			Name     string   `json:"name"`
			Verified bool     `json:"verified"`
			Tagline  string   `json:"tagline"`
			Areas    []string `json:"areas"`
			Services []string `json:"services"`
			City     struct{ Code string }
			Trade    *struct{ Code string }
		} `json:"profile"`
	}](t, rec).Profile
	if p.Name != "Sample Roofing Repairs" || !p.Verified || p.City.Code != "harare" || p.Trade == nil || len(p.Areas) != 2 || len(p.Services) != 2 || p.Tagline == "" {
		t.Fatalf("profile = %+v", p)
	}
	list := e.get("/v1/directory/service-providers?q=roofing")
	if !strings.Contains(list.Body.String(), `"slug":"sample-roofing-repairs"`) || !strings.Contains(list.Body.String(), `"verified":true`) {
		t.Fatalf("the profile is not in the directory: %s", list.Body)
	}
	if after := decode[struct {
		Total int `json:"total"`
	}](t, e.get("/v1/directory/service-providers")).Total; after != before+1 {
		t.Fatalf("directory total %d -> %d", before, after)
	}
	if !strings.Contains(e.get("/v1/sitemap").Body.String(), `"slug":"sample-roofing-repairs"`) {
		t.Fatal("the profile is not in the sitemap")
	}
	for _, path := range []string{"/v1/directory/service-providers/sample-roofing-repairs", "/v1/directory/service-providers?page_size=48", "/v1/sitemap"} {
		body := strings.ToLower(e.get(path).Body.String())
		for _, private := range []string{"+263770000001", "770000001", "sample-0001", "pending.provider", "pamela", "registration_number", "contact_phone", "proof_of", "sample-proof", "devseed"} {
			if strings.Contains(body, private) {
				t.Fatalf("%s exposes %q", path, private)
			}
		}
	}

	// The applicant: the role is ACTIVE, they own the new organisation.
	user, raw := e.me(applicant)
	if status, ob := accountRole(user, raw, "SERVICE_PROVIDER"); status != "ACTIVE" || ob != "APPROVED" {
		t.Fatalf("role = %s / %s", status, ob)
	}
	if len(user.Memberships) != 1 || user.Memberships[0].Role != "OWNER" || user.Memberships[0].OrganisationKind != "SERVICE_PROVIDER" ||
		user.Memberships[0].OrganisationName != "Sample Roofing Repairs" || user.Memberships[0].OrganisationID != *c.OrganisationID || user.Memberships[0].MarketCode != "ZW" {
		t.Fatalf("memberships = %+v", user.Memberships)
	}
	if len(user.PlatformRoles) != 0 {
		t.Fatal("an approval must never grant a platform role")
	}
	mine := e.myOnboarding(applicant)[0]
	if mine.Status != "APPROVED" || mine.CanEdit || mine.Profile == nil || mine.Profile.Slug != "sample-roofing-repairs" {
		t.Fatalf("applicant view = %+v", mine)
	}
	// ...they can use the inboxes, and the new profile receives enquiries.
	wantStatus(t, e.get("/v1/agent/property-requests", withCookie(applicant)), http.StatusOK)
	wantStatus(t, e.post("/v1/business-enquiries", e.enquiry("service-providers", "sample-roofing-repairs")), http.StatusCreated)
	inbox := e.get("/v1/inbox/business-enquiries", withCookie(applicant))
	wantStatus(t, inbox, http.StatusOK)
	if !strings.Contains(inbox.Body.String(), "the geyser is leaking") {
		t.Fatalf("the enquiry did not reach the new owner: %s", inbox.Body)
	}
	// Nobody else's inbox has it.
	if got := e.enquiryInbox(plumbersEmail); len(got) != 0 {
		t.Fatalf("another business received it: %v", got)
	}

	// One event, one email; deciding twice is refused; nothing can be edited.
	if ev := e.onboardingEvents(); ev[onboarding.EventApproved] != 1 || len(ev) != 1 {
		t.Fatalf("events = %v", ev)
	}
	wantError(t, e.post("/v1/admin/onboarding/"+id+"/approve", obj{}, browser(admin)), http.StatusConflict, onboarding.CodeInvalidState)
	wantError(t, e.post("/v1/admin/onboarding/"+id+"/reject", obj{"reason": "Changed my mind."}, browser(admin)), http.StatusConflict, onboarding.CodeInvalidState)
	wantError(t, e.put("/v1/me/onboarding/SERVICE_PROVIDER", obj{"tagline": "x"}, browser(applicant)), http.StatusConflict, onboarding.CodeInvalidState)
	// The documents stay private after approval.
	wantStatus(t, e.get("/v1/admin/onboarding/"+id+"/documents/"+out.Application.Documents[0].ID+"/file"), http.StatusUnauthorized)

	// A second business with the same name gets a numbered slug.
	_, twin := e.apply("twin@example.com", "SERVICE_PROVIDER", "Sample Roofing Repairs")
	if got := e.approve(admin, twin.ID).Created.Profile.Slug; got != "sample-roofing-repairs-2" {
		t.Fatalf("second slug = %q", got)
	}
	_, third := e.apply("third@example.com", "SERVICE_PROVIDER", "Sample   Roofing & Repairs!!")
	if got := e.approve(admin, third.ID).Created.Profile.Slug; got != "sample-roofing-repairs-3" {
		t.Fatalf("third slug = %q", got)
	}
	// A draft, and an application that is not there, cannot be approved.
	e.selfRegister("draft@example.com", "HOST")
	drafts := decode[adminListBody](t, e.get("/v1/admin/onboarding?status=DRAFT&role=HOST", withCookie(admin)))
	wantError(t, e.post("/v1/admin/onboarding/"+drafts.Items[0].ID+"/approve", obj{}, browser(admin)), http.StatusConflict, onboarding.CodeInvalidState)
	wantError(t, e.post("/v1/admin/onboarding/"+uuid.NewString()+"/approve", obj{}, browser(admin)), http.StatusNotFound, onboarding.CodeNotFound)
}

func TestOnboardingApprovalPerOutcome(t *testing.T) {
	e := newEnv(t)
	admin := e.admin()
	cases := []struct {
		role, orgKind, profileKind, membership, orgName string
	}{
		{"AGENCY_OWNER", "AGENCY", "agencies", "OWNER", "Kopje Realty"},
		{"LANDLORD", "LANDLORD", "property-managers", "OWNER", "Kopje Lettings"},
		{"HOST", "HOST", "hosts", "OWNER", "Kopje Stays"},
		{"DEVELOPER", "DEVELOPER", "developers", "OWNER", "Kopje Developments"},
		{"SERVICE_PROVIDER", "SERVICE_PROVIDER", "service-providers", "OWNER", "Kopje Plumbing"},
		{"AGENT", "", "agents", "AGENT", "Sample Estates"},
		{"AGENCY_STAFF", "", "", "STAFF", "Sample Estates"},
		{"AGENCY_ADMIN", "", "", "MANAGER", "Sample Estates"},
		{"PRIVATE_SELLER", "", "", "", ""},
	}
	profiles := decode[struct {
		Profiles []any `json:"profiles"`
	}](t, e.get("/v1/sitemap")).Profiles
	for i, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			c, app := e.apply(fmt.Sprintf("applicant%d@example.com", i), tc.role, tc.orgName)
			out := e.approve(admin, app.ID)
			created := out.Created
			if (created.OrganisationID != nil) != (tc.orgKind != "") {
				t.Fatalf("organisation created = %v", created.OrganisationID != nil)
			}
			if (created.Profile != nil) != (tc.profileKind != "") || (created.Profile != nil && created.Profile.Kind != tc.profileKind) {
				t.Fatalf("profile = %+v, want kind %q", created.Profile, tc.profileKind)
			}
			if (created.MembershipRole != nil) != (tc.membership != "") || (created.MembershipRole != nil && *created.MembershipRole != tc.membership) {
				t.Fatalf("membership = %v, want %q", created.MembershipRole, tc.membership)
			}
			user, raw := e.me(c)
			if status, ob := accountRole(user, raw, tc.role); status != "ACTIVE" || ob != "APPROVED" {
				t.Fatalf("role = %s / %s", status, ob)
			}
			switch tc.membership {
			case "":
				if len(user.Memberships) != 0 {
					t.Fatalf("memberships = %+v", user.Memberships)
				}
			default:
				if len(user.Memberships) != 1 || user.Memberships[0].Role != tc.membership || user.Memberships[0].OrganisationName != tc.orgName ||
					(tc.orgKind != "" && user.Memberships[0].OrganisationKind != tc.orgKind) || (tc.orgKind == "" && user.Memberships[0].OrganisationKind != "AGENCY") {
					t.Fatalf("memberships = %+v", user.Memberships)
				}
			}
			// The inbox opens for everyone approved (members and private sellers).
			wantStatus(t, e.get("/v1/agent/property-requests", withCookie(c)), http.StatusOK)
			if created.Profile != nil {
				rec := e.get("/v1/directory/" + created.Profile.Kind + "/" + created.Profile.Slug)
				wantStatus(t, rec, http.StatusOK)
				if !strings.Contains(rec.Body.String(), `"verified":true`) || strings.Contains(rec.Body.String(), "+263") || strings.Contains(rec.Body.String(), "REG-12345") {
					t.Fatalf("profile = %s", rec.Body)
				}
				wantStatus(t, e.post("/v1/business-enquiries", e.enquiry(created.Profile.Kind, created.Profile.Slug)), http.StatusCreated)
			}
			if tc.role == "AGENT" {
				// The agent profile is linked to the agency and shows on it.
				agent := e.get("/v1/directory/agents/" + created.Profile.Slug).Body.String()
				if !strings.Contains(agent, `"agency":{"kind":"agencies","slug":"sample-estates"`) || !strings.Contains(agent, `"name":"Rudo Ncube"`) || !strings.Contains(agent, `"role":"Sales agent"`) {
					t.Fatalf("agent profile = %s", agent)
				}
				if agency := e.get("/v1/directory/agencies/sample-estates").Body.String(); !strings.Contains(agency, `"slug":"`+created.Profile.Slug+`"`) {
					t.Fatal("the new agent is not listed on the agency")
				}
			}
		})
	}
	after := decode[struct {
		Profiles []any `json:"profiles"`
	}](t, e.get("/v1/sitemap")).Profiles
	if len(after) != len(profiles)+6 {
		t.Fatalf("%d profiles were created, want 6 (five businesses and one agent)", len(after)-len(profiles))
	}
	if ev := e.onboardingEvents(); ev[onboarding.EventApproved] != len(cases) || ev[onboarding.EventSubmitted] != len(cases) {
		t.Fatalf("events = %v", ev)
	}
}

func TestOnboardingAgencyMustStillBeVerified(t *testing.T) {
	e := newEnv(t)
	admin := e.admin()
	c, app := e.apply("agent@example.com", "AGENT", "")
	// The agency loses its verification after the application was submitted.
	agency, err := e.directory.Get(t.Context(), directory.KindAgencies, "sample-estates")
	if err != nil {
		t.Fatal(err)
	}
	agency.Verified = false
	e.directory.Put(agency)
	wantError(t, e.get("/v1/directory/agencies/sample-estates"), http.StatusNotFound, directory.CodeProfileNotFound) // unverified = not public

	wantError(t, e.post("/v1/admin/onboarding/"+app.ID+"/approve", obj{}, browser(admin)), http.StatusConflict, onboarding.CodeAgencyNotVerified)
	user, raw := e.me(c)
	if status, ob := accountRole(user, raw, "AGENT"); status != "PENDING_VERIFICATION" || ob != "SUBMITTED" || len(user.Memberships) != 0 {
		t.Fatalf("a refused approval changed something: %s / %s / %+v", status, ob, user.Memberships)
	}
	if ev := e.onboardingEvents(); ev[onboarding.EventApproved] != 0 {
		t.Fatalf("events = %v", ev)
	}
	// It can still be sent back or rejected.
	wantStatus(t, e.post("/v1/admin/onboarding/"+app.ID+"/request-changes", obj{"reason": "Please choose another agency."}, browser(admin)), http.StatusOK)
	// ...and the applicant cannot resubmit with that agency.
	env := wantError(t, e.post("/v1/me/onboarding/AGENT/submit", nil, browser(c)), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["details.agency_slug"] == "" {
		t.Fatalf("fields = %v", env.Error.Fields)
	}
}

func TestOnboardingRejectAndReapply(t *testing.T) {
	e := newEnv(t)
	admin := e.admin()
	c, app := e.apply("pro@example.com", "DEVELOPER", "Kopje Developments")
	profiles := len(e.get("/v1/sitemap").Body.String())

	// A reason is required, between 5 and 500 characters, plain text.
	for _, action := range []string{"reject", "request-changes"} {
		for _, body := range []obj{{}, {"reason": ""}, {"reason": "no"}, {"reason": strings.Repeat("x", 501)}, {"reason": "<b>Because</b> I say so"}} {
			env := wantError(t, e.post("/v1/admin/onboarding/"+app.ID+"/"+action, body, browser(admin)), http.StatusBadRequest, httpx.CodeValidationFailed)
			if env.Error.Fields["reason"] == "" {
				t.Fatalf("%s %v: fields = %v", action, body, env.Error.Fields)
			}
		}
	}

	rec := e.post("/v1/admin/onboarding/"+app.ID+"/reject", obj{"reason": "We could not confirm the registration."}, browser(admin))
	wantStatus(t, rec, http.StatusOK)
	if d := decode[struct {
		Application adminApplicationBody `json:"application"`
	}](t, rec).Application; d.Status != "REJECTED" || len(d.History) != 2 || d.History[1].To != "REJECTED" {
		t.Fatalf("rejected = %+v", d)
	}
	// The role is REJECTED and nothing was created.
	user, raw := e.me(c)
	if status, ob := accountRole(user, raw, "DEVELOPER"); status != "REJECTED" || ob != "REJECTED" || len(user.Memberships) != 0 {
		t.Fatalf("after rejection: %s / %s / %+v", status, ob, user.Memberships)
	}
	if len(e.get("/v1/sitemap").Body.String()) != profiles {
		t.Fatal("a rejection created a profile")
	}
	wantError(t, e.get("/v1/inbox/business-enquiries", withCookie(c)), http.StatusForbidden, httpx.CodeForbidden)
	mine := e.myOnboarding(c)[0]
	if mine.Status != "REJECTED" || mine.CanEdit || mine.CanSubmit || !mine.CanReapply || mine.Decision.Reason == nil || *mine.Decision.Reason != "We could not confirm the registration." {
		t.Fatalf("applicant view = %+v", mine)
	}
	// REJECTED is final for that application.
	for action, body := range map[string]obj{"approve": {"note": "Another go."}, "reject": {"reason": "Another go."}, "request-changes": {"reason": "Another go."}} {
		wantError(t, e.post("/v1/admin/onboarding/"+app.ID+"/"+action, body, browser(admin)), http.StatusConflict, onboarding.CodeInvalidState)
	}
	wantError(t, e.post("/v1/me/onboarding/DEVELOPER/submit", nil, browser(c)), http.StatusConflict, onboarding.CodeInvalidState)
	wantError(t, e.upload(c, "DEVELOPER", "PROOF_OF_IDENTITY", "id.pdf", testPDF()), http.StatusConflict, onboarding.CodeInvalidState)
	if ev := e.onboardingEvents(); ev[onboarding.EventRejected] != 1 || ev[onboarding.EventSubmitted] != 1 || len(ev) != 2 {
		t.Fatalf("events = %v", ev)
	}

	// Saving details again starts a NEW application; the old one is kept.
	rec = e.put("/v1/me/onboarding/DEVELOPER", obj{"business_name": "Kopje Developments (Pvt) Ltd"}, browser(c))
	wantStatus(t, rec, http.StatusOK)
	fresh := decode[oneApplication](t, rec).Application
	if fresh.ID == app.ID || fresh.Status != "DRAFT" || !fresh.CanEdit || fresh.CanReapply || len(fresh.Documents) != 0 ||
		fresh.Details["business_name"] != "Kopje Developments (Pvt) Ltd" || fresh.Details["city"] != nil || fresh.Decision.Reason != nil {
		t.Fatalf("new application = %+v", fresh)
	}
	if apps := e.myOnboarding(c); len(apps) != 1 || apps[0].ID != fresh.ID {
		t.Fatalf("current applications = %+v", apps)
	}
	user, raw = e.me(c)
	if status, ob := accountRole(user, raw, "DEVELOPER"); status != "PENDING_VERIFICATION" || ob != "DRAFT" {
		t.Fatalf("after reapplying: %s / %s", status, ob)
	}
	old := decode[struct {
		Application adminApplicationBody `json:"application"`
	}](t, e.get("/v1/admin/onboarding/"+app.ID, withCookie(admin))).Application
	if old.Status != "REJECTED" || len(old.History) != 2 || len(old.Documents) != len(requiredDocuments("DEVELOPER")) {
		t.Fatalf("the rejected application was not kept: %+v", old)
	}
	if got := decode[adminListBody](t, e.get("/v1/admin/onboarding?status=REJECTED", withCookie(admin))); got.Total != 1 || got.Counts["DRAFT"] != 2 {
		t.Fatalf("list after reapplying = %+v", got) // the new draft and the seeded agent's
	}
	// The new one goes through the whole flow.
	wantStatus(t, e.put("/v1/me/onboarding/DEVELOPER", detailsFor("DEVELOPER", "Kopje Developments"), browser(c)), http.StatusOK)
	for _, doc := range requiredDocuments("DEVELOPER") {
		wantStatus(t, e.upload(c, "DEVELOPER", doc, "scan.pdf", testPDF()), http.StatusCreated)
	}
	wantStatus(t, e.post("/v1/me/onboarding/DEVELOPER/submit", nil, browser(c)), http.StatusOK)
	if got := e.approve(admin, fresh.ID).Created.Profile; got == nil || got.Slug != "kopje-developments" {
		t.Fatalf("approval after reapplying = %+v", got)
	}
}

// A profile that is hidden or not verified is never public, however it got there.
func TestOnlyVerifiedPublishedProfilesArePublic(t *testing.T) {
	e := newEnv(t)
	before := e.get("/v1/directory").Body.String()
	for i, r := range []directory.Record{
		{Kind: "hosts", Slug: "unverified-host", Name: "Unverified Host", Status: directory.StatusActive, Verified: false},
		{Kind: "hosts", Slug: "pending-host", Name: "Pending Host", Status: "PENDING_VERIFICATION", Verified: true},
		{Kind: "hosts", Slug: "suspended-host", Name: "Suspended Host", Status: "SUSPENDED", Verified: true},
	} {
		r.ID, r.OrganisationID, r.CityCode, r.UpdatedAt = uuid.New(), devseed.Organisations()[i].ID, "harare", e.now
		e.directory.Put(r)
		wantError(t, e.get("/v1/directory/hosts/"+r.Slug), http.StatusNotFound, directory.CodeProfileNotFound)
		for _, path := range []string{"/v1/directory/hosts?page_size=48", "/v1/directory/hosts?q=host", "/v1/sitemap"} {
			if strings.Contains(e.get(path).Body.String(), r.Slug) {
				t.Fatalf("%s lists %s", path, r.Slug)
			}
		}
	}
	if after := e.get("/v1/directory").Body.String(); after != before {
		t.Fatalf("hidden profiles are counted:\n%s\n%s", before, after)
	}
	// Every seeded sample profile is verified and public.
	for _, p := range devseed.Sample().Profiles {
		if rec := e.get("/v1/directory/" + p.Kind + "/" + p.Slug); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"verified":true`) {
			t.Fatalf("%s/%s = %d", p.Kind, p.Slug, rec.Code)
		}
	}
}
