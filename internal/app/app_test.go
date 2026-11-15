package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/availability"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/health"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/teams"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/storage"
)

const (
	testOrigin = "http://localhost:3000"
	// Shared password of the seeded test users (test-only value).
	seedPassword = "seed-password-for-tests"
	newPassword  = "a-brand-new-password"
)

// captureMailer records the links that would have been emailed.
type captureMailer struct {
	mu            sync.Mutex
	verifications []string
	resets        []string
	accountExists []string
	// signInCodes are the sign-in codes that would have been emailed.
	signInCodes []sentCode
	// invitations are the team invitations that would have been emailed.
	invitations []sentInvitation
	// signings, agreementCodes, completions and declines are the agreement emails.
	signings       []sentSigning
	agreementCodes []sentCode
	completions    []sentSigning
	declines       []string
}

// sentSigning is one agreement email with a link.
type sentSigning struct {
	to       string
	token    string
	reminder bool
}

func (m *captureMailer) SendSigning(_ context.Context, to string, s agreements.SigningMail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signings = append(m.signings, sentSigning{to: to, token: s.Token, reminder: s.Reminder})
	return nil
}

func (m *captureMailer) SendCode(_ context.Context, to string, c agreements.CodeMail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agreementCodes = append(m.agreementCodes, sentCode{to: to, code: c.Code})
	return nil
}

func (m *captureMailer) SendCompleted(_ context.Context, to string, c agreements.CompletedMail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completions = append(m.completions, sentSigning{to: to, token: c.Token})
	return nil
}

func (m *captureMailer) SendDeclined(_ context.Context, to string, _ agreements.DeclinedMail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.declines = append(m.declines, to)
	return nil
}

// signingFor returns the newest signing link emailed to an address ("" when none).
func (m *captureMailer) signingFor(to string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.signings) - 1; i >= 0; i-- {
		if m.signings[i].to == to {
			return m.signings[i].token
		}
	}
	return ""
}

// codeFor returns the newest agreement code emailed to an address.
func (m *captureMailer) codeFor(to string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.agreementCodes) - 1; i >= 0; i-- {
		if m.agreementCodes[i].to == to {
			return m.agreementCodes[i].code
		}
	}
	return ""
}

// sentInvitation is one invitation email.
type sentInvitation struct {
	to   string
	mail teams.InvitationMail
}

func (m *captureMailer) SendInvitation(_ context.Context, to string, inv teams.InvitationMail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invitations = append(m.invitations, sentInvitation{to: to, mail: inv})
	return nil
}

// lastInvitation returns the most recent invitation email (its token is the
// link's secret, as a person reads it from their inbox).
func (m *captureMailer) lastInvitation(t *testing.T) sentInvitation {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.invitations) == 0 {
		t.Fatal("no invitation was sent")
	}
	return m.invitations[len(m.invitations)-1]
}

// sentCode is one sign-in code email.
type sentCode struct {
	to, code string
	admin    bool
}

func (m *captureMailer) SendSignInCode(_ context.Context, to, _, code string, admin bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signInCodes = append(m.signInCodes, sentCode{to: to, code: code, admin: admin})
	return nil
}

// lastCode returns the most recent sign-in code (the test seam: a test reads
// the code from the captured email, as a person reads it from their inbox).
func (m *captureMailer) lastCode(t *testing.T) sentCode {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.signInCodes) == 0 {
		t.Fatal("no sign-in code was sent")
	}
	return m.signInCodes[len(m.signInCodes)-1]
}

func (m *captureMailer) codeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.signInCodes)
}

// The mailer receives the raw token and builds the link itself; this fake
// builds the same link shape so the tests can follow it.
func (m *captureMailer) SendVerification(_ context.Context, _, _, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.verifications = append(m.verifications, testOrigin+"/verify-email?token="+url.QueryEscape(token))
	return nil
}

func (m *captureMailer) SendPasswordReset(_ context.Context, _, _, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resets = append(m.resets, testOrigin+"/reset-password?token="+url.QueryEscape(token))
	return nil
}

func (m *captureMailer) SendAccountExists(_ context.Context, to, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accountExists = append(m.accountExists, to)
	return nil
}

func (m *captureMailer) counts() (verifications, resets, exists int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.verifications), len(m.resets), len(m.accountExists)
}

// lastToken extracts the token from the most recent link of the given kind.
func (m *captureMailer) lastToken(t *testing.T, links *[]string, wantPath string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(*links) == 0 {
		t.Fatalf("no %s email was sent", wantPath)
	}
	link := (*links)[len(*links)-1]
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("bad link %q: %v", link, err)
	}
	if u.Scheme+"://"+u.Host != testOrigin || u.Path != wantPath || u.Query().Get("token") == "" {
		t.Fatalf("link = %q, want %s%s?token=...", link, testOrigin, wantPath)
	}
	return u.Query().Get("token")
}

func (m *captureMailer) verifyToken(t *testing.T) string {
	return m.lastToken(t, &m.verifications, "/verify-email")
}
func (m *captureMailer) resetToken(t *testing.T) string {
	return m.lastToken(t, &m.resets, "/reset-password")
}

type env struct {
	t         *testing.T
	handler   http.Handler
	identity  *identity.MemoryStore
	requests  *propertyrequests.MemoryStore
	enquiries *businessenquiries.MemoryStore
	reference *reference.MemoryStore
	listings  *listings.MemoryStore
	directory *directory.MemoryStore
	onboard   *onboarding.MemoryStore
	// marketStore holds the countries (all enabled unless a test says otherwise).
	marketStore *markets.MemoryStore
	// calendars holds the availability of the short stays (empty unless a test fills it).
	calendars *availability.MemoryStore
	appts     *appointments.MemoryStore
	team      *teams.MemoryStore
	leases    *leases.MemoryStore
	agree     *agreements.MemoryStore
	files     *storage.Local
	mail      *captureMailer
	logs      *bytes.Buffer
	now       time.Time
	seq       int
	// captchaSeq numbers the registration questions the tests ask for.
	captchaSeq int
	// twoStepLogin turns off the helper that completes the sign-in code step
	// (see do): the tests of the code step itself see the raw answers.
	twoStepLogin bool
}

func newEnv(t *testing.T) *env {
	return newEnvWithConfig(t, nil)
}

// newEnvWithConfig builds the full router on memory stores with the
// development seed users. Argon2 runs with minimal parameters to keep tests fast.
func newEnvWithConfig(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	cfg, err := config.FromLookup(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(&cfg)
	}
	e := &env{
		t:         t,
		identity:  identity.NewMemoryStore(),
		requests:  propertyrequests.NewMemoryStore(),
		enquiries: businessenquiries.NewMemoryStore(),
		reference: devseed.NewMemoryReferenceStore(),
		mail:      &captureMailer{},
		logs:      &bytes.Buffer{},
		now:       time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC),
	}
	hasher := identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1}
	e.listings = listings.NewMemoryStore(reference.NewService(e.reference, nil))
	e.directory = directory.NewMemoryStore()
	devseed.SeedMemoryContent(e.listings, e.directory, e.now)
	devseed.SeedMemoryListings(e.requests)
	devseed.SeedMemoryProfiles(e.enquiries)
	// Appointments start empty in tests (the sample ones are seeded by the
	// tests that want them); a request and its appointment are stored together.
	e.appts = appointments.NewMemoryStore()
	e.requests.SetAppointmentWriter(e.appts.CreateLinked)
	if err := devseed.SeedMemoryUsers(context.Background(), e.identity, hasher, seedPassword, e.now); err != nil {
		t.Fatal(err)
	}
	// Onboarding: its own store, and document files in a per-test directory.
	e.onboard = onboarding.NewMemoryStore(onboarding.MemoryDeps{
		Identity: e.identity, Directory: e.directory, Enquiries: e.enquiries,
		MarketID: func(code string) (uuid.UUID, bool) {
			id := markets.IDFor(code)
			return id, id != uuid.Nil
		},
	})
	if e.files, err = storage.NewLocal(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := devseed.SeedMemoryOnboarding(context.Background(), e.identity, e.onboard, e.files, hasher, seedPassword, e.now); err != nil {
		t.Fatal(err)
	}
	// As cmd/api: the sample logos' images, and advertisers resolved on every read.
	if err := devseed.SeedLogoFiles(context.Background(), e.files); err != nil {
		t.Fatal(err)
	}
	e.listings.SetAdvertisers(MemoryAdvertisers(e.directory, e.identity))
	e.team = teams.NewMemoryStore(teams.MemoryDeps{
		Identity: e.identity, Directory: e.directory, Enquiries: e.enquiries, Requests: e.requests, Appointments: e.appts,
		Onboarding: e.onboard,
		MarketID: func(code string) (uuid.UUID, bool) {
			id := markets.IDFor(code)
			return id, id != uuid.Nil
		},
	})
	e.leases = leases.NewMemoryStore()
	if err := devseed.SeedMemoryLeases(context.Background(), e.leases, e.files, e.now); err != nil {
		t.Fatal(err)
	}
	e.agree = agreements.NewMemoryStore()
	if err := devseed.SeedMemoryAgreements(context.Background(), e.agree, e.leases, e.files, e.now); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	e.marketStore = markets.NewMemoryStore()
	e.calendars = availability.NewMemoryStore()
	e.handler, err = NewRouter(Deps{
		Config:    cfg,
		Logger:    logging.New(e.logs, slog.LevelInfo, "api", cfg.Env),
		Health:    health.NewHandler(health.StoreMemory, nil, logger),
		Markets:   e.marketStore,
		Requests:  e.requests,
		Enquiries: e.enquiries,
		Reference: e.reference,
		Listings:  e.listings, ListingSearch: e.listings, Directory: e.directory,
		Identity:   e.identity,
		Mailer:     e.mail,
		Onboarding: e.onboard, Files: e.files,
		Appointments: e.appts,
		Availability: e.calendars,
		Teams:        e.team,
		Leases:       e.leases,
		Agreements:   e.agree,
		Now:          func() time.Time { return e.now },
		Hasher:       &hasher,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type reqOpt func(*http.Request)

func withCookie(c *http.Cookie) reqOpt { return func(r *http.Request) { r.AddCookie(c) } }
func withBearer(token string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}
func withOrigin(o string) reqOpt { return func(r *http.Request) { r.Header.Set("Origin", o) } }
func withHeader(k, v string) reqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}
func withIP(ip string) reqOpt { return func(r *http.Request) { r.RemoteAddr = ip + ":40000" } }

// browser simulates the web app: the session cookie plus its Origin header.
func browser(c *http.Cookie) reqOpt {
	return func(r *http.Request) {
		r.AddCookie(c)
		r.Header.Set("Origin", testOrigin)
	}
}

// captchaBody is the body of GET /v1/auth/captcha.
type captchaBody struct {
	CaptchaID string    `json:"captcha_id"`
	Question  string    `json:"question"`
	ExpiresAt time.Time `json:"expires_at"`
}

// newCaptcha asks the API for a registration question, the way a client does.
// Each call comes from its own address so that the helper never runs into the
// per-IP limit of the endpoint.
func (e *env) newCaptcha() captchaBody {
	e.t.Helper()
	e.captchaSeq++
	ip := fmt.Sprintf("10.77.%d.%d", e.captchaSeq/250, e.captchaSeq%250+1)
	rec := e.do(http.MethodGet, "/v1/auth/captcha", nil, withIP(ip))
	wantStatus(e.t, rec, http.StatusOK)
	return decode[captchaBody](e.t, rec)
}

// solveCaptcha works the answer out from the question, as a person would.
func solveCaptcha(t *testing.T, question string) int {
	t.Helper()
	var a, b int
	var op string
	if _, err := fmt.Sscanf(question, "What is %d %s %d?", &a, &op, &b); err != nil {
		t.Fatalf("unexpected captcha question %q: %v", question, err)
	}
	switch op {
	case "+":
		return a + b
	case "-":
		return a - b
	}
	t.Fatalf("unexpected captcha question %q", question)
	return 0
}

// solvedCaptcha returns the "captcha" object of a registration: a fresh
// question with its right answer.
func (e *env) solvedCaptcha() obj {
	e.t.Helper()
	c := e.newCaptcha()
	return obj{"captcha_id": c.CaptchaID, "answer": fmt.Sprint(solveCaptcha(e.t, c.Question))}
}

// withCaptcha gives a registration body a solved captcha unless the test set
// the "captcha" key itself (nil stands for a registration without one). The
// tests that are not about the captcha therefore register exactly as a client
// does: they ask for a question and answer it. The body is copied, not changed.
func (e *env) withCaptcha(body any) any {
	e.t.Helper()
	in, ok := body.(obj)
	if !ok {
		return body
	}
	if _, set := in["captcha"]; set {
		return body
	}
	out := make(obj, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	out["captcha"] = e.solvedCaptcha()
	return out
}

func (e *env) do(method, path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	if method == http.MethodPost && path == "/v1/auth/register" {
		body = e.withCaptcha(body)
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if !e.twoStepLogin && method == http.MethodPost && (path == "/v1/auth/login" || path == "/v1/admin/auth/login") {
		return e.completeCode(path, rec, opts...)
	}
	return rec
}

// completeCode finishes a sign-in whose password was right, the way a person
// does: it reads the code from the captured email and sends it to the verify
// endpoint from the same client. The tests that are not about the code step
// therefore sign in through both steps and see the session answer.
func (e *env) completeCode(path string, rec *httptest.ResponseRecorder, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	if rec.Code != http.StatusOK {
		return rec
	}
	var c codeChallengeBody
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil || c.Status != "CODE_REQUIRED" || c.ChallengeID == "" {
		e.t.Fatalf("login answered 200 without a code challenge: %s", rec.Body)
	}
	if sessionCookie(rec) != nil || strings.Contains(rec.Body.String(), "access_token") {
		e.t.Fatalf("the password step must not start a session: %s", rec.Body)
	}
	code := e.mail.lastCode(e.t)
	raw, _ := json.Marshal(obj{"challenge_id": c.ChallengeID, "code": code.code})
	req := httptest.NewRequest(http.MethodPost, path+"/verify", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for _, opt := range opts {
		opt(req)
	}
	out := httptest.NewRecorder()
	e.handler.ServeHTTP(out, req)
	return out
}

// codeChallengeBody is the answer of the password step.
type codeChallengeBody struct {
	Status      string    `json:"status"`
	ChallengeID string    `json:"challenge_id"`
	SentTo      string    `json:"sent_to"`
	ExpiresAt   time.Time `json:"expires_at"`
	CodeLength  int       `json:"code_length"`
}

func (e *env) post(path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	return e.do(http.MethodPost, path, body, opts...)
}

func (e *env) get(path string, opts ...reqOpt) *httptest.ResponseRecorder {
	return e.do(http.MethodGet, path, nil, opts...)
}

type obj = map[string]any

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, want, rec.Body)
	}
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) httpx.ErrorEnvelope {
	t.Helper()
	wantStatus(t, rec, status)
	var env httpx.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
	if env.Error.Code != code {
		t.Fatalf("error code = %q, want %q", env.Error.Code, code)
	}
	if env.TraceID == "" || env.TraceID != rec.Header().Get(httpx.HeaderRequestID) {
		t.Fatalf("trace_id %q does not match X-Request-Id %q", env.TraceID, rec.Header().Get(httpx.HeaderRequestID))
	}
	return env
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
	return v
}

// sessionCookie returns the rp_session cookie set by a response, or nil.
func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	return nil
}

type userBody struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	PlatformRoles []struct {
		Role       string  `json:"role"`
		MarketCode *string `json:"market_code"`
	} `json:"platform_roles"`
	Memberships []struct {
		OrganisationID   string `json:"organisation_id"`
		OrganisationName string `json:"organisation_name"`
		OrganisationKind string `json:"organisation_kind"`
		MarketCode       string `json:"market_code"`
		Role             string `json:"role"`
	} `json:"memberships"`
	AccountRoles []accountRoleBody `json:"account_roles"`
}

