package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/health"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/logging"
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
}

func (m *captureMailer) SendVerification(_ context.Context, _, _, link string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.verifications = append(m.verifications, link)
	return nil
}

func (m *captureMailer) SendPasswordReset(_ context.Context, _, _, link string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resets = append(m.resets, link)
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
	t        *testing.T
	handler  http.Handler
	identity *identity.MemoryStore
	requests *propertyrequests.MemoryStore
	mail     *captureMailer
	logs     *bytes.Buffer
	now      time.Time
	seq      int
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
		t:        t,
		identity: identity.NewMemoryStore(),
		requests: propertyrequests.NewMemoryStore(),
		mail:     &captureMailer{},
		logs:     &bytes.Buffer{},
		now:      time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC),
	}
	hasher := identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1}
	if err := e.identity.SeedDevelopment(context.Background(), hasher, seedPassword, propertyrequests.SeedOrganisationID, e.now); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	e.handler, err = NewRouter(Deps{
		Config:   cfg,
		Logger:   logging.New(e.logs, slog.LevelInfo, "api", cfg.Env),
		Health:   health.NewHandler(health.StoreMemory, nil, logger),
		Markets:  markets.NewMemoryStore(),
		Requests: e.requests,
		Identity: e.identity,
		Mailer:   e.mail,
		Now:      func() time.Time { return e.now },
		Hasher:   &hasher,
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

func (e *env) do(method, path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
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
	return rec
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
}

type authBody struct {
	User    userBody `json:"user"`
	Session struct {
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"session"`
	AccessToken string `json:"access_token"`
}

type inboxBody struct {
	Items []struct {
		RequestID              string    `json:"request_id"`
		Reference              string    `json:"reference"`
		ListingID              string    `json:"listing_id"`
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
		want := `{"user":{"id":"` + identity.SeedConsumerUserID.String() + `","name":"Casey Consumer","email":"consumer@example.test","email_verified":true,"platform_roles":[],"memberships":[]}}`
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Fatalf("body = %s\nwant   %s", got, want)
		}
	})

	t.Run("agent", func(t *testing.T) {
		rec := e.get("/v1/me", withCookie(e.loginCookie(identity.SeedAgentEmail, seedPassword)))
		wantStatus(t, rec, http.StatusOK)
		want := `"platform_roles":[],"memberships":[{"organisation_id":"` + propertyrequests.SeedOrganisationID.String() +
			`","organisation_name":"Sample Estates","organisation_kind":"AGENCY","market_code":"ZW","role":"AGENT"}]`
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body = %s\nwant to contain %s", rec.Body, want)
		}
	})

	t.Run("admin", func(t *testing.T) {
		rec := e.get("/v1/me", withCookie(e.loginCookie(identity.SeedAdminEmail, seedPassword)))
		wantStatus(t, rec, http.StatusOK)
		if want := `"platform_roles":[{"role":"SUPER_ADMIN","market_code":null}],"memberships":[]`; !strings.Contains(rec.Body.String(), want) {
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
		wantError(t, e.get("/v1/agent/property-requests", withBearer(e.loginBearer(identity.SeedAdminEmail, seedPassword))),
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
		if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[]}` {
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

	e.requests.PutListing(propertyrequests.Listing{
		ID: propertyrequests.SeedListingID(7), MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
		OrganisationID: &orgA, OrganisationEligible: true, AgentUserID: &otherAgent, AgentEligible: true,
	})
	// A listing nobody can receive: stored as ROUTING_EXCEPTION, in no inbox.
	e.requests.PutListing(propertyrequests.Listing{
		ID: propertyrequests.SeedListingID(8), MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
		OrganisationID: &orgA, OrganisationEligible: false,
	})

	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(1)), http.StatusCreated) // organisation inbox
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(7)), http.StatusCreated) // assigned to other-agent
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(8)), http.StatusCreated) // routing exception
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(6)), http.StatusCreated) // direct seller

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
	l1, l7 := propertyrequests.SeedListingID(1).String(), propertyrequests.SeedListingID(7).String()

	tests := []struct {
		email string
		want  []string
	}{
		{"owner@example.com", []string{l1, l7}},
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
