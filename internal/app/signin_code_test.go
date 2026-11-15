package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// newCodeEnv is a test environment in which logins are NOT completed by the
// helper, so the code step can be exercised directly.
func newCodeEnv(t *testing.T, mutate func(*config.Config)) *env {
	e := newEnvWithConfig(t, mutate)
	e.twoStepLogin = true
	return e
}

// passwordStep signs in with the right password and returns the challenge.
func (e *env) passwordStep(path, email string, extra obj, opts ...reqOpt) codeChallengeBody {
	e.t.Helper()
	body := obj{"email": email, "password": seedPassword}
	for k, v := range extra {
		body[k] = v
	}
	rec := e.post(path, body, opts...)
	wantStatus(e.t, rec, http.StatusOK)
	if sessionCookie(rec) != nil || strings.Contains(rec.Body.String(), "access_token") || strings.Contains(rec.Body.String(), `"user"`) {
		e.t.Fatalf("the password step must not start a session: %s", rec.Body)
	}
	return decode[codeChallengeBody](e.t, rec)
}

func (e *env) verify(path, challengeID string, code any, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.post(path+"/verify", obj{"challenge_id": challengeID, "code": code}, opts...)
}

// wrongCode is a six-digit code that is not the one sent.
func wrongCode(right string) string {
	if right == "000000" {
		return "111111"
	}
	return "000000"
}

const (
	customerLogin = "/v1/auth/login"
	adminLogin    = "/v1/admin/auth/login"
)

func TestSignInCodeCustomerHappyPath(t *testing.T) {
	e := newCodeEnv(t, nil)
	c := e.passwordStep(customerLogin, identity.SeedConsumerEmail, nil)
	if c.Status != "CODE_REQUIRED" || c.CodeLength != 6 || c.SentTo != "c•••@example.test" ||
		!c.ExpiresAt.Equal(e.now.Add(10*time.Minute)) || len(c.ChallengeID) < 40 {
		t.Fatalf("challenge = %+v", c)
	}
	sent := e.mail.lastCode(t)
	if sent.to != identity.SeedConsumerEmail || sent.admin || len(sent.code) != 6 || strings.Trim(sent.code, "0123456789") != "" {
		t.Fatalf("code email = %+v", sent)
	}
	for _, s := range e.identity.Sessions() {
		if s.UserID == identity.SeedConsumerUserID {
			t.Fatal("no session may exist before the code")
		}
	}

	rec := e.verify(customerLogin, c.ChallengeID, sent.code)
	wantStatus(t, rec, http.StatusOK)
	body := decode[authBody](t, rec)
	cookie := sessionCookie(rec)
	if cookie == nil || body.Session == nil || body.Session.Scope != "customer" || body.User.Email != identity.SeedConsumerEmail || body.AccessToken != "" {
		t.Fatalf("verify = %s", rec.Body)
	}
	wantStatus(t, e.get("/v1/me", withCookie(cookie)), http.StatusOK)

	// The mobile app: token_delivery chosen at the password step applies.
	c = e.passwordStep(customerLogin, identity.SeedAgentEmail, obj{"token_delivery": "bearer"})
	rec = e.verify(customerLogin, c.ChallengeID, e.mail.lastCode(t).code)
	wantStatus(t, rec, http.StatusOK)
	token := decode[authBody](t, rec).AccessToken
	if token == "" || sessionCookie(rec) != nil {
		t.Fatalf("bearer verify = %s", rec.Body)
	}
	wantStatus(t, e.get("/v1/me", withBearer(token)), http.StatusOK)

	// The code is never logged.
	if strings.Contains(e.logs.String(), sent.code) {
		t.Fatal("logs contain the sign-in code")
	}
}

func TestSignInCodeAdminHappyPath(t *testing.T) {
	e := newCodeEnv(t, nil)
	c := e.passwordStep(adminLogin, identity.SeedAdminEmail, nil)
	sent := e.mail.lastCode(t)
	if !sent.admin || c.SentTo != "a•••@example.test" {
		t.Fatalf("admin code = %+v, challenge %+v", sent, c)
	}
	rec := e.verify(adminLogin, c.ChallengeID, sent.code)
	wantStatus(t, rec, http.StatusOK)
	body := decode[authBody](t, rec)
	cookie := sessionCookie(rec)
	if cookie == nil || body.Session == nil || body.Session.Scope != "admin" || strings.Contains(rec.Body.String(), "access_token") {
		t.Fatalf("admin verify = %s", rec.Body)
	}
	wantStatus(t, e.get("/v1/admin/me", withCookie(cookie)), http.StatusOK)
	if !strings.Contains(e.logs.String(), `"event":"admin_sign_in","outcome":"code_sent"`) ||
		!strings.Contains(e.logs.String(), `"event":"admin_sign_in","outcome":"success"`) {
		t.Fatal("the admin sign-in is audited at both steps")
	}
}