type accountRoleBody struct {
	Role   string `json:"role"`
	Status string `json:"status"`
}

type authBody struct {
	User    userBody `json:"user"`
	Session *struct {
		Scope     string    `json:"scope"`
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"session"`
	AccessToken string `json:"access_token"`
}

type inboxBody struct {
	Items []struct {
		RequestID string `json:"request_id"`
		Reference string `json:"reference"`
		ListingID string `json:"listing_id"`
		Listing   *struct {
			Slug      string `json:"slug"`
			Title     string `json:"title"`
			Reference string `json:"reference"`
		} `json:"listing"`
		RequestType            string    `json:"request_type"`
		Status                 string    `json:"status"`
		Message                string    `json:"message"`
		PreferredContactMethod string    `json:"preferred_contact_method"`
		CreatedAt              time.Time `json:"created_at"`
		Contact                struct {
			Name  string  `json:"name"`
			Email string  `json:"email"`
			Phone *string `json:"phone"`
		} `json:"contact"`
	} `json:"items"`
}

// loginCookie signs a user in the way the web app does.
func (e *env) loginCookie(email, password string) *http.Cookie {
	e.t.Helper()
	rec := e.post("/v1/auth/login", obj{"email": email, "password": password})
	wantStatus(e.t, rec, http.StatusOK)
	c := sessionCookie(rec)
	if c == nil {
		e.t.Fatal("login did not set the session cookie")
	}
	return c
}

// adminLogin signs a platform administrator in at the administrator endpoint.
func (e *env) adminLogin(email, password string, opts ...reqOpt) *http.Cookie {
	e.t.Helper()
	rec := e.post("/v1/admin/auth/login", obj{"email": email, "password": password}, opts...)
	wantStatus(e.t, rec, http.StatusOK)
	c := sessionCookie(rec)
	if c == nil {
		e.t.Fatal("admin login did not set the session cookie")
	}
	return c
}

// loginBearer signs a user in the way the mobile app does.
func (e *env) loginBearer(email, password string) string {
	e.t.Helper()
	rec := e.post("/v1/auth/login", obj{"email": email, "password": password, "token_delivery": "bearer"})
	wantStatus(e.t, rec, http.StatusOK)
	return decode[authBody](e.t, rec).AccessToken
}

func registerBody(email string) obj {
	return obj{
		"name": "Rudo Ncube", "email": email, "password": "correct-horse-battery",
		"consent": obj{"privacy_version": "2026-01", "accepted": true},
		"roles":   []string{"CONSUMER"},
	}
}

// propertyRequest returns a fresh, valid guest request for seeded listing n.
func (e *env) propertyRequest(n int) obj {
	e.seq++
	return obj{
		"listing_id":               propertyrequests.SeedListingID(n).String(),
		"request_type":             "ENQUIRY",
		"message":                  fmt.Sprintf("Enquiry %d about listing %d", e.seq, n),
		"preferred_contact_method": "EMAIL",
		"idempotency_key":          fmt.Sprintf("test-key-%04d", e.seq),
		"guest_contact":            obj{"name": "Tariro Moyo", "email": "tariro@example.com", "phone": "+263771234567"},
		"consent":                  obj{"privacy_version": "2026-01", "accepted": true},
	}
}

// ---------------------------------------------------------------------------

func TestRegisterVerifyMe(t *testing.T) {
	e := newEnv(t)
	before := e.identity.UserCount()

	rec := e.post("/v1/auth/register", registerBody("  Rudo@Example.com "))
	wantStatus(t, rec, http.StatusAccepted)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"VERIFICATION_SENT"}` {
		t.Fatalf("body = %s", got)
	}
	if sessionCookie(rec) != nil {
		t.Fatal("register must not sign the user in")
	}
	if e.identity.UserCount() != before+1 {
		t.Fatal("user was not created")
	}
	if c := e.identity.Consents(); c[len(c)-1].Version != "2026-01" || c[len(c)-1].ConsentType != identity.ConsentPrivacyNotice {
		t.Fatalf("registration consent not recorded: %+v", c[len(c)-1])
	}

	// Correct password but not yet verified.
	wantError(t, e.post("/v1/auth/login", obj{"email": "rudo@example.com", "password": "correct-horse-battery"}),
		http.StatusForbidden, identity.CodeEmailNotVerified)

	rec = e.post("/v1/auth/verify-email", obj{"token": e.mail.verifyToken(t)})
	wantStatus(t, rec, http.StatusOK)
	body := decode[authBody](t, rec)
	if body.User.Email != "rudo@example.com" || body.User.Name != "Rudo Ncube" || !body.User.EmailVerified {
		t.Fatalf("unexpected user: %+v", body.User)
	}
	if body.AccessToken != "" {
		t.Fatal("cookie delivery must not return access_token")
	}
	if want := e.now.Add(30 * 24 * time.Hour); !body.Session.ExpiresAt.Equal(want) {
		t.Fatalf("session.expires_at = %s, want %s", body.Session.ExpiresAt, want)
	}
	if !strings.Contains(rec.Body.String(), `"platform_roles":[]`) || !strings.Contains(rec.Body.String(), `"memberships":[]`) {
		t.Fatalf("role arrays must be empty arrays, not null: %s", rec.Body)
	}

	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("verify-email must start a cookie session")
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure || c.MaxAge != 30*24*3600 {
		t.Fatalf("unexpected cookie attributes: %+v", c)
	}

	rec = e.get("/v1/me", withCookie(c))
	wantStatus(t, rec, http.StatusOK)
	me := decode[struct {
		User userBody `json:"user"`
	}](t, rec)
	if me.User.ID != body.User.ID || me.User.Email != "rudo@example.com" || !me.User.EmailVerified {
		t.Fatalf("unexpected /v1/me: %+v", me.User)
	}
	if !strings.Contains(e.logs.String(), `"user_id":"`+body.User.ID+`"`) {
		t.Fatal("access log of an authenticated request must carry user_id")
	}
	if strings.Contains(e.logs.String(), c.Value) || strings.Contains(e.logs.String(), "correct-horse-battery") {
		t.Fatal("logs must not contain session tokens or passwords")
	}
}

func TestSecureCookieOutsideDevelopment(t *testing.T) {
	e := newEnvWithConfig(t, func(c *config.Config) { c.Env = config.EnvProduction })
	if c := e.loginCookie(identity.SeedConsumerEmail, seedPassword); !c.Secure || !c.HttpOnly {
		t.Fatalf("cookie must be Secure and HttpOnly outside development: %+v", c)
	}
}

func TestRegisterExistingEmail(t *testing.T) {
	t.Run("verified account: 202, no second user, nothing changes", func(t *testing.T) {
		e := newEnv(t)
		before := e.identity.UserCount()
		body := registerBody(strings.ToUpper(identity.SeedConsumerEmail))

		rec := e.post("/v1/auth/register", body)
		wantStatus(t, rec, http.StatusAccepted)
		if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"VERIFICATION_SENT"}` {
			t.Fatalf("body = %s", got)
		}
		if e.identity.UserCount() != before {
			t.Fatal("a second user was created for an existing email")
		}
		if v, _, exists := e.mail.counts(); v != 0 || exists != 1 {
			t.Fatalf("verifications=%d accountExists=%d, want 0 and 1", v, exists)
		}
		// The stranger's password was not applied; the real one still works.
		wantError(t, e.post("/v1/auth/login", obj{"email": identity.SeedConsumerEmail, "password": "correct-horse-battery"}),
			http.StatusUnauthorized, identity.CodeInvalidCredentials)
		e.loginCookie(identity.SeedConsumerEmail, seedPassword)
	})

	t.Run("unverified account: 202, no second user, latest registration wins", func(t *testing.T) {
		e := newEnv(t)
		wantStatus(t, e.post("/v1/auth/register", registerBody("new@example.com")), http.StatusAccepted)
		firstToken := e.mail.verifyToken(t)
		count := e.identity.UserCount()

		second := registerBody("new@example.com")
		second["password"] = "second-registration-pw"
		wantStatus(t, e.post("/v1/auth/register", second), http.StatusAccepted)
		if e.identity.UserCount() != count {
			t.Fatal("a second user was created for an existing email")
		}

		wantError(t, e.post("/v1/auth/verify-email", obj{"token": firstToken}), http.StatusBadRequest, identity.CodeInvalidToken)
		wantStatus(t, e.post("/v1/auth/verify-email", obj{"token": e.mail.verifyToken(t)}), http.StatusOK)
		wantError(t, e.post("/v1/auth/login", obj{"email": "new@example.com", "password": "correct-horse-battery"}),
			http.StatusUnauthorized, identity.CodeInvalidCredentials)
		e.loginCookie("new@example.com", "second-registration-pw")
	})
}

func TestRegisterValidation(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(obj)
		wantField string
	}{
		{"missing name", func(b obj) { b["name"] = "  " }, "name"},
		{"html in name", func(b obj) { b["name"] = "<b>Rudo</b>" }, "name"},
		{"missing email", func(b obj) { delete(b, "email") }, "email"},
		{"bad email", func(b obj) { b["email"] = "not-an-email" }, "email"},
		{"missing password", func(b obj) { delete(b, "password") }, "password"},
		{"short password", func(b obj) { b["password"] = "123456789" }, "password"},
		{"long password", func(b obj) { b["password"] = strings.Repeat("x", 129) }, "password"},
		{"consent missing", func(b obj) { delete(b, "consent") }, "consent.accepted"},
		{"consent not accepted", func(b obj) { b["consent"] = obj{"privacy_version": "2026-01", "accepted": false} }, "consent.accepted"},
		{"privacy version missing", func(b obj) { b["consent"] = obj{"accepted": true} }, "consent.privacy_version"},
		{"roles missing", func(b obj) { delete(b, "roles") }, "roles"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			before := e.identity.UserCount()
			body := registerBody("valid@example.com")
			tt.mutate(body)
			env := wantError(t, e.post("/v1/auth/register", body), http.StatusBadRequest, httpx.CodeValidationFailed)
			if _, ok := env.Error.Fields[tt.wantField]; !ok || len(env.Error.Fields) != 1 {
				t.Fatalf("fields = %v, want only %q", env.Error.Fields, tt.wantField)
			}
			if e.identity.UserCount() != before {
				t.Fatal("invalid registration created a user")
			}
		})
	}

	t.Run("boundary lengths accepted", func(t *testing.T) {
		e := newEnv(t)
		for i, pw := range []string{strings.Repeat("a", 10), strings.Repeat("b", 128)} {
			body := registerBody(fmt.Sprintf("edge%d@example.com", i))
			body["password"] = pw
			body["marketing_opt_in"] = true
			wantStatus(t, e.post("/v1/auth/register", body), http.StatusAccepted)
		}
	})

	t.Run("strict JSON and content type still enforced", func(t *testing.T) {
		e := newEnv(t)
		body := registerBody("strict@example.com")
		body["role"] = "SUPER_ADMIN"
		wantError(t, e.post("/v1/auth/register", body), http.StatusBadRequest, httpx.CodeInvalidJSON)
		wantError(t, e.post("/v1/auth/login", obj{"email": "a@b.co", "password": "x"}, withHeader("Content-Type", "text/plain")),
			http.StatusUnsupportedMediaType, httpx.CodeUnsupportedMediaType)
	})
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	e := newEnv(t)
	const rid = "fixed-request-id"
	wrong := e.post("/v1/auth/login", obj{"email": identity.SeedConsumerEmail, "password": "definitely-wrong-pw"}, withHeader(httpx.HeaderRequestID, rid))
	unknown := e.post("/v1/auth/login", obj{"email": "nobody@example.com", "password": "definitely-wrong-pw"}, withHeader(httpx.HeaderRequestID, rid))

	wantError(t, wrong, http.StatusUnauthorized, identity.CodeInvalidCredentials)
	wantError(t, unknown, http.StatusUnauthorized, identity.CodeInvalidCredentials)
	if wrong.Body.String() != unknown.Body.String() {
		t.Fatalf("bodies differ:\n%s\n%s", wrong.Body, unknown.Body)
	}
	if sessionCookie(wrong) != nil || sessionCookie(unknown) != nil {
		t.Fatal("failed login must not set a cookie")
	}
}

func TestLoginUnverified(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.post("/v1/auth/register", registerBody("pending@example.com")), http.StatusAccepted)

	rec := e.post("/v1/auth/login", obj{"email": "pending@example.com", "password": "correct-horse-battery"})
	wantError(t, rec, http.StatusForbidden, identity.CodeEmailNotVerified)
	if sessionCookie(rec) != nil {
		t.Fatal("unverified login must not set a cookie")
	}
	// A wrong password must not reveal that the account exists but is unverified.
	wantError(t, e.post("/v1/auth/login", obj{"email": "pending@example.com", "password": "wrong-password-1"}),
		http.StatusUnauthorized, identity.CodeInvalidCredentials)

	// Resend works for the pending account and is silent for everything else.
	v0, _, _ := e.mail.counts()
	for _, email := range []string{"pending@example.com", "nobody@example.com", identity.SeedConsumerEmail} {
		rec := e.post("/v1/auth/resend-verification", obj{"email": email})
		wantStatus(t, rec, http.StatusAccepted)
		if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"VERIFICATION_SENT"}` {
			t.Fatalf("body = %s", got)
		}
	}
	if v1, _, _ := e.mail.counts(); v1 != v0+1 {
		t.Fatalf("verification emails sent = %d, want exactly 1", v1-v0)
	}
	wantStatus(t, e.post("/v1/auth/verify-email", obj{"token": e.mail.verifyToken(t)}), http.StatusOK)
	e.loginCookie("pending@example.com", "correct-horse-battery")
}

func TestLoginValidation(t *testing.T) {
	e := newEnv(t)
	env := wantError(t, e.post("/v1/auth/login", obj{"email": identity.SeedConsumerEmail, "password": seedPassword, "token_delivery": "jwt"}),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	if _, ok := env.Error.Fields["token_delivery"]; !ok {
		t.Fatalf("fields = %v", env.Error.Fields)
	}
	env = wantError(t, e.post("/v1/auth/login", obj{}), http.StatusBadRequest, httpx.CodeValidationFailed)
	if len(env.Error.Fields) != 2 {
		t.Fatalf("fields = %v, want email and password", env.Error.Fields)
	}
}

func TestBearerDelivery(t *testing.T) {
	e := newEnv(t)
	rec := e.post("/v1/auth/login", obj{"email": identity.SeedAgentEmail, "password": seedPassword, "token_delivery": "bearer"})
	wantStatus(t, rec, http.StatusOK)
	if len(rec.Result().Cookies()) != 0 || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("bearer delivery must not set a cookie: %v", rec.Header()["Set-Cookie"])
	}
	body := decode[authBody](t, rec)
	if len(body.AccessToken) != 43 { // 32 random bytes, base64url without padding
		t.Fatalf("access_token = %q, want a 43-character opaque token", body.AccessToken)
	}
	if body.Session.ExpiresAt.IsZero() || body.User.Email != identity.SeedAgentEmail {
		t.Fatalf("unexpected body: %+v", body)
	}

	wantStatus(t, e.get("/v1/me", withBearer(body.AccessToken)), http.StatusOK)
	wantError(t, e.get("/v1/me", withBearer(body.AccessToken+"x")), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// Only the SHA-256 is stored.
	for _, s := range e.identity.Sessions() {
		if string(s.TokenHash) == body.AccessToken || len(s.TokenHash) != 32 {
			t.Fatal("sessions must be stored as SHA-256 digests")
		}
	}
}

func TestSeedUsersAndMeShape(t *testing.T) {
	e := newEnv(t)

	t.Run("anonymous", func(t *testing.T) {
		wantError(t, e.get("/v1/me"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		wantError(t, e.get("/v1/me", withBearer("not-a-token")), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		wantError(t, e.get("/v1/me", withCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "junk"})), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("consumer", func(t *testing.T) {
		rec := e.get("/v1/me", withCookie(e.loginCookie(identity.SeedConsumerEmail, seedPassword)))
		wantStatus(t, rec, http.StatusOK)
		want := `{"user":{"id":"` + identity.SeedConsumerUserID.String() + `","name":"Casey Consumer","email":"consumer@example.test","email_verified":true,"platform_roles":[],"memberships":[],"account_roles":[{"role":"CONSUMER","status":"ACTIVE","onboarding_status":null}]},"session":{"scope":"customer","expires_at":"2026-12-01T09:00:00Z"}}`
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Fatalf("body = %s\nwant   %s", got, want)
		}
	})

	t.Run("agent", func(t *testing.T) {
		rec := e.get("/v1/me", withCookie(e.loginCookie(identity.SeedAgentEmail, seedPassword)))
		wantStatus(t, rec, http.StatusOK)
		want := `"platform_roles":[],"memberships":[{"organisation_id":"` + propertyrequests.SeedOrganisationID.String() +
			`","organisation_name":"Sample Estates","organisation_kind":"AGENCY","market_code":"ZW","role":"AGENT","role_label":"Estate Agent"}],` +
			`"account_roles":[{"role":"AGENT","status":"ACTIVE","onboarding_status":null}]`
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body = %s\nwant to contain %s", rec.Body, want)
		}
	})

	t.Run("admin", func(t *testing.T) {
		rec := e.get("/v1/me", withCookie(e.adminLogin(identity.SeedAdminEmail, seedPassword)))
		wantStatus(t, rec, http.StatusOK)
		if want := `"platform_roles":[{"role":"SUPER_ADMIN","market_code":null}],"memberships":[],"account_roles":[]`; !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body = %s\nwant to contain %s", rec.Body, want)
		}
	})
}

func TestLogoutRevokes(t *testing.T) {
	e := newEnv(t)

	t.Run("cookie", func(t *testing.T) {
		c := e.loginCookie(identity.SeedConsumerEmail, seedPassword)
		wantStatus(t, e.get("/v1/me", withCookie(c)), http.StatusOK)

		rec := e.post("/v1/auth/logout", nil, browser(c))
		wantStatus(t, rec, http.StatusNoContent)
		if cleared := sessionCookie(rec); cleared == nil || cleared.MaxAge >= 0 || cleared.Value != "" {
			t.Fatalf("logout must clear the cookie, got %+v", cleared)
		}
		// The server-side session is gone even if the browser kept the cookie.
		wantError(t, e.get("/v1/me", withCookie(c)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("bearer", func(t *testing.T) {
		token := e.loginBearer(identity.SeedConsumerEmail, seedPassword)
		other := e.loginBearer(identity.SeedConsumerEmail, seedPassword)
		wantStatus(t, e.post("/v1/auth/logout", nil, withBearer(token)), http.StatusNoContent)
		wantError(t, e.get("/v1/me", withBearer(token)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		// Only the current session is revoked.
		wantStatus(t, e.get("/v1/me", withBearer(other)), http.StatusOK)
	})

	t.Run("not signed in", func(t *testing.T) {
		wantStatus(t, e.post("/v1/auth/logout", nil), http.StatusNoContent)
	})
}

func TestPasswordReset(t *testing.T) {
	e := newEnv(t)
	cookie := e.loginCookie(identity.SeedConsumerEmail, seedPassword)
	bearer := e.loginBearer(identity.SeedConsumerEmail, seedPassword)
	bystander := e.loginBearer(identity.SeedAgentEmail, seedPassword)

	// Same answer whether or not the account exists; only one email goes out.
	for _, email := range []string{identity.SeedConsumerEmail, "nobody@example.com"} {
		rec := e.post("/v1/auth/password-reset/request", obj{"email": email})
		wantStatus(t, rec, http.StatusAccepted)
		if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"RESET_SENT"}` {
			t.Fatalf("body = %s", got)
		}
	}
	if _, resets, _ := e.mail.counts(); resets != 1 {
		t.Fatalf("reset emails = %d, want 1", resets)
	}
	token := e.mail.resetToken(t)

	// A weak password is rejected without burning the token.
	env := wantError(t, e.post("/v1/auth/password-reset/confirm", obj{"token": token, "password": "short"}),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	if _, ok := env.Error.Fields["password"]; !ok {
		t.Fatalf("fields = %v", env.Error.Fields)
	}

	rec := e.post("/v1/auth/password-reset/confirm", obj{"token": token, "password": newPassword})
	wantStatus(t, rec, http.StatusNoContent)
	if rec.Body.Len() != 0 {
		t.Fatalf("204 must have no body, got %s", rec.Body)
	}

	// Every session of that user is revoked, cookie and bearer alike.
	wantError(t, e.get("/v1/me", withCookie(cookie)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.get("/v1/me", withBearer(bearer)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	// Other users are untouched.
	wantStatus(t, e.get("/v1/me", withBearer(bystander)), http.StatusOK)

	wantError(t, e.post("/v1/auth/login", obj{"email": identity.SeedConsumerEmail, "password": seedPassword}),
		http.StatusUnauthorized, identity.CodeInvalidCredentials)
	e.loginCookie(identity.SeedConsumerEmail, newPassword)

	// Single use.
	wantError(t, e.post("/v1/auth/password-reset/confirm", obj{"token": token, "password": "yet-another-password"}),
		http.StatusBadRequest, identity.CodeInvalidToken)
}

func TestTokensExpireAndAreSingleUse(t *testing.T) {
	t.Run("verification token used twice", func(t *testing.T) {
		e := newEnv(t)
		wantStatus(t, e.post("/v1/auth/register", registerBody("once@example.com")), http.StatusAccepted)
		token := e.mail.verifyToken(t)
		wantStatus(t, e.post("/v1/auth/verify-email", obj{"token": token}), http.StatusOK)
		rec := e.post("/v1/auth/verify-email", obj{"token": token})
		wantError(t, rec, http.StatusBadRequest, identity.CodeInvalidToken)
		if sessionCookie(rec) != nil {
			t.Fatal("a rejected token must not start a session")
		}
	})

	t.Run("verification token expires after 24h", func(t *testing.T) {
		e := newEnv(t)
		wantStatus(t, e.post("/v1/auth/register", registerBody("late@example.com")), http.StatusAccepted)
		token := e.mail.verifyToken(t)
		e.now = e.now.Add(24 * time.Hour)
		wantError(t, e.post("/v1/auth/verify-email", obj{"token": token}), http.StatusBadRequest, identity.CodeInvalidToken)
		wantError(t, e.post("/v1/auth/login", obj{"email": "late@example.com", "password": "correct-horse-battery"}),
			http.StatusForbidden, identity.CodeEmailNotVerified)
	})

	t.Run("verification token valid just before 24h", func(t *testing.T) {
		e := newEnv(t)
		wantStatus(t, e.post("/v1/auth/register", registerBody("intime@example.com")), http.StatusAccepted)
		token := e.mail.verifyToken(t)
		e.now = e.now.Add(24*time.Hour - time.Second)
		wantStatus(t, e.post("/v1/auth/verify-email", obj{"token": token}), http.StatusOK)
	})

	t.Run("reset token expires after 1h", func(t *testing.T) {
		e := newEnv(t)
		wantStatus(t, e.post("/v1/auth/password-reset/request", obj{"email": identity.SeedConsumerEmail}), http.StatusAccepted)
		token := e.mail.resetToken(t)
		e.now = e.now.Add(time.Hour)
		wantError(t, e.post("/v1/auth/password-reset/confirm", obj{"token": token, "password": newPassword}),
			http.StatusBadRequest, identity.CodeInvalidToken)
		e.loginCookie(identity.SeedConsumerEmail, seedPassword) // unchanged
	})

	t.Run("unknown and cross-purpose tokens", func(t *testing.T) {
		e := newEnv(t)
		wantStatus(t, e.post("/v1/auth/register", registerBody("cross@example.com")), http.StatusAccepted)
		verify := e.mail.verifyToken(t)
		wantStatus(t, e.post("/v1/auth/password-reset/request", obj{"email": identity.SeedConsumerEmail}), http.StatusAccepted)
		reset := e.mail.resetToken(t)

		for _, token := range []string{"nope", strings.Repeat("A", 43), reset} {
			wantError(t, e.post("/v1/auth/verify-email", obj{"token": token}), http.StatusBadRequest, identity.CodeInvalidToken)
		}
		wantError(t, e.post("/v1/auth/password-reset/confirm", obj{"token": verify, "password": newPassword}),
			http.StatusBadRequest, identity.CodeInvalidToken)
		// A session token is not a verification token either.
		wantError(t, e.post("/v1/auth/verify-email", obj{"token": e.loginBearer(identity.SeedConsumerEmail, seedPassword)}),
			http.StatusBadRequest, identity.CodeInvalidToken)
	})
}

func TestSessionExpiry(t *testing.T) {
	t.Run("idle for 14 days", func(t *testing.T) {
		e := newEnv(t)
		token := e.loginBearer(identity.SeedConsumerEmail, seedPassword)
		e.now = e.now.Add(14*24*time.Hour - time.Minute)
		wantStatus(t, e.get("/v1/me", withBearer(token)), http.StatusOK) // refreshes last_seen
		e.now = e.now.Add(14*24*time.Hour - time.Minute)
		wantStatus(t, e.get("/v1/me", withBearer(token)), http.StatusOK)
		e.now = e.now.Add(14 * 24 * time.Hour)
		wantError(t, e.get("/v1/me", withBearer(token)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("absolute 30 days even when active", func(t *testing.T) {
		e := newEnv(t)
		token := e.loginBearer(identity.SeedConsumerEmail, seedPassword)
		for range 29 {
			e.now = e.now.Add(24 * time.Hour)
			wantStatus(t, e.get("/v1/me", withBearer(token)), http.StatusOK)
		}
		e.now = e.now.Add(24 * time.Hour)
		wantError(t, e.get("/v1/me", withBearer(token)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("last_seen is written at most once a minute", func(t *testing.T) {
		e := newEnv(t)
		start := e.now
		token := e.loginBearer(identity.SeedConsumerEmail, seedPassword)
		lastSeen := func() time.Time {
			for _, s := range e.identity.Sessions() {
				if s.UserID == identity.SeedConsumerUserID {
					return s.LastSeenAt
				}
			}
			t.Fatal("session not found")
			return time.Time{}
		}
		e.now = start.Add(59 * time.Second)
		wantStatus(t, e.get("/v1/me", withBearer(token)), http.StatusOK)
		if !lastSeen().Equal(start) {
			t.Fatalf("last_seen touched after 59s: %s", lastSeen())
		}
		e.now = start.Add(61 * time.Second)
		wantStatus(t, e.get("/v1/me", withBearer(token)), http.StatusOK)
		if !lastSeen().Equal(e.now) {
			t.Fatalf("last_seen = %s, want %s", lastSeen(), e.now)
		}
	})
}

func TestCSRFOrigin(t *testing.T) {
	e := newEnv(t)
	cookie := e.loginCookie(identity.SeedConsumerEmail, seedPassword)
	bearer := e.loginBearer(identity.SeedConsumerEmail, seedPassword)

	tests := []struct {
		name       string
		opts       []reqOpt
		wantStatus int
		wantCode   string
	}{
		{"cookie without Origin", []reqOpt{withCookie(cookie)}, http.StatusForbidden, httpx.CodeCSRFOrigin},
		{"cookie with foreign Origin", []reqOpt{withCookie(cookie), withOrigin("https://evil.example")}, http.StatusForbidden, httpx.CodeCSRFOrigin},
		{"cookie with look-alike Origin", []reqOpt{withCookie(cookie), withOrigin("http://localhost:3000.evil.example")}, http.StatusForbidden, httpx.CodeCSRFOrigin},
		{"cookie with null Origin", []reqOpt{withCookie(cookie), withOrigin("null")}, http.StatusForbidden, httpx.CodeCSRFOrigin},
		{"cookie with allowed Origin", []reqOpt{browser(cookie)}, http.StatusCreated, ""},
		{"bearer without Origin", []reqOpt{withBearer(bearer)}, http.StatusCreated, ""},
		{"anonymous without Origin", nil, http.StatusCreated, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(e.requests.Requests())
			rec := e.post("/v1/property-requests", e.propertyRequest(1), tt.opts...)
			if tt.wantCode != "" {
				wantError(t, rec, tt.wantStatus, tt.wantCode)
				if len(e.requests.Requests()) != before {
					t.Fatal("a request rejected for CSRF must not be stored")
				}
				return
			}
			wantStatus(t, rec, tt.wantStatus)
		})
	}

	t.Run("cookie GET needs no Origin", func(t *testing.T) {
		wantStatus(t, e.get("/v1/me", withCookie(cookie)), http.StatusOK)
	})
	t.Run("logout by cookie without Origin is refused and the session survives", func(t *testing.T) {
		wantError(t, e.post("/v1/auth/logout", nil, withCookie(cookie)), http.StatusForbidden, httpx.CodeCSRFOrigin)
		wantStatus(t, e.get("/v1/me", withCookie(cookie)), http.StatusOK)
	})
}

func TestCORSWithCredentials(t *testing.T) {
	e := newEnv(t)

	rec := e.do(http.MethodOptions, "/v1/auth/login", nil, withOrigin(testOrigin),
		withHeader("Access-Control-Request-Method", "POST"), withHeader("Access-Control-Request-Headers", "content-type"))
	wantStatus(t, rec, http.StatusNoContent)
	h := rec.Header()
	if h.Get("Access-Control-Allow-Origin") != testOrigin || h.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("preflight headers: %v", h)
	}
	if !strings.Contains(h.Get("Access-Control-Allow-Headers"), "Content-Type") || !strings.Contains(h.Get("Access-Control-Allow-Methods"), "POST") {
		t.Fatalf("preflight headers: %v", h)
	}

	rec = e.get("/v1/me", browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword)))
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Access-Control-Allow-Origin") != testOrigin || rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("actual-request headers: %v", rec.Header())
	}

	rec = e.do(http.MethodOptions, "/v1/auth/login", nil, withOrigin("https://evil.example"), withHeader("Access-Control-Request-Method", "POST"))
	wantError(t, rec, http.StatusForbidden, httpx.CodeOriginNotAllowed)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("a foreign origin must get no CORS headers: %v", rec.Header())
	}
	rec = e.get("/v1/markets", withOrigin("https://evil.example"))
	if got := rec.Header().Get("Access-Control-Allow-Origin") + rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("a foreign origin must get no CORS headers, got %q", got)
	}
}

func TestAuthRateLimit(t *testing.T) {
	e := newEnv(t)
	body := obj{"email": "nobody@example.com", "password": "definitely-wrong-pw"}
	for i := range AuthRequestsBurst {
		// Spread over several auth routes: they share one bucket per IP.
		path := "/v1/auth/login"
		if i%2 == 1 {
			path = "/v1/auth/logout"
		}
		if rec := e.post(path, body); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate limited too early", i+1)
		}
	}
	rec := e.post("/v1/auth/login", body)
	wantError(t, rec, http.StatusTooManyRequests, httpx.CodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	wantError(t, e.post("/v1/auth/register", registerBody("limited@example.com")), http.StatusTooManyRequests, httpx.CodeRateLimited)

	// Another IP and the property-request bucket are unaffected.
	wantError(t, e.post("/v1/auth/login", body, withIP("203.0.113.9")), http.StatusUnauthorized, identity.CodeInvalidCredentials)
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(1)), http.StatusCreated)
	wantStatus(t, e.get("/v1/markets"), http.StatusOK)
}

func TestAgentInbox(t *testing.T) {
	e := newEnv(t)

	// Two requests for organisation listings and one for the direct seller.
	first := e.propertyRequest(1)
	wantStatus(t, e.post("/v1/property-requests", first), http.StatusCreated)
	e.now = e.now.Add(time.Minute)
	second := e.propertyRequest(2)
	second["guest_contact"] = obj{"name": "Farai Dube", "email": "farai@example.com"}
	wantStatus(t, e.post("/v1/property-requests", second), http.StatusCreated)
	e.now = e.now.Add(time.Minute)
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(6)), http.StatusCreated)

	t.Run("anonymous is 401", func(t *testing.T) {
		wantError(t, e.get("/v1/agent/property-requests"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("consumer is 403", func(t *testing.T) {
		wantError(t, e.get("/v1/agent/property-requests", withCookie(e.loginCookie(identity.SeedConsumerEmail, seedPassword))),
			http.StatusForbidden, httpx.CodeForbidden)
	})

	t.Run("platform admin without a membership is 403", func(t *testing.T) {
		wantError(t, e.get("/v1/agent/property-requests", withCookie(e.adminLogin(identity.SeedAdminEmail, seedPassword))),
			http.StatusForbidden, httpx.CodeForbidden)
	})

	t.Run("agent of the organisation sees its requests, newest first, and not the direct seller's", func(t *testing.T) {
		rec := e.get("/v1/agent/property-requests", withCookie(e.loginCookie(identity.SeedAgentEmail, seedPassword)))
		wantStatus(t, rec, http.StatusOK)
		inbox := decode[inboxBody](t, rec)
		if len(inbox.Items) != 2 {
			t.Fatalf("items = %d, want 2: %s", len(inbox.Items), rec.Body)
		}
		newest, oldest := inbox.Items[0], inbox.Items[1]
		if newest.ListingID != propertyrequests.SeedListingID(2).String() || oldest.ListingID != propertyrequests.SeedListingID(1).String() {
			t.Fatalf("unexpected order/listings: %s then %s", newest.ListingID, oldest.ListingID)
		}
		for _, it := range inbox.Items {
			if it.ListingID == propertyrequests.SeedListingID(6).String() {
				t.Fatal("a direct-seller request leaked into an organisation inbox")
			}
			if _, err := uuid.Parse(it.RequestID); err != nil || !strings.HasPrefix(it.Reference, "RP-") || it.CreatedAt.IsZero() {
				t.Fatalf("unexpected item: %+v", it)
			}
			if it.Status != "RECEIVED" || it.RequestType != "ENQUIRY" || it.PreferredContactMethod != "EMAIL" {
				t.Fatalf("unexpected item: %+v", it)
			}
		}
		// Each item names its listing by the public summary.
		if oldest.Listing == nil || oldest.Listing.Slug != "four-bedroom-family-home-borrowdale" || oldest.Listing.Reference != "RP-ZW-10001" ||
			oldest.Listing.Title != "Four-bedroom family home with garden" || newest.Listing == nil || newest.Listing.Reference != "RP-ZW-10002" {
			t.Fatalf("listing summaries: %+v %+v", oldest.Listing, newest.Listing)
		}
		if oldest.Message != first["message"] || oldest.Contact.Name != "Tariro Moyo" || oldest.Contact.Email != "tariro@example.com" ||
			oldest.Contact.Phone == nil || *oldest.Contact.Phone != "+263771234567" {
			t.Fatalf("unexpected contact/message: %+v", oldest)
		}
		if newest.Contact.Name != "Farai Dube" || newest.Contact.Phone != nil {
			t.Fatalf("unexpected contact: %+v", newest.Contact)
		}
	})

	t.Run("agent of another organisation sees nothing", func(t *testing.T) {
		rec := e.get("/v1/agent/property-requests", withCookie(e.loginCookie(identity.SeedAgentBEmail, seedPassword)))
		wantStatus(t, rec, http.StatusOK)
		if got := strings.TrimSpace(rec.Body.String()); !strings.HasPrefix(got, `{"items":[],"page":1,"page_size":20,"total":0,"total_pages":0,`) {
			t.Fatalf("body = %s, want an empty items array", got)
		}
	})

	t.Run("scope cannot be widened by the client", func(t *testing.T) {
		rec := e.get("/v1/agent/property-requests?organisation_id="+propertyrequests.SeedOrganisationID.String(),
			withBearer(e.loginBearer(identity.SeedAgentBEmail, seedPassword)), withHeader("X-Organisation-Id", propertyrequests.SeedOrganisationID.String()))
		wantStatus(t, rec, http.StatusOK)
		if len(decode[inboxBody](t, rec).Items) != 0 {
			t.Fatal("organisation B read organisation A's requests")
		}
	})
}

func TestAgentInboxRoleScoping(t *testing.T) {
	e := newEnv(t)
	orgA := propertyrequests.SeedOrganisationID
	ctx := context.Background()

	// Extra members of organisation A: an owner, a manager, a staff member and
	// a second agent to whom listing 7 is assigned personally.
	addUser := func(email, role string) uuid.UUID {
		id := uuid.New()
		verified := e.now
		hash, err := identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1}.Hash(seedPassword)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.identity.CreateUser(ctx, identity.NewUser{ID: id, Name: email, Email: email, PasswordHash: hash, EmailVerifiedAt: &verified, CreatedAt: e.now}); err != nil {
			t.Fatal(err)
		}
		if err := e.identity.AddMembership(id, orgA, "ZW", role); err != nil {
			t.Fatal(err)
		}
		return id
	}
	addUser("owner@example.com", auth.RoleOwner)
	addUser("manager@example.com", auth.RoleManager)
	addUser("staff@example.com", auth.RoleStaff)
	otherAgent := addUser("other-agent@example.com", auth.RoleAgent)

	// Listings outside the seeded 1..18 range, so the seed's own assignments
	// do not interfere: 30 has no assigned agent (organisation inbox), 31 is
	// assigned to other-agent, 32 cannot be received by anybody.
	e.requests.PutListing(propertyrequests.Listing{
		ID: propertyrequests.SeedListingID(30), MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
		OrganisationID: &orgA, OrganisationEligible: true,
	})
	e.requests.PutListing(propertyrequests.Listing{
		ID: propertyrequests.SeedListingID(31), MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
		OrganisationID: &orgA, OrganisationEligible: true, AgentUserID: &otherAgent, AgentEligible: true,
	})
	// A listing nobody can receive: stored as ROUTING_EXCEPTION, in no inbox.
	e.requests.PutListing(propertyrequests.Listing{
		ID: propertyrequests.SeedListingID(32), MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
		OrganisationID: &orgA, OrganisationEligible: false,
	})

	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(30)), http.StatusCreated) // organisation inbox
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(31)), http.StatusCreated) // assigned to other-agent
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(32)), http.StatusCreated) // routing exception
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(6)), http.StatusCreated)  // direct seller

	listings := func(email string) map[string]bool {
		t.Helper()
		rec := e.get("/v1/agent/property-requests", withBearer(e.loginBearer(email, seedPassword)))
		wantStatus(t, rec, http.StatusOK)
		got := map[string]bool{}
		for _, it := range decode[inboxBody](t, rec).Items {
			if it.Status != "RECEIVED" {
				t.Fatalf("%s saw status %s", email, it.Status)
			}
			got[it.ListingID] = true
		}
		return got
	}
	l1, l7 := propertyrequests.SeedListingID(30).String(), propertyrequests.SeedListingID(31).String()

	tests := []struct {
		email string
		want  []string
	}{
		{"owner@example.com", []string{l1, l7}},
		{devseed.EstatesOwnerEmail, []string{l1, l7}}, // the seeded owner of the same organisation
		{"manager@example.com", []string{l1, l7}},
		{"other-agent@example.com", []string{l1, l7}},
		{identity.SeedAgentEmail, []string{l1}}, // not the one assigned to a colleague
		{"staff@example.com", []string{l1}},
		{identity.SeedAgentBEmail, nil},
	}
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			got := listings(tt.email)
			if len(got) != len(tt.want) {
				t.Fatalf("saw %v, want %v", got, tt.want)
			}
			for _, id := range tt.want {
				if !got[id] {
					t.Fatalf("saw %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestAuthenticatedPropertyRequest(t *testing.T) {
	e := newEnv(t)
	cookie := e.loginCookie(identity.SeedConsumerEmail, seedPassword)

	body := e.propertyRequest(1)
	delete(body, "guest_contact")

	t.Run("anonymous still needs guest_contact", func(t *testing.T) {
		env := wantError(t, e.post("/v1/property-requests", body), http.StatusBadRequest, httpx.CodeValidationFailed)
		if _, ok := env.Error.Fields["guest_contact"]; !ok {
			t.Fatalf("fields = %v", env.Error.Fields)
		}
	})

	t.Run("authenticated without guest_contact", func(t *testing.T) {
		rec := e.post("/v1/property-requests", body, browser(cookie))
		wantStatus(t, rec, http.StatusCreated)
		if !strings.Contains(rec.Body.String(), `"status":"RECEIVED"`) {
			t.Fatalf("body = %s", rec.Body)
		}
		reqs := e.requests.Requests()
		stored := reqs[len(reqs)-1]
		if stored.CustomerUserID == nil || *stored.CustomerUserID != identity.SeedConsumerUserID {
			t.Fatalf("customer user id = %v, want %s", stored.CustomerUserID, identity.SeedConsumerUserID)
		}
		if stored.GuestName != "Casey Consumer" || stored.GuestEmail != identity.SeedConsumerEmail || stored.GuestPhone != "" {
			t.Fatalf("contact snapshot = %q %q %q", stored.GuestName, stored.GuestEmail, stored.GuestPhone)
		}
		act := e.requests.Activity()
		if last := act[len(act)-1]; last.UserID == nil || *last.UserID != identity.SeedConsumerUserID || last.GuestEmail != "" {
			t.Fatalf("activity not attributed to the account: %+v", last)
		}

		// Idempotent replay for the same customer; conflict for anyone else.
		wantStatus(t, e.post("/v1/property-requests", body, browser(cookie)), http.StatusOK)
		wantError(t, e.post("/v1/property-requests", body, withBearer(e.loginBearer(identity.SeedAgentEmail, seedPassword))),
			http.StatusConflict, propertyrequests.CodeIdempotencyConflict)

		// The receiving organisation sees the profile's contact details.
		inbox := decode[inboxBody](t, e.get("/v1/agent/property-requests", withBearer(e.loginBearer(identity.SeedAgentEmail, seedPassword))))
		if len(inbox.Items) != 1 || inbox.Items[0].Contact.Email != identity.SeedConsumerEmail || inbox.Items[0].Contact.Name != "Casey Consumer" {
			t.Fatalf("inbox = %+v", inbox.Items)
		}
	})

	t.Run("identity in the body is ignored when authenticated", func(t *testing.T) {
		b := e.propertyRequest(2)
		b["guest_contact"] = obj{"name": "Someone Else", "email": "spoof@example.com", "phone": "+263771234567"}
		wantStatus(t, e.post("/v1/property-requests", b, browser(cookie)), http.StatusCreated)
		reqs := e.requests.Requests()
		stored := reqs[len(reqs)-1]
		if stored.GuestEmail != identity.SeedConsumerEmail || stored.GuestName != "Casey Consumer" || stored.GuestPhone != "+263771234567" {
			t.Fatalf("contact = %q %q %q", stored.GuestName, stored.GuestEmail, stored.GuestPhone)
		}
	})

	t.Run("phone still required for PHONE and WHATSAPP", func(t *testing.T) {
		for _, method := range []string{"PHONE", "WHATSAPP"} {
			b := e.propertyRequest(1)
			delete(b, "guest_contact")
			b["preferred_contact_method"] = method
			// A second client IP keeps this subtest clear of the per-IP rate limit.
			env := wantError(t, e.post("/v1/property-requests", b, browser(cookie), withIP("203.0.113.7")), http.StatusBadRequest, httpx.CodeValidationFailed)
			if _, ok := env.Error.Fields["guest_contact.phone"]; !ok || len(env.Error.Fields) != 1 {
				t.Fatalf("fields = %v, want only guest_contact.phone", env.Error.Fields)
			}
			b["guest_contact"] = obj{"phone": "+263771234567"}
			wantStatus(t, e.post("/v1/property-requests", b, browser(cookie), withIP("203.0.113.7")), http.StatusCreated)
		}
	})

	t.Run("consent still required", func(t *testing.T) {
		b := e.propertyRequest(1)
		delete(b, "guest_contact")
		b["consent"] = obj{"privacy_version": "2026-01", "accepted": false}
		env := wantError(t, e.post("/v1/property-requests", b, browser(cookie)), http.StatusBadRequest, httpx.CodeValidationFailed)
		if _, ok := env.Error.Fields["consent.accepted"]; !ok {
			t.Fatalf("fields = %v", env.Error.Fields)
		}
	})

	t.Run("anonymous request stores no user id", func(t *testing.T) {
		wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(3)), http.StatusCreated)
		reqs := e.requests.Requests()
		if reqs[len(reqs)-1].CustomerUserID != nil {
			t.Fatal("guest request must not carry a customer user id")
		}
	})
}

// ---------------------------------------------------------------------------
// Account roles chosen at registration
// ---------------------------------------------------------------------------

// me returns the `user` object of GET /v1/me for a cookie session.
func (e *env) me(c *http.Cookie) (userBody, string) {
	e.t.Helper()
	rec := e.get("/v1/me", withCookie(c))
	wantStatus(e.t, rec, http.StatusOK)
	return decode[struct {
		User userBody `json:"user"`
	}](e.t, rec).User, rec.Body.String()
}

func TestRegisterWithAccountRoles(t *testing.T) {
	e := newEnv(t)
	body := registerBody("multi@example.com")
	// Deliberately not in allowlist order: the API returns a stable order.
	body["roles"] = []string{"LANDLORD", "HOST", "AGENT", "SOLICITOR"}

	rec := e.post("/v1/auth/register", body)
	wantStatus(t, rec, http.StatusAccepted)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"VERIFICATION_SENT"}` {
		t.Fatalf("body = %s", got)
	}

	rec = e.post("/v1/auth/verify-email", obj{"token": e.mail.verifyToken(t)})
	wantStatus(t, rec, http.StatusOK)
	want := []accountRoleBody{
		{"AGENT", "PENDING_VERIFICATION"}, {"SOLICITOR", "PENDING_VERIFICATION"},
		{"LANDLORD", "PENDING_VERIFICATION"}, {"HOST", "PENDING_VERIFICATION"},
	}
	check := func(where string, u userBody) {
		t.Helper()
		if len(u.AccountRoles) != len(want) {
			t.Fatalf("%s: account_roles = %+v, want %+v", where, u.AccountRoles, want)
		}
		for i := range want {
			if u.AccountRoles[i] != want[i] {
				t.Fatalf("%s: account_roles = %+v, want %+v", where, u.AccountRoles, want)
			}
		}
		// A requested professional role is not a membership or a platform role.
		if len(u.Memberships) != 0 || len(u.PlatformRoles) != 0 {
			t.Fatalf("%s: selecting roles must not create memberships or platform roles: %+v", where, u)
		}
	}
	check("verify-email", decode[authBody](t, rec).User)

	cookie := sessionCookie(rec)
	meUser, raw := e.me(cookie)
	check("/v1/me", meUser)
	if !strings.Contains(raw, `"memberships":[],"account_roles":[{"role":"AGENT","status":"PENDING_VERIFICATION","onboarding_status":"DRAFT"},`) {
		t.Fatalf("unexpected /v1/me body: %s", raw)
	}

	login := e.post("/v1/auth/login", obj{"email": "multi@example.com", "password": "correct-horse-battery", "token_delivery": "bearer"})
	wantStatus(t, login, http.StatusOK)
	check("login", decode[authBody](t, login).User)
}

func TestRegisterRolesValidation(t *testing.T) {
	const (
		required   = "Choose at least one role."
		notAllowed = "That role cannot be chosen at sign-up."
		unknown    = "Choose roles from the options provided."
		duplicated = "Each role can only be chosen once."
		seekerOnly = "A property seeker account cannot also have a professional, business or owner role. Choose one or the other."
	)
	tests := []struct {
		name    string
		roles   any // nil = omit the field
		wantMsg string
	}{
		{"missing", nil, required},
		{"null", json.RawMessage("null"), required},
		{"empty array", []string{}, required},
		{"unknown value", []string{"CONSUMER", "WIZARD"}, unknown},
		{"lower case is not an allowlisted code", []string{"consumer"}, unknown},
		{"empty string", []string{""}, unknown},
		{"membership role name", []string{"OWNER"}, unknown},
		{"organisation kind that is not an account role", []string{"AGENCY"}, unknown},
		{"non-string entry", []any{"CONSUMER", 7}, unknown},
		{"null entry", []any{"CONSUMER", nil}, unknown},
		{"object entry", []any{obj{"role": "CONSUMER"}}, unknown},
		{"not an array", "CONSUMER", unknown},
		{"duplicate", []string{"CONSUMER", "AGENT", "CONSUMER"}, duplicated},
		{"platform role hidden behind a duplicate", []string{"CONSUMER", "CONSUMER", "SUPER_ADMIN"}, duplicated},
		// A property seeker account is only that.
		{"buyer with a professional role", []string{"CONSUMER", "AGENT"}, seekerOnly},
		{"tenant with an owner role", []string{"LANDLORD", "TENANT"}, seekerOnly},
		{"tenant with a business role", []string{"TENANT", "AGENCY_OWNER"}, seekerOnly},
		{"buyer with another property business", []string{"HOST", "CONSUMER"}, seekerOnly},
		{"both seeker roles with a professional role", []string{"CONSUMER", "TENANT", "SOLICITOR"}, seekerOnly},
		// The other checks still come first.
		{"seeker mix with an unknown value", []string{"CONSUMER", "AGENT", "WIZARD"}, unknown},
	}
	// Every platform administrator role, alone and mixed with a valid one.
	for _, role := range []string{
		auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformMarketAdmin, auth.PlatformFinanceAdmin,
		auth.PlatformContentAdmin, auth.PlatformModerator, auth.PlatformSupport,
	} {
		tests = append(tests,
			struct {
				name    string
				roles   any
				wantMsg string
			}{"platform role " + role, []string{role}, notAllowed},
			struct {
				name    string
				roles   any
				wantMsg string
			}{"platform role " + role + " with CONSUMER", []string{"CONSUMER", role}, notAllowed},
		)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			before := e.identity.UserCount()
			body := registerBody("roles@example.com")
			if tt.roles == nil {
				delete(body, "roles")
			} else {
				body["roles"] = tt.roles
			}
			env := wantError(t, e.post("/v1/auth/register", body), http.StatusBadRequest, httpx.CodeValidationFailed)
			if got := env.Error.Fields["roles"]; got != tt.wantMsg || len(env.Error.Fields) != 1 {
				t.Fatalf("fields = %v, want only roles=%q", env.Error.Fields, tt.wantMsg)
			}
			if e.identity.UserCount() != before {
				t.Fatal("a rejected registration created a user")
			}
			if v, _, _ := e.mail.counts(); v != 0 {
				t.Fatal("a rejected registration sent a verification email")
			}
		})
	}

	// The allowlist splits in two: the property seeker roles, and all the
	// others. Each half can be chosen whole; the two never mix.
	all := identity.SelectableAccountRoles()
	if len(all) != 15 {
		t.Fatalf("allowlist has %d roles, want 15", len(all))
	}
	var seekers, others []string
	for _, role := range all {
		if identity.IsSeekerRole(role) {
			seekers = append(seekers, role)
		} else {
			others = append(others, role)
		}
	}
	if len(seekers) != 2 || len(others) != 13 {
		t.Fatalf("seeker roles = %v, other roles = %v", seekers, others)
	}
	accepted := []struct {
		name       string
		roles      []string
		wantStatus string
	}{
		{"buyer and tenant together are accepted", seekers, "ACTIVE"},
		{"buyer alone is accepted", []string{"CONSUMER"}, "ACTIVE"},
		{"tenant alone is accepted", []string{"TENANT"}, "ACTIVE"},
		{"agent and landlord together are accepted", []string{"AGENT", "LANDLORD"}, "PENDING_VERIFICATION"},
		{"every role that is not a seeker role at once is accepted", others, "PENDING_VERIFICATION"},
	}
	for i, tt := range accepted {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			body := registerBody(fmt.Sprintf("accepted%d@example.com", i))
			body["roles"] = tt.roles
			wantStatus(t, e.post("/v1/auth/register", body), http.StatusAccepted)
			rec := e.post("/v1/auth/verify-email", obj{"token": e.mail.verifyToken(t)})
			wantStatus(t, rec, http.StatusOK)
			got := decode[authBody](t, rec).User.AccountRoles
			if len(got) != len(tt.roles) {
				t.Fatalf("account_roles = %+v", got)
			}
			for i, r := range got {
				if r.Role != tt.roles[i] || r.Status != tt.wantStatus {
					t.Fatalf("account_roles[%d] = %+v, want %s %s", i, r, tt.roles[i], tt.wantStatus)
				}
			}
		})
	}

	t.Run("every seeker role with every other role is refused, in either order", func(t *testing.T) {
		e := newEnv(t)
		before := e.identity.UserCount()
		n := 0
		for _, seeker := range seekers {
			for _, other := range others {
				for _, roles := range [][]string{{seeker, other}, {other, seeker}} {
					n++
					body := registerBody("mixed@example.com")
					body["roles"] = roles
					// Each from its own address: this is not about the rate limit.
					rec := e.post("/v1/auth/register", body, withIP(fmt.Sprintf("198.51.100.%d", n)))
					env := wantError(t, rec, http.StatusBadRequest, httpx.CodeValidationFailed)
					if got := env.Error.Fields["roles"]; got != seekerOnly || len(env.Error.Fields) != 1 {
						t.Fatalf("roles %v: fields = %v, want only roles=%q", roles, env.Error.Fields, seekerOnly)
					}
				}
			}
		}
		if e.identity.UserCount() != before {
			t.Fatal("a rejected registration created a user")
		}
	})
}

// Choosing AGENT at sign-up is only a request: it must not open the
// organisation workspace or create a membership.
func TestSelfDeclaredAgentHasNoAccess(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(1)), http.StatusCreated)

	body := registerBody("wannabe@example.com")
	body["roles"] = []string{"AGENT", "AGENCY_OWNER", "AGENCY_ADMIN", "AGENCY_STAFF"}
	wantStatus(t, e.post("/v1/auth/register", body), http.StatusAccepted)
	rec := e.post("/v1/auth/verify-email", obj{"token": e.mail.verifyToken(t)})
	wantStatus(t, rec, http.StatusOK)
	cookie := sessionCookie(rec)

	user, _ := e.me(cookie)
	if len(user.Memberships) != 0 || len(user.PlatformRoles) != 0 {
		t.Fatalf("self-declared roles created access: %+v", user)
	}
	if len(user.AccountRoles) != 4 {
		t.Fatalf("account_roles = %+v", user.AccountRoles)
	}
	for _, r := range user.AccountRoles {
		if r.Status != "PENDING_VERIFICATION" {
			t.Fatalf("professional role %s must be PENDING_VERIFICATION, got %s", r.Role, r.Status)
		}
	}

	wantError(t, e.get("/v1/agent/property-requests", withCookie(cookie)), http.StatusForbidden, httpx.CodeForbidden)
	bearer := e.loginBearer("wannabe@example.com", "correct-horse-battery")
	wantError(t, e.get("/v1/agent/property-requests", withBearer(bearer)), http.StatusForbidden, httpx.CodeForbidden)

	// The real member is unaffected, and an ACTIVE seeded AGENT account role
	// without a membership in that organisation still sees nothing of it.
	inbox := decode[inboxBody](t, e.get("/v1/agent/property-requests", withBearer(e.loginBearer(identity.SeedAgentEmail, seedPassword))))
	if len(inbox.Items) != 1 {
		t.Fatalf("member inbox = %d items, want 1", len(inbox.Items))
	}
	inbox = decode[inboxBody](t, e.get("/v1/agent/property-requests", withBearer(e.loginBearer(identity.SeedAgentBEmail, seedPassword))))
	if len(inbox.Items) != 0 {
		t.Fatal("organisation B saw organisation A's requests")
	}
}

func TestRegisterExistingEmailWithRoles(t *testing.T) {
	t.Run("verified account: 202 and its roles are untouched", func(t *testing.T) {
		e := newEnv(t)
		before := e.identity.UserCount()
		body := registerBody(identity.SeedConsumerEmail)
		body["roles"] = []string{"AGENCY_OWNER", "LANDLORD"}
		rec := e.post("/v1/auth/register", body)
		wantStatus(t, rec, http.StatusAccepted)
		if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"VERIFICATION_SENT"}` {
			t.Fatalf("body = %s", got)
		}
		if e.identity.UserCount() != before {
			t.Fatal("a second user was created for an existing email")
		}
		user, _ := e.me(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
		if len(user.AccountRoles) != 1 || user.AccountRoles[0] != (accountRoleBody{"CONSUMER", "ACTIVE"}) {
			t.Fatalf("account_roles of the existing account changed: %+v", user.AccountRoles)
		}
	})

	t.Run("unverified account: 202 and the latest selection replaces the pending one", func(t *testing.T) {
		e := newEnv(t)
		first := registerBody("pending-roles@example.com")
		first["roles"] = []string{"CONSUMER", "TENANT"}
		wantStatus(t, e.post("/v1/auth/register", first), http.StatusAccepted)
		count := e.identity.UserCount()

		second := registerBody("pending-roles@example.com")
		second["roles"] = []string{"AGENT", "HOST"}
		wantStatus(t, e.post("/v1/auth/register", second), http.StatusAccepted)
		if e.identity.UserCount() != count {
			t.Fatal("a second user was created for an existing email")
		}

		rec := e.post("/v1/auth/verify-email", obj{"token": e.mail.verifyToken(t)})
		wantStatus(t, rec, http.StatusOK)
		got := decode[authBody](t, rec).User.AccountRoles
		if len(got) != 2 || got[0] != (accountRoleBody{"AGENT", "PENDING_VERIFICATION"}) || got[1] != (accountRoleBody{"HOST", "PENDING_VERIFICATION"}) {
			t.Fatalf("account_roles = %+v, want AGENT + HOST, both PENDING_VERIFICATION", got)
		}
	})

	t.Run("invalid roles are rejected even for an existing email", func(t *testing.T) {
		e := newEnv(t)
		body := registerBody(identity.SeedConsumerEmail)
		body["roles"] = []string{"SUPPORT"}
		wantError(t, e.post("/v1/auth/register", body), http.StatusBadRequest, httpx.CodeValidationFailed)
	})
}

// ---------------------------------------------------------------------------
// Administrator login and session scopes
// ---------------------------------------------------------------------------

type meBody struct {
	User    userBody `json:"user"`
	Session struct {
		Scope     string    `json:"scope"`
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"session"`
}

// nonSeekerRoles is every role that can be chosen at sign-up except the
// property seeker roles: the most one account can declare.
func nonSeekerRoles() []string {
	var out []string
	for _, role := range identity.SelectableAccountRoles() {
		if !identity.IsSeekerRole(role) {
			out = append(out, role)
		}
	}
	return out
}

// selfRegister creates and verifies an account through the public API.
func (e *env) selfRegister(email string, roles ...string) *http.Cookie {
	e.t.Helper()
	body := registerBody(email)
	body["roles"] = roles
	// A fresh client IP per registration keeps helpers clear of the auth rate limit.
	e.seq++
	ip := withIP(fmt.Sprintf("192.0.2.%d", 100+e.seq%100))
	wantStatus(e.t, e.post("/v1/auth/register", body, ip), http.StatusAccepted)
	rec := e.post("/v1/auth/verify-email", obj{"token": e.mail.verifyToken(e.t)}, ip)
	wantStatus(e.t, rec, http.StatusOK)
	return sessionCookie(rec)
}

func TestAdminLogin(t *testing.T) {
	e := newEnv(t)
	rec := e.post("/v1/admin/auth/login", obj{"email": " Admin@Example.test ", "password": seedPassword})
	wantStatus(t, rec, http.StatusOK)

	if strings.Contains(rec.Body.String(), "access_token") {
		t.Fatalf("admin login must never return a bearer token: %s", rec.Body)
	}
	body := decode[authBody](t, rec)
	if body.Session == nil || body.Session.Scope != "admin" || !body.Session.ExpiresAt.Equal(e.now.Add(12*time.Hour)) {
		t.Fatalf("session = %+v, want scope admin expiring in 12h", body.Session)
	}
	if body.User.Email != identity.SeedAdminEmail || len(body.User.PlatformRoles) != 1 || body.User.PlatformRoles[0].Role != "SUPER_ADMIN" {
		t.Fatalf("unexpected user: %+v", body.User)
	}
	c := sessionCookie(rec)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 12*3600 {
		t.Fatalf("unexpected admin cookie: %+v", c)
	}
	for _, sess := range e.identity.Sessions() {
		if sess.UserID == identity.SeedAdminUserID && (sess.Scope != auth.ScopeAdmin || sess.Delivery != identity.DeliveryCookie) {
			t.Fatalf("admin session stored as scope=%s delivery=%s", sess.Scope, sess.Delivery)
		}
	}

	for _, path := range []string{"/v1/admin/me", "/v1/me"} {
		rec := e.get(path, withCookie(c))
		wantStatus(t, rec, http.StatusOK)
		me := decode[meBody](t, rec)
		if me.Session.Scope != "admin" || !me.Session.ExpiresAt.Equal(e.now.Add(12*time.Hour)) || me.User.Email != identity.SeedAdminEmail {
			t.Fatalf("%s = %s", path, rec.Body)
		}
	}

	// Customer sessions report their scope too.
	login := e.post("/v1/auth/login", obj{"email": identity.SeedConsumerEmail, "password": seedPassword})
	if lb := decode[authBody](t, login); lb.Session == nil || lb.Session.Scope != "customer" {
		t.Fatalf("customer login session = %+v", lb.Session)
	}

	// Signing out is the ordinary logout; the session is revoked.
	wantStatus(t, e.post("/v1/auth/logout", nil, browser(c)), http.StatusNoContent)
	wantError(t, e.get("/v1/admin/me", withCookie(c)), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// Audit lines: success recorded, never the password or the raw email.
	logs := e.logs.String()
	if !strings.Contains(logs, `"audit":true,"event":"admin_sign_in","outcome":"success"`) {
		t.Fatalf("no audit line for the successful admin sign-in")
	}
	if strings.Contains(logs, seedPassword) || strings.Contains(logs, c.Value) {
		t.Fatal("logs contain the password or the session token")
	}
}

func TestAdminLoginConfigurableLifetime(t *testing.T) {
	e := newEnvWithConfig(t, func(c *config.Config) {
		c.AdminSessionTTL = 2 * time.Hour
		c.AdminSessionIdleTTL = 10 * time.Minute
	})
	rec := e.post("/v1/admin/auth/login", obj{"email": identity.SeedAdminEmail, "password": seedPassword})
	wantStatus(t, rec, http.StatusOK)
	if got := decode[authBody](t, rec).Session.ExpiresAt; !got.Equal(e.now.Add(2 * time.Hour)) {
		t.Fatalf("expires_at = %s, want +2h", got)
	}
	c := sessionCookie(rec)
	e.now = e.now.Add(10 * time.Minute)
	wantError(t, e.get("/v1/admin/me", withCookie(c)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
}

// Every refusal at the administrator login is the same 401 with the same
// body, so the endpoint does not reveal who is an administrator.
func TestAdminLoginRefusalsAreIdentical(t *testing.T) {
	e := newEnv(t)
	e.selfRegister("pro@example.com", "AGENT", "AGENCY_OWNER")
	wantStatus(t, e.post("/v1/auth/register", registerBody("unverified@example.com")), http.StatusAccepted)

	const rid = "fixed-request-id"
	attempt := func(path, email, password string, n int) *httptest.ResponseRecorder {
		// A distinct client IP per attempt keeps this clear of the rate limits.
		return e.post(path, obj{"email": email, "password": password},
			withHeader(httpx.HeaderRequestID, rid), withIP(fmt.Sprintf("198.51.100.%d", n)))
	}

	reference := attempt("/v1/admin/auth/login", identity.SeedAdminEmail, "definitely-wrong-pw", 1)
	wantError(t, reference, http.StatusUnauthorized, identity.CodeInvalidCredentials)

	tests := []struct {
		name, path, email, password string
	}{
		{"admin endpoint: unknown email", "/v1/admin/auth/login", "nobody@example.com", "definitely-wrong-pw"},
		{"admin endpoint: consumer with the right password", "/v1/admin/auth/login", identity.SeedConsumerEmail, seedPassword},
		{"admin endpoint: organisation agent with the right password", "/v1/admin/auth/login", identity.SeedAgentEmail, seedPassword},
		{"admin endpoint: self-registered professional with the right password", "/v1/admin/auth/login", "pro@example.com", "correct-horse-battery"},
		{"admin endpoint: unverified account with the right password", "/v1/admin/auth/login", "unverified@example.com", "correct-horse-battery"},
		{"customer endpoint: admin with the right password", "/v1/auth/login", identity.SeedAdminEmail, seedPassword},
		{"customer endpoint: admin with a wrong password", "/v1/auth/login", identity.SeedAdminEmail, "definitely-wrong-pw"},
		{"customer endpoint: consumer with a wrong password", "/v1/auth/login", identity.SeedConsumerEmail, "definitely-wrong-pw"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := attempt(tt.path, tt.email, tt.password, i+2)
			wantError(t, rec, http.StatusUnauthorized, identity.CodeInvalidCredentials)
			if rec.Body.String() != reference.Body.String() {
				t.Fatalf("body differs from a wrong-password response:\n%s\n%s", rec.Body, reference.Body)
			}
			if sessionCookie(rec) != nil || strings.Contains(rec.Body.String(), "access_token") {
				t.Fatal("a refused login must not issue a session")
			}
		})
	}

	t.Run("customer endpoint refuses the admin with bearer delivery too", func(t *testing.T) {
		rec := e.post("/v1/auth/login", obj{"email": identity.SeedAdminEmail, "password": seedPassword, "token_delivery": "bearer"},
			withHeader(httpx.HeaderRequestID, rid), withIP("198.51.100.50"))
		wantError(t, rec, http.StatusUnauthorized, identity.CodeInvalidCredentials)
		if rec.Body.String() != reference.Body.String() {
			t.Fatalf("body differs: %s", rec.Body)
		}
	})

	t.Run("no admin session exists after all of that", func(t *testing.T) {
		for _, sess := range e.identity.Sessions() {
			if sess.Scope == auth.ScopeAdmin || sess.UserID == identity.SeedAdminUserID {
				t.Fatalf("unexpected session: %+v", sess)
			}
		}
	})

	logs := e.logs.String()
	for _, want := range []string{
		`"event":"admin_sign_in","outcome":"failure"`, `"reason":"wrong_password"`, `"reason":"unknown_email"`,
		`"reason":"not_a_platform_account"`, `"ip":"198.51.100.1"`, `"trace_id":"` + rid + `"`,
		`"event":"customer_sign_in_refused_platform_account"`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("audit log is missing %s", want)
		}
	}
	for _, secret := range []string{seedPassword, "definitely-wrong-pw", "correct-horse-battery", "nobody@example.com", identity.SeedAdminEmail} {
		if strings.Contains(logs, secret) {
			t.Fatalf("audit log contains %q", secret)
		}
	}
}

func TestAdminLoginIsCookieOnly(t *testing.T) {
	e := newEnv(t)

	// token_delivery is not part of this endpoint: strict JSON rejects it.
	rec := e.post("/v1/admin/auth/login", obj{"email": identity.SeedAdminEmail, "password": seedPassword, "token_delivery": "bearer"})
	wantError(t, rec, http.StatusBadRequest, httpx.CodeInvalidJSON)
	if sessionCookie(rec) != nil {
		t.Fatal("no session may be issued")
	}
	env := wantError(t, e.post("/v1/admin/auth/login", obj{}), http.StatusBadRequest, httpx.CodeValidationFailed)
	if len(env.Error.Fields) != 2 {
		t.Fatalf("fields = %v", env.Error.Fields)
	}

	// The admin cookie's token presented as a bearer credential is not a login.
	c := e.adminLogin(identity.SeedAdminEmail, seedPassword)
	wantStatus(t, e.get("/v1/admin/me", withCookie(c)), http.StatusOK)
	wantError(t, e.get("/v1/admin/me", withBearer(c.Value)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.get("/v1/me", withBearer(c.Value)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	// A cookie-authenticated admin POST still needs an allowlisted Origin.
	wantError(t, e.post("/v1/auth/logout", nil, withCookie(c)), http.StatusForbidden, httpx.CodeCSRFOrigin)
}

func TestAdminGate(t *testing.T) {
	e := newEnv(t)

	t.Run("anonymous is 401", func(t *testing.T) {
		wantError(t, e.get("/v1/admin/me"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("customer sessions are 403", func(t *testing.T) {
		for _, c := range []*http.Cookie{
			e.loginCookie(identity.SeedConsumerEmail, seedPassword),
			e.loginCookie(identity.SeedAgentEmail, seedPassword),
			e.selfRegister("wannabe-admin@example.com", "AGENCY_OWNER", "AGENCY_ADMIN"),
		} {
			wantError(t, e.get("/v1/admin/me", withCookie(c)), http.StatusForbidden, httpx.CodeForbidden)
		}
		wantError(t, e.get("/v1/admin/me", withBearer(e.loginBearer(identity.SeedConsumerEmail, seedPassword))),
			http.StatusForbidden, httpx.CodeForbidden)
	})

	// A customer-scope session cannot be obtained by a platform-role account
	// through the API, so build one directly in the store: even then the
	// platform role must not count.
	t.Run("platform role on a customer-scope session is 403", func(t *testing.T) {
		for _, delivery := range []string{identity.DeliveryCookie, identity.DeliveryBearer} {
			token := strings.Repeat("c", 42) + delivery[:1] // 43 characters, like a real token
			sum := sha256.Sum256([]byte(token))
			if err := e.identity.CreateSession(context.Background(), identity.Session{
				ID: uuid.New(), UserID: identity.SeedAdminUserID, TokenHash: sum[:], Scope: auth.ScopeCustomer, Delivery: delivery,
				CreatedAt: e.now, LastSeenAt: e.now, ExpiresAt: e.now.Add(time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			opt := withCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
			if delivery == identity.DeliveryBearer {
				opt = withBearer(token)
			}
			wantError(t, e.get("/v1/admin/me", opt), http.StatusForbidden, httpx.CodeForbidden)

			rec := e.get("/v1/me", opt)
			wantStatus(t, rec, http.StatusOK)
			me := decode[meBody](t, rec)
			if me.Session.Scope != "customer" || len(me.User.PlatformRoles) != 1 {
				t.Fatalf("/v1/me = %s", rec.Body)
			}
		}
	})

	t.Run("admin session with a platform role is 200", func(t *testing.T) {
		wantStatus(t, e.get("/v1/admin/me", withCookie(e.adminLogin(identity.SeedAdminEmail, seedPassword))), http.StatusOK)
	})
}

func TestAdminSessionExpiry(t *testing.T) {
	t.Run("idle for 30 minutes", func(t *testing.T) {
		e := newEnv(t)
		c := e.adminLogin(identity.SeedAdminEmail, seedPassword)
		e.now = e.now.Add(29 * time.Minute)
		wantStatus(t, e.get("/v1/admin/me", withCookie(c)), http.StatusOK) // refreshes last_seen
		e.now = e.now.Add(29 * time.Minute)
		wantStatus(t, e.get("/v1/admin/me", withCookie(c)), http.StatusOK)
		e.now = e.now.Add(30 * time.Minute)
		wantError(t, e.get("/v1/admin/me", withCookie(c)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		wantError(t, e.get("/v1/me", withCookie(c)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("absolute 12 hours even when active", func(t *testing.T) {
		e := newEnv(t)
		c := e.adminLogin(identity.SeedAdminEmail, seedPassword)
		for range 47 { // every 15 minutes up to 11h45m
			e.now = e.now.Add(15 * time.Minute)
			wantStatus(t, e.get("/v1/admin/me", withCookie(c)), http.StatusOK)
		}
		e.now = e.now.Add(15 * time.Minute)
		wantError(t, e.get("/v1/admin/me", withCookie(c)), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("a customer session is unaffected by the admin lifetimes", func(t *testing.T) {
		e := newEnv(t)
		c := e.loginCookie(identity.SeedConsumerEmail, seedPassword)
		e.now = e.now.Add(13 * time.Hour)
		wantStatus(t, e.get("/v1/me", withCookie(c)), http.StatusOK)
	})

	t.Run("last_seen is refreshed often enough for a 30-minute idle window", func(t *testing.T) {
		e := newEnv(t)
		start := e.now
		c := e.adminLogin(identity.SeedAdminEmail, seedPassword)
		lastSeen := func() time.Time {
			for _, s := range e.identity.Sessions() {
				if s.UserID == identity.SeedAdminUserID {
					return s.LastSeenAt
				}
			}
			t.Fatal("session not found")
			return time.Time{}
		}
		e.now = start.Add(59 * time.Second)
		wantStatus(t, e.get("/v1/admin/me", withCookie(c)), http.StatusOK)
		if !lastSeen().Equal(start) {
			t.Fatalf("last_seen written after 59s: %s", lastSeen())
		}
		e.now = start.Add(61 * time.Second)
		wantStatus(t, e.get("/v1/admin/me", withCookie(c)), http.StatusOK)
		if !lastSeen().Equal(e.now) {
			t.Fatalf("last_seen = %s, want %s", lastSeen(), e.now)
		}
	})
}

func TestAdminLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	body := obj{"email": identity.SeedAdminEmail, "password": "definitely-wrong-pw"}
	for range AdminAuthRequestsBurst {
		wantError(t, e.post("/v1/admin/auth/login", body), http.StatusUnauthorized, identity.CodeInvalidCredentials)
	}
	// Even the correct password is now refused for this IP.
	rec := e.post("/v1/admin/auth/login", obj{"email": identity.SeedAdminEmail, "password": seedPassword})
	wantError(t, rec, http.StatusTooManyRequests, httpx.CodeRateLimited)
	if rec.Header().Get("Retry-After") == "" || sessionCookie(rec) != nil {
		t.Fatal("429 must carry Retry-After and no session")
	}
	// The customer auth bucket and other IPs are separate.
	e.loginCookie(identity.SeedConsumerEmail, seedPassword)
	e.adminLogin(identity.SeedAdminEmail, seedPassword, withIP("203.0.113.9"))
}

func TestAdminPasswordResetSendsNothing(t *testing.T) {
	e := newEnv(t)
	rec := e.post("/v1/auth/password-reset/request", obj{"email": identity.SeedAdminEmail})
	wantStatus(t, rec, http.StatusAccepted)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"RESET_SENT"}` {
		t.Fatalf("body = %s", got)
	}
	if _, resets, _ := e.mail.counts(); resets != 0 {
		t.Fatalf("a reset email was sent for a platform-role account")
	}
	// Ordinary accounts still get theirs.
	wantStatus(t, e.post("/v1/auth/password-reset/request", obj{"email": identity.SeedConsumerEmail}), http.StatusAccepted)
	if _, resets, _ := e.mail.counts(); resets != 1 {
		t.Fatalf("reset emails = %d, want 1", resets)
	}
	e.adminLogin(identity.SeedAdminEmail, seedPassword) // password unchanged
}

// A platform-role account that verifies its email is verified but NOT signed
// in: administrators only get a session from the administrator login.
func TestVerifyEmailDoesNotSignInPlatformAccount(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.post("/v1/auth/register", registerBody("promoted@example.com")), http.StatusAccepted)
	token := e.mail.verifyToken(t)
	user, err := e.identity.UserByEmail(context.Background(), "promoted@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.identity.AddPlatformRole(user.ID, auth.PlatformSupport, ""); err != nil {
		t.Fatal(err)
	}
	before := len(e.identity.Sessions())

	rec := e.post("/v1/auth/verify-email", obj{"token": token})
	wantStatus(t, rec, http.StatusOK)
	if sessionCookie(rec) != nil || strings.Contains(rec.Body.String(), `"session"`) || strings.Contains(rec.Body.String(), "access_token") {
		t.Fatalf("a platform-role account must not be signed in by verify-email: %s", rec.Body)
	}
	body := decode[authBody](t, rec)
	if body.Session != nil || !body.User.EmailVerified || body.User.Email != "promoted@example.com" {
		t.Fatalf("unexpected body: %s", rec.Body)
	}
	if len(e.identity.Sessions()) != before {
		t.Fatal("a session was created")
	}
	// Now verified, the account can use the administrator login.
	e.adminLogin("promoted@example.com", "correct-horse-battery")
}

// ---------------------------------------------------------------------------
// Sample data: every listing belongs to a named agent or seller
// ---------------------------------------------------------------------------

// inboxListings returns the listing numbers in the caller's inbox.
func (e *env) inboxListings(email string) []int {
	e.t.Helper()
	rec := e.get("/v1/agent/property-requests", withBearer(e.loginBearer(email, seedPassword)))
	wantStatus(e.t, rec, http.StatusOK)
	var got []int
	for _, it := range decode[inboxBody](e.t, rec).Items {
		n := 0
		for i := 1; i <= 99; i++ {
			if propertyrequests.SeedListingID(i).String() == it.ListingID {
				n = i
				break
			}
		}
		got = append(got, n)
	}
	sort.Ints(got)
	return got
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSeededInboxScoping(t *testing.T) {
	e := newEnv(t)
	// One request each for: Tendai's 1, Chipo's 7 (same agency), Farai's 3
	// (another agency), Grace's 6 (direct seller).
	for i, n := range []int{1, 7, 3, 6} {
		e.now = e.now.Add(time.Duration(i) * time.Second)
		wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(n)), http.StatusCreated)
	}

	tests := []struct {
		who, email string
		want       []int
	}{
		{"Tendai (AGENT, Sample Estates) sees his own, not his colleague's", identity.SeedAgentEmail, []int{1}},
		{"Chipo (AGENT, Sample Estates) sees hers, not her colleague's", devseed.EstatesAgent2Email, []int{7}},
		{"Rudo (OWNER, Sample Estates) sees all of the agency's", devseed.EstatesOwnerEmail, []int{1, 7}},
		{"Farai (OWNER, Sample Commercial) sees none of Sample Estates'", devseed.CommercialAgentEmail, []int{3}},
		{"Nomsa (OWNER, Sample Land Agents) sees nothing", devseed.LandAgentEmail, nil},
		{"Tatenda (OWNER, Sample Realty) sees nothing", devseed.RealtyAgentEmail, nil},
		{"Sipho (OWNER, Sample Homes Bulawayo) sees nothing", devseed.BulawayoAgentEmail, nil},
		{"Blake (AGENT, Second Street Realty) sees nothing", identity.SeedAgentBEmail, nil},
		{"Grace (direct seller) sees her listing's request only", devseed.SellerEmail, []int{6}},
	}
	for _, tt := range tests {
		t.Run(tt.who, func(t *testing.T) {
			if got := e.inboxListings(tt.email); !sameInts(got, tt.want) {
				t.Fatalf("inbox listings = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("consumer is 403, anonymous is 401", func(t *testing.T) {
		wantError(t, e.get("/v1/agent/property-requests", withBearer(e.loginBearer(identity.SeedConsumerEmail, seedPassword))),
			http.StatusForbidden, httpx.CodeForbidden)
		wantError(t, e.get("/v1/agent/property-requests"), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	})

	t.Run("Grace's inbox item carries the usual shape", func(t *testing.T) {
		rec := e.get("/v1/agent/property-requests", withBearer(e.loginBearer(devseed.SellerEmail, seedPassword)))
		inbox := decode[inboxBody](t, rec)
		if len(inbox.Items) != 1 || inbox.Items[0].Status != "RECEIVED" || inbox.Items[0].Contact.Email != "tariro@example.com" ||
			!strings.HasPrefix(inbox.Items[0].Reference, "RP-") {
			t.Fatalf("unexpected inbox: %s", rec.Body)
		}
	})

	t.Run("Grace's /v1/me", func(t *testing.T) {
		user, _ := e.me(e.loginCookie(devseed.SellerEmail, seedPassword))
		if user.Name != "Grace Mhlanga" || len(user.Memberships) != 0 || len(user.AccountRoles) != 1 ||
			user.AccountRoles[0] != (accountRoleBody{"PRIVATE_SELLER", "ACTIVE"}) {
			t.Fatalf("unexpected user: %+v", user)
		}
	})
}

// The PRIVATE_SELLER account role may open the inbox, but never widens what
// it contains; no other account role opens it at all.
func TestSellerInboxAccess(t *testing.T) {
	e := newEnv(t)
	for _, n := range []int{1, 6, 7} {
		wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(n)), http.StatusCreated)
	}

	t.Run("self-registered PENDING PRIVATE_SELLER gets 200 with zero items", func(t *testing.T) {
		c := e.selfRegister("newseller@example.com", "PRIVATE_SELLER")
		user, _ := e.me(c)
		if len(user.AccountRoles) != 1 || user.AccountRoles[0] != (accountRoleBody{"PRIVATE_SELLER", "PENDING_VERIFICATION"}) {
			t.Fatalf("account_roles = %+v", user.AccountRoles)
		}
		rec := e.get("/v1/agent/property-requests", withCookie(c))
		wantStatus(t, rec, http.StatusOK)
		if got := strings.TrimSpace(rec.Body.String()); !strings.HasPrefix(got, `{"items":[],"page":1,"page_size":20,"total":0,"total_pages":0,`) {
			t.Fatalf("body = %s, want an empty items array", got)
		}
	})

	t.Run("every other self-declared role is still 403", func(t *testing.T) {
		for i, role := range identity.SelectableAccountRoles() {
			if role == "PRIVATE_SELLER" || role == "LANDLORD" { // the two direct-owner roles
				continue
			}
			c := e.selfRegister(fmt.Sprintf("declared%d@example.com", i), role)
			wantError(t, e.get("/v1/agent/property-requests", withCookie(c)), http.StatusForbidden, httpx.CodeForbidden)
		}
	})

	t.Run("all professional roles together, without a direct-owner role, are 403", func(t *testing.T) {
		c := e.selfRegister("allpro@example.com", "AGENT", "SALES_AGENT", "LETTINGS_AGENT", "PROPERTY_MANAGER", "SOLICITOR", "AGENCY_OWNER", "AGENCY_ADMIN", "AGENCY_STAFF", "HOST", "DEVELOPER", "SERVICE_PROVIDER")
		wantError(t, e.get("/v1/agent/property-requests", withCookie(c)), http.StatusForbidden, httpx.CodeForbidden)
	})

	// A landlord is an individual owner, like the property seller: the inbox
	// opens (empty), and one person may hold both roles.
	t.Run("a landlord, and a landlord who is also a property seller, may open an empty inbox", func(t *testing.T) {
		for i, roles := range [][]string{{"LANDLORD"}, {"LANDLORD", "PRIVATE_SELLER"}} {
			c := e.selfRegister(fmt.Sprintf("owner%d@example.com", i), roles...)
			rec := e.get("/v1/agent/property-requests", withCookie(c))
			wantStatus(t, rec, http.StatusOK)
			if got := strings.TrimSpace(rec.Body.String()); !strings.HasPrefix(got, `{"items":[],"page":1,"page_size":20,"total":0,"total_pages":0,`) {
				t.Fatalf("%v: body = %s, want an empty items array", roles, got)
			}
			user, _ := e.me(c)
			if len(user.AccountRoles) != len(roles) || len(user.Memberships) != 0 {
				t.Fatalf("%v: roles %+v memberships %+v", roles, user.AccountRoles, user.Memberships)
			}
			for _, r := range user.AccountRoles {
				if r.Status != "PENDING_VERIFICATION" {
					t.Fatalf("%v: %+v", roles, r)
				}
			}
			// The other inbox stays closed: it needs a membership.
			wantError(t, e.get("/v1/inbox/business-enquiries", withCookie(c)), http.StatusForbidden, httpx.CodeForbidden)
		}
	})

	t.Run("a direct seller without the account role sees their own listing only", func(t *testing.T) {
		c := e.selfRegister("quiet-seller@example.com", "CONSUMER")
		user, _ := e.me(c)
		wantError(t, e.get("/v1/agent/property-requests", withCookie(c)), http.StatusForbidden, httpx.CodeForbidden)

		seller := uuid.MustParse(user.ID)
		e.requests.PutListing(propertyrequests.Listing{
			ID: propertyrequests.SeedListingID(40), MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
			SellerUserID: &seller, SellerEligible: true,
		})
		rec := e.get("/v1/agent/property-requests", withCookie(c))
		wantStatus(t, rec, http.StatusOK)
		if len(decode[inboxBody](t, rec).Items) != 0 {
			t.Fatalf("no request was made for their listing yet: %s", rec.Body)
		}
		wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(40)), http.StatusCreated)
		inbox := decode[inboxBody](t, e.get("/v1/agent/property-requests", withCookie(c)))
		if len(inbox.Items) != 1 || inbox.Items[0].ListingID != propertyrequests.SeedListingID(40).String() {
			t.Fatalf("inbox = %+v", inbox.Items)
		}
	})

	t.Run("a member who is also a direct seller gets the union", func(t *testing.T) {
		tendai := identity.SeedAgentUserID
		e.requests.PutListing(propertyrequests.Listing{
			ID: propertyrequests.SeedListingID(41), MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
			SellerUserID: &tendai, SellerEligible: true,
		})
		wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(41)), http.StatusCreated)
		if got := e.inboxListings(identity.SeedAgentEmail); !sameInts(got, []int{1, 41}) {
			t.Fatalf("Tendai's inbox = %v, want [1 41]", got)
		}
		// Nobody else gains anything from it.
		if got := e.inboxListings(devseed.EstatesAgent2Email); !sameInts(got, []int{7}) {
			t.Fatalf("Chipo's inbox = %v, want [7]", got)
		}
		if got := e.inboxListings(devseed.EstatesOwnerEmail); !sameInts(got, []int{1, 7}) {
			t.Fatalf("Rudo's inbox = %v, want [1 7]", got)
		}
		if got := e.inboxListings(devseed.SellerEmail); !sameInts(got, []int{6}) {
			t.Fatalf("Grace's inbox = %v, want [6]", got)
		}
	})
}

// Every one of the 18 sample listings accepts a request and routes it to the
// person the sample data names.
func TestEverySampleListingRoutesToItsOwner(t *testing.T) {
	e := newEnv(t)
	want := map[int]string{}
	for _, a := range devseed.Accounts() {
		for _, n := range a.Listings {
			want[n] = a.Email
		}
	}
	total := devseed.ListingCount + devseed.BackendListingCount
	if len(want) != total {
		t.Fatalf("%d listings are assigned, want %d", len(want), total)
	}
	for n := 1; n <= total; n++ {
		// A distinct client IP per request keeps this clear of the rate limit.
		wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(n), withIP(fmt.Sprintf("198.51.100.%d", n))), http.StatusCreated)
	}
	byEmail := map[string][]int{}
	for n, email := range want {
		byEmail[email] = append(byEmail[email], n)
	}
	// Rudo owns the agency: he sees Tendai's and Chipo's.
	byEmail[devseed.EstatesOwnerEmail] = []int{1, 2, 5, 7, 10, 17}
	for email, listings := range byEmail {
		sort.Ints(listings)
		if got := e.inboxListings(email); !sameInts(got, listings) {
			t.Fatalf("%s inbox = %v, want %v", email, got, listings)
		}
	}
	for _, req := range e.requests.Requests() {
		if req.Status != propertyrequests.StatusReceived || req.Recipient == nil || req.Recipient.UserID == nil {
			t.Fatalf("listing %s was not routed to a person: %+v", req.ListingID, req.Recipient)
		}
	}
	// The expired sample listing still refuses requests.
	body := e.propertyRequest(1)
	body["listing_id"] = devseed.ExpiredListingID.String()
	wantError(t, e.post("/v1/property-requests", body, withIP("198.51.100.99")), http.StatusGone, propertyrequests.CodeListingUnavailable)
}

// ---------------------------------------------------------------------------
// Outbox events behind the account notification emails
// ---------------------------------------------------------------------------

func (e *env) identityEvents() []string {
	var types []string
	for _, ev := range e.identity.Outbox() {
		types = append(types, ev.Type)
	}
	return types
}

func TestIdentityOutboxEvents(t *testing.T) {
	e := newEnv(t)
	if n := len(e.identity.Outbox()); n != 0 {
		t.Fatalf("seeding wrote %d outbox events", n)
	}

	// Registration and a customer sign-in write nothing; verification writes the welcome event once.
	cookie := e.selfRegister("events@example.com", "LANDLORD")
	e.loginCookie(identity.SeedConsumerEmail, seedPassword)
	if got := e.identityEvents(); len(got) != 1 || got[0] != identity.EventUserEmailVerified {
		t.Fatalf("events after verification = %v, want [USER_EMAIL_VERIFIED]", got)
	}
	user, _ := e.me(cookie)
	first := e.identity.Outbox()[0]
	if first.AggregateID.String() != user.ID || first.AggregateType != identity.AggregateUser {
		t.Fatalf("unexpected event: %+v", first)
	}

	// A password reset writes PASSWORD_CHANGED, and verifies nothing new.
	wantStatus(t, e.post("/v1/auth/password-reset/request", obj{"email": "events@example.com"}), http.StatusAccepted)
	wantStatus(t, e.post("/v1/auth/password-reset/confirm", obj{"token": e.mail.resetToken(t), "password": newPassword}), http.StatusNoContent)
	// An administrator sign-in writes ADMIN_SIGNED_IN; a refused one does not.
	wantError(t, e.post("/v1/admin/auth/login", obj{"email": identity.SeedAdminEmail, "password": "definitely-wrong-pw"}), http.StatusUnauthorized, identity.CodeInvalidCredentials)
	e.adminLogin(identity.SeedAdminEmail, seedPassword)

	got := e.identityEvents()
	want := []string{identity.EventUserEmailVerified, identity.EventPasswordChanged, identity.EventAdminSignedIn}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if admin := e.identity.Outbox()[2]; admin.AggregateID != identity.SeedAdminUserID {
		t.Fatalf("admin event: %+v", admin)
	}

	// Payloads carry an id and a time: no address, name, token or password.
	for _, ev := range e.identity.Outbox() {
		payload := string(ev.Payload)
		for _, private := range []string{"@", "events", "Rudo", newPassword, seedPassword, "token"} {
			if strings.Contains(payload, private) {
				t.Fatalf("%s payload contains %q: %s", ev.Type, private, payload)
			}
		}
		if !strings.Contains(payload, `"user_id":"`+ev.AggregateID.String()+`"`) || !strings.Contains(payload, `"occurred_at":"`) {
			t.Fatalf("%s payload = %s", ev.Type, payload)
		}
	}
	// The verification and reset tokens never reach any outbox.
	for _, link := range append(append([]string{}, e.mail.verifications...), e.mail.resets...) {
		token := link[strings.Index(link, "token=")+len("token="):]
		for _, ev := range e.identity.Outbox() {
			if strings.Contains(string(ev.Payload), token) {
				t.Fatal("a secret token was written to the outbox")
			}
		}
		if strings.Contains(e.logs.String(), token) {
			t.Fatal("a secret token was logged")
		}
	}
}