func TestSignInCodeWrongThenRight(t *testing.T) {
	e := newCodeEnv(t, nil)
	c := e.passwordStep(customerLogin, identity.SeedConsumerEmail, nil)
	right := e.mail.lastCode(t).code

	for _, tt := range []struct {
		code any
		msg  string
	}{
		{"", "Enter the code we sent you."},
		{"   ", "Enter the code we sent you."},
		{"12ab56", "Enter the code we sent you."},
		{nil, "Enter the code we sent you."},
		{"1234", "Enter the code we sent you."}, // four digits: only ever the demo code
		{wrongCode(right), "That code is not right."},
	} {
		env := wantError(t, e.verify(customerLogin, c.ChallengeID, tt.code), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields["code"] != tt.msg {
			t.Fatalf("code %v: fields = %v", tt.code, env.Error.Fields)
		}
	}
	// The right code still works after a wrong one; as a JSON number too.
	var asNumber int
	fmt.Sscan(right, &asNumber)
	var code any = right
	if right[0] != '0' {
		code = asNumber
	}
	wantStatus(t, e.verify(customerLogin, c.ChallengeID, code), http.StatusOK)
}

func TestSignInCodeFiveWrongAttemptsKillTheChallenge(t *testing.T) {
	e := newCodeEnv(t, nil)
	c := e.passwordStep(customerLogin, identity.SeedConsumerEmail, nil)
	right := e.mail.lastCode(t).code
	for i := 1; i <= 4; i++ {
		env := wantError(t, e.verify(customerLogin, c.ChallengeID, wrongCode(right)), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields["code"] != "That code is not right." {
			t.Fatalf("attempt %d: %v", i, env.Error.Fields)
		}
	}
	env := wantError(t, e.verify(customerLogin, c.ChallengeID, wrongCode(right)), http.StatusBadRequest, identity.CodeCodeExpired)
	if env.Error.Message != "That code has expired or was used too many times. Sign in again." {
		t.Fatalf("message = %q", env.Error.Message)
	}
	// Dead: even the right code is refused now, and so is a resend.
	wantError(t, e.verify(customerLogin, c.ChallengeID, right), http.StatusBadRequest, identity.CodeCodeExpired)
	e.now = e.now.Add(time.Minute)
	wantError(t, e.post(customerLogin+"/resend", obj{"challenge_id": c.ChallengeID}), http.StatusBadRequest, identity.CodeCodeExpired)
}

func TestSignInCodeExpiresAndIsSingleUse(t *testing.T) {
	e := newCodeEnv(t, nil)
	c := e.passwordStep(customerLogin, identity.SeedConsumerEmail, nil)
	right := e.mail.lastCode(t).code
	e.now = e.now.Add(10 * time.Minute)
	wantError(t, e.verify(customerLogin, c.ChallengeID, right), http.StatusBadRequest, identity.CodeCodeExpired)

	c = e.passwordStep(customerLogin, identity.SeedConsumerEmail, nil)
	right = e.mail.lastCode(t).code
	e.now = e.now.Add(9*time.Minute + 59*time.Second)
	wantStatus(t, e.verify(customerLogin, c.ChallengeID, right), http.StatusOK)
	// Reuse is refused.
	rec := e.verify(customerLogin, c.ChallengeID, right)
	wantError(t, rec, http.StatusBadRequest, identity.CodeCodeExpired)
	if sessionCookie(rec) != nil {
		t.Fatal("a reused code must not start a session")
	}
	// An unknown or malformed challenge is the same answer.
	wantError(t, e.verify(customerLogin, "not-a-challenge", right), http.StatusBadRequest, identity.CodeCodeExpired)
	wantError(t, e.verify(customerLogin, strings.Repeat("A", len(c.ChallengeID)), right), http.StatusBadRequest, identity.CodeCodeExpired)
	// Expired challenges are cleaned up when a new one starts.
	e.now = e.now.Add(time.Hour)
	e.passwordStep(customerLogin, identity.SeedConsumerEmail, nil)
	if n := e.identity.LoginChallengeCount(); n != 1 {
		t.Fatalf("%d challenges kept, want only the new one", n)
	}
}

func TestSignInCodeScopesAreSeparate(t *testing.T) {
	e := newCodeEnv(t, nil)
	customer := e.passwordStep(customerLogin, identity.SeedConsumerEmail, nil)
	customerCode := e.mail.lastCode(t).code
	admin := e.passwordStep(adminLogin, identity.SeedAdminEmail, nil)
	adminCode := e.mail.lastCode(t).code

	wantError(t, e.verify(adminLogin, customer.ChallengeID, customerCode), http.StatusBadRequest, identity.CodeCodeExpired)
	wantError(t, e.verify(customerLogin, admin.ChallengeID, adminCode), http.StatusBadRequest, identity.CodeCodeExpired)
	wantError(t, e.post(adminLogin+"/resend", obj{"challenge_id": customer.ChallengeID}), http.StatusBadRequest, identity.CodeCodeExpired)
	// Neither attempt used anything up: each works at its own endpoint.
	wantStatus(t, e.verify(customerLogin, customer.ChallengeID, customerCode), http.StatusOK)
	wantStatus(t, e.verify(adminLogin, admin.ChallengeID, adminCode), http.StatusOK)
}

func TestSignInCodeResend(t *testing.T) {
	e := newCodeEnv(t, nil)
	c := e.passwordStep(customerLogin, identity.SeedConsumerEmail, nil)
	first := e.mail.lastCode(t).code

	rec := e.post(customerLogin+"/resend", obj{"challenge_id": c.ChallengeID})
	wantError(t, rec, http.StatusTooManyRequests, httpx.CodeRateLimited)
	if rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	e.now = e.now.Add(20 * time.Second)
	if rec := e.post(customerLogin+"/resend", obj{"challenge_id": c.ChallengeID}); rec.Header().Get("Retry-After") != "10" {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}

	var codes []string
	for i := 1; i <= identity.SignInCodeMaxResends; i++ {
		e.now = e.now.Add(30 * time.Second)
		rec := e.post(customerLogin+"/resend", obj{"challenge_id": c.ChallengeID})
		wantStatus(t, rec, http.StatusOK)
		again := decode[codeChallengeBody](t, rec)
		if again.Status != "CODE_REQUIRED" || again.ChallengeID != c.ChallengeID || again.SentTo != c.SentTo ||
			again.CodeLength != 6 || !again.ExpiresAt.Equal(e.now.Add(10*time.Minute)) {
			t.Fatalf("resend %d = %+v", i, again)
		}
		codes = append(codes, e.mail.lastCode(t).code)
	}
	if e.mail.codeCount() != 1+identity.SignInCodeMaxResends {
		t.Fatalf("%d codes sent", e.mail.codeCount())
	}
	e.now = e.now.Add(time.Minute)
	wantError(t, e.post(customerLogin+"/resend", obj{"challenge_id": c.ChallengeID}), http.StatusTooManyRequests, httpx.CodeRateLimited)

	latest := codes[len(codes)-1]
	if first != latest {
		// An earlier code stops working once a new one is sent.
		wantError(t, e.verify(customerLogin, c.ChallengeID, first), http.StatusBadRequest, httpx.CodeValidationFailed)
	}
	wantStatus(t, e.verify(customerLogin, c.ChallengeID, latest), http.StatusOK)
}

// The demo code (AUTH_DEMO_CODE) works for any challenge in staging, and is
// ignored if a production configuration ever carried one.
func TestSignInDemoCode(t *testing.T) {
	demo := "12" + "34"
	e := newCodeEnv(t, func(c *config.Config) { c.Env, c.AuthDemoCode = config.EnvStaging, config.Secret(demo) })
	c := e.passwordStep(customerLogin, identity.SeedConsumerEmail, nil)
	wantStatus(t, e.verify(customerLogin, c.ChallengeID, demo), http.StatusOK)
	wantError(t, e.verify(customerLogin, c.ChallengeID, demo), http.StatusBadRequest, identity.CodeCodeExpired) // still single use
	a := e.passwordStep(adminLogin, identity.SeedAdminEmail, nil)
	wantStatus(t, e.verify(adminLogin, a.ChallengeID, demo), http.StatusOK)
	// Another 4-digit value is not a code.
	c = e.passwordStep(customerLogin, identity.SeedAgentEmail, nil)
	env := wantError(t, e.verify(customerLogin, c.ChallengeID, "4321"), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["code"] != "Enter the code we sent you." {
		t.Fatalf("fields = %v", env.Error.Fields)
	}
	// The real code still works next to the demo code.
	wantStatus(t, e.verify(customerLogin, c.ChallengeID, e.mail.lastCode(t).code), http.StatusOK)
	if strings.Contains(e.logs.String(), `"code":"`+demo+`"`) {
		t.Fatal("the demo code must never be logged")
	}

	prod := newCodeEnv(t, func(c *config.Config) { c.Env, c.AuthDemoCode = config.EnvProduction, config.Secret(demo) })
	c = prod.passwordStep(customerLogin, identity.SeedConsumerEmail, nil)
	wantError(t, prod.verify(customerLogin, c.ChallengeID, demo), http.StatusBadRequest, httpx.CodeValidationFailed)
}

// A wrong password (or unknown email) is answered exactly as before the code
// step existed, and starts no challenge and sends no code.
func TestSignInWrongPasswordUnchanged(t *testing.T) {
	e := newCodeEnv(t, nil)
	const rid = "fixed-request-id"
	want := `{"error":{"code":"INVALID_CREDENTIALS","message":"The email or password is incorrect."},"trace_id":"` + rid + `"}`
	for _, tt := range []struct{ path, email, password string }{
		{customerLogin, identity.SeedConsumerEmail, "definitely-wrong-pw"},
		{customerLogin, "nobody@example.com", "definitely-wrong-pw"},
		{adminLogin, identity.SeedAdminEmail, "definitely-wrong-pw"},
		{adminLogin, identity.SeedConsumerEmail, seedPassword},
	} {
		rec := e.post(tt.path, obj{"email": tt.email, "password": tt.password}, withHeader(httpx.HeaderRequestID, rid))
		wantStatus(t, rec, http.StatusUnauthorized)
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Fatalf("%s %s: body = %s", tt.path, tt.email, got)
		}
	}
	if e.mail.codeCount() != 0 || e.identity.LoginChallengeCount() != 0 {
		t.Fatal("a refused password must not start a challenge or send a code")
	}
}
