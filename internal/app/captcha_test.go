package app

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// The messages of the "captcha" field, as the clients show them.
const (
	captchaRequired = "Answer the question to show you are a person."
	captchaWrong    = "That answer is not right. Try the new question."
	captchaExpired  = "That question has expired. Try the new question."
)

var captchaQuestionRe = regexp.MustCompile(`^What is (\d{1,2}) ([+-]) (\d{1,2})\?$`)

// wantCaptchaError checks a refused registration: 400 VALIDATION_FAILED with
// only the "captcha" field, set to msg.
func wantCaptchaError(t *testing.T, e *env, body obj, msg string) {
	t.Helper()
	env := wantError(t, e.post("/v1/auth/register", body), http.StatusBadRequest, httpx.CodeValidationFailed)
	if got := env.Error.Fields["captcha"]; got != msg || len(env.Error.Fields) != 1 {
		t.Fatalf("fields = %v, want only captcha=%q", env.Error.Fields, msg)
	}
}

// answered returns the "captcha" object for a question with the given answer.
func answered(c captchaBody, answer string) obj {
	return obj{"captcha_id": c.CaptchaID, "answer": answer}
}

func TestCaptchaEndpoint(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/v1/auth/captcha")
	wantStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}
	// Exactly these three keys, and nothing that gives the answer away.
	raw := decode[map[string]any](t, rec)
	if len(raw) != 3 || raw["captcha_id"] == nil || raw["question"] == nil || raw["expires_at"] == nil {
		t.Fatalf("body = %s", rec.Body)
	}
	c := decode[captchaBody](t, rec)
	if len(c.CaptchaID) != 43 {
		t.Fatalf("captcha_id = %q, want an opaque 43-character id", c.CaptchaID)
	}
	if want := e.now.Add(10 * time.Minute); !c.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %s, want %s", c.ExpiresAt, want)
	}
	if !strings.Contains(rec.Body.String(), `"expires_at":"2026-11-01T09:10:00Z"`) {
		t.Fatalf("expires_at is not RFC 3339: %s", rec.Body)
	}

	// Many questions: every one is plain arithmetic with small numbers and a
	// result that is never negative, both operations appear, no id repeats.
	ids, ops := map[string]bool{c.CaptchaID: true}, map[string]bool{}
	for i := range 200 {
		q := e.newCaptcha()
		m := captchaQuestionRe.FindStringSubmatch(q.Question)
		if m == nil {
			t.Fatalf("question %d = %q", i, q.Question)
		}
		var a, b int
		fmt.Sscan(m[1], &a)
		fmt.Sscan(m[3], &b)
		if a < 1 || a > 10 || b < 1 || b > 10 {
			t.Fatalf("question %q uses numbers outside 1 to 10", q.Question)
		}
		if answer := solveCaptcha(t, q.Question); answer < 0 || answer > 20 {
			t.Fatalf("question %q has the answer %d", q.Question, answer)
		}
		if ids[q.CaptchaID] {
			t.Fatalf("captcha id %q was issued twice", q.CaptchaID)
		}
		ids[q.CaptchaID], ops[m[2]] = true, true
	}
	if !ops["+"] || !ops["-"] {
		t.Fatalf("200 questions used only %v", ops)
	}

	// The endpoint only answers GET, and needs no session and no Origin.
	wantStatus(t, e.post("/v1/auth/captcha", obj{}), http.StatusMethodNotAllowed)
}

func TestRegisterWithCaptcha(t *testing.T) {
	t.Run("the right answer registers the account", func(t *testing.T) {
		e := newEnv(t)
		before := e.identity.UserCount()
		c := e.newCaptcha()
		body := registerBody("person@example.com")
		body["captcha"] = answered(c, fmt.Sprint(solveCaptcha(t, c.Question)))
		rec := e.post("/v1/auth/register", body)
		wantStatus(t, rec, http.StatusAccepted)
		if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"VERIFICATION_SENT"}` {
			t.Fatalf("body = %s", got)
		}
		if v, _, _ := e.mail.counts(); v != 1 || e.identity.UserCount() != before+1 {
			t.Fatalf("verifications = %d, users = %d (before %d)", v, e.identity.UserCount(), before)
		}
	})

	t.Run("spaces around the answer and leading zeros are fine", func(t *testing.T) {
		e := newEnv(t)
		for i, format := range []string{"  %d  ", "0%d", "\t%d\n"} {
			c := e.newCaptcha()
			body := registerBody(fmt.Sprintf("spaced%d@example.com", i))
			body["captcha"] = answered(c, fmt.Sprintf(format, solveCaptcha(t, c.Question)))
			wantStatus(t, e.post("/v1/auth/register", body), http.StatusAccepted)
		}
	})

	t.Run("missing", func(t *testing.T) {
		e := newEnv(t)
		before := e.identity.UserCount()
		c := e.newCaptcha()
		for name, captcha := range map[string]any{
			"no captcha":     nil,
			"empty object":   obj{},
			"no answer":      obj{"captcha_id": c.CaptchaID},
			"blank answer":   answered(c, "   "),
			"no captcha id":  obj{"answer": "7"},
			"blank id":       obj{"captcha_id": " ", "answer": "7"},
			"both are blank": obj{"captcha_id": "", "answer": ""},
		} {
			body := registerBody("missing@example.com")
			body["captcha"] = captcha
			env := wantError(t, e.post("/v1/auth/register", body), http.StatusBadRequest, httpx.CodeValidationFailed)
			if got := env.Error.Fields["captcha"]; got != captchaRequired || len(env.Error.Fields) != 1 {
				t.Fatalf("%s: fields = %v, want only captcha=%q", name, env.Error.Fields, captchaRequired)
			}
		}
		// The field is reported with the others, not instead of them.
		body := registerBody("not-an-email")
		body["captcha"] = nil
		env := wantError(t, e.post("/v1/auth/register", body), http.StatusBadRequest, httpx.CodeValidationFailed)
		if len(env.Error.Fields) != 2 || env.Error.Fields["captcha"] != captchaRequired || env.Error.Fields["email"] == "" {
			t.Fatalf("fields = %v, want email and captcha", env.Error.Fields)
		}
		if v, _, _ := e.mail.counts(); v != 0 || e.identity.UserCount() != before {
			t.Fatal("a registration without a captcha created an account or sent an email")
		}
		// A blank answer is not a guess: the question can still be answered.
		body = registerBody("missing@example.com")
		body["captcha"] = answered(c, fmt.Sprint(solveCaptcha(t, c.Question)))
		wantStatus(t, e.post("/v1/auth/register", body), http.StatusAccepted)
	})

	t.Run("a wrong answer is refused and uses the captcha up", func(t *testing.T) {
		e := newEnv(t)
		before := e.identity.UserCount()
		c := e.newCaptcha()
		right := solveCaptcha(t, c.Question)

		body := registerBody("guesser@example.com")
		body["captcha"] = answered(c, fmt.Sprint(right+1))
		wantCaptchaError(t, e, body, captchaWrong)
		if v, _, _ := e.mail.counts(); v != 0 || e.identity.UserCount() != before {
			t.Fatal("a wrong answer created an account or sent an email")
		}

		// The right answer to the same question comes too late.
		body["captcha"] = answered(c, fmt.Sprint(right))
		wantCaptchaError(t, e, body, captchaExpired)
		if e.identity.UserCount() != before {
			t.Fatal("a used captcha created an account")
		}

		// A new question works.
		body["captcha"] = e.solvedCaptcha()
		wantStatus(t, e.post("/v1/auth/register", body), http.StatusAccepted)
	})

	t.Run("only digits are an answer", func(t *testing.T) {
		e := newEnv(t)
		before := e.identity.UserCount()
		for _, answer := range []string{"seven", "7.0", "-7", "+7", "7 0", "0x7", "７", "99999"} {
			c := e.newCaptcha()
			body := registerBody("words@example.com")
			body["captcha"] = answered(c, answer)
			wantCaptchaError(t, e, body, captchaWrong)
		}
		if e.identity.UserCount() != before {
			t.Fatal("an answer that is not a number created an account")
		}
		// The answer is a string in the contract: a JSON number is not the shape.
		c := e.newCaptcha()
		body := registerBody("number@example.com")
		body["captcha"] = obj{"captcha_id": c.CaptchaID, "answer": solveCaptcha(t, c.Question)}
		wantError(t, e.post("/v1/auth/register", body), http.StatusBadRequest, httpx.CodeInvalidJSON)
	})

	t.Run("a captcha registers one account only", func(t *testing.T) {
		e := newEnv(t)
		captcha := e.solvedCaptcha()
		first := registerBody("first@example.com")
		first["captcha"] = captcha
		wantStatus(t, e.post("/v1/auth/register", first), http.StatusAccepted)
		count := e.identity.UserCount()

		second := registerBody("second@example.com")
		second["captcha"] = captcha
		wantCaptchaError(t, e, second, captchaExpired)
		// Not for the same email either.
		wantCaptchaError(t, e, first, captchaExpired)
		if v, _, _ := e.mail.counts(); v != 1 || e.identity.UserCount() != count {
			t.Fatal("a used captcha created an account or sent an email")
		}
	})

	t.Run("an unknown captcha", func(t *testing.T) {
		e := newEnv(t)
		for _, id := range []string{"nope", strings.Repeat("A", 43), strings.Repeat("x", 500)} {
			body := registerBody("unknown@example.com")
			body["captcha"] = obj{"captcha_id": id, "answer": "7"}
			wantCaptchaError(t, e, body, captchaExpired)
		}
	})

	t.Run("expired after ten minutes", func(t *testing.T) {
		e := newEnv(t)
		before := e.identity.UserCount()
		inTime, late := e.solvedCaptcha(), e.solvedCaptcha()

		e.now = e.now.Add(10*time.Minute - time.Second)
		body := registerBody("in-time@example.com")
		body["captcha"] = inTime
		wantStatus(t, e.post("/v1/auth/register", body), http.StatusAccepted)

		e.now = e.now.Add(time.Second)
		body = registerBody("late@example.com")
		body["captcha"] = late
		wantCaptchaError(t, e, body, captchaExpired)
		if e.identity.UserCount() != before+1 {
			t.Fatal("an expired captcha created an account")
		}
	})

	t.Run("a mistake in another field does not use the captcha up", func(t *testing.T) {
		e := newEnv(t)
		captcha := e.solvedCaptcha()
		for field, mutate := range map[string]func(obj){
			"email":            func(b obj) { b["email"] = "not-an-email" },
			"password":         func(b obj) { b["password"] = "short" },
			"name":             func(b obj) { b["name"] = "" },
			"consent.accepted": func(b obj) { delete(b, "consent") },
			"roles":            func(b obj) { b["roles"] = []string{"CONSUMER", "AGENT"} },
		} {
			body := registerBody("typo@example.com")
			body["captcha"] = captcha
			mutate(body)
			env := wantError(t, e.post("/v1/auth/register", body), http.StatusBadRequest, httpx.CodeValidationFailed)
			if _, ok := env.Error.Fields[field]; !ok || len(env.Error.Fields) != 1 {
				t.Fatalf("fields = %v, want only %q", env.Error.Fields, field)
			}
		}
		body := registerBody("typo@example.com")
		body["captcha"] = captcha
		wantStatus(t, e.post("/v1/auth/register", body), http.StatusAccepted)
	})
}

// The captcha is judged before the account is looked up, so the answer to a
// registration does not depend on whether the email already has an account.
func TestRegisterCaptchaDoesNotRevealAccounts(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.post("/v1/auth/register", registerBody("pending@example.com")), http.StatusAccepted)
	before := e.identity.UserCount()
	v0, _, x0 := e.mail.counts()

	const rid = "fixed-request-id"
	emails := []string{"brand-new@example.com", identity.SeedConsumerEmail, "pending@example.com"}

	// A wrong answer and a used captcha: the same refusal for every email, and
	// nothing is written or sent, not even the "you already have an account" email.
	for _, kind := range []string{"wrong", "used"} {
		var bodies []string
		for _, email := range emails {
			c := e.newCaptcha()
			answer := solveCaptcha(t, c.Question)
			body := registerBody(email)
			switch kind {
			case "wrong":
				body["captcha"] = answered(c, fmt.Sprint(answer+3))
			case "used":
				spent := registerBody("spender-" + email)
				spent["captcha"] = answered(c, fmt.Sprint(answer+3))
				wantCaptchaError(t, e, spent, captchaWrong)
				body["captcha"] = answered(c, fmt.Sprint(answer))
			}
			rec := e.post("/v1/auth/register", body, withHeader(httpx.HeaderRequestID, rid))
			wantError(t, rec, http.StatusBadRequest, httpx.CodeValidationFailed)
			bodies = append(bodies, rec.Body.String())
		}
		if bodies[0] != bodies[1] || bodies[0] != bodies[2] {
			t.Fatalf("%s captcha: the refusals differ:\n%s", kind, strings.Join(bodies, ""))
		}
	}
	if v, _, x := e.mail.counts(); v != v0 || x != x0 || e.identity.UserCount() != before {
		t.Fatal("a refused captcha wrote an account or sent an email")
	}
	// The stranger's password was not applied to the pending account.
	wantError(t, e.post("/v1/auth/login", obj{"email": "pending@example.com", "password": "correct-horse-battery"}),
		http.StatusForbidden, identity.CodeEmailNotVerified)

	// The right answer: the same 202 for every email.
	var bodies []string
	for _, email := range emails {
		rec := e.post("/v1/auth/register", registerBody(email), withHeader(httpx.HeaderRequestID, rid))
		wantStatus(t, rec, http.StatusAccepted)
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] || bodies[0] != bodies[2] {
		t.Fatalf("the answers differ:\n%s", strings.Join(bodies, ""))
	}
	if e.identity.UserCount() != before+1 {
		t.Fatalf("users = %d, want %d", e.identity.UserCount(), before+1)
	}
	if _, _, x := e.mail.counts(); x != x0+1 {
		t.Fatal("the owner of the existing account was not told")
	}
}

func TestCaptchaRateLimit(t *testing.T) {
	e := newEnv(t)
	for i := range CaptchaRequestsBurst {
		if rec := e.get("/v1/auth/captcha"); rec.Code != http.StatusOK {
			t.Fatalf("question %d: status %d", i+1, rec.Code)
		}
	}
	rec := e.get("/v1/auth/captcha")
	wantError(t, rec, http.StatusTooManyRequests, httpx.CodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	// Another address is unaffected, and so is the sign-in bucket of this one.
	wantStatus(t, e.get("/v1/auth/captcha", withIP("203.0.113.9")), http.StatusOK)
	wantError(t, e.post("/v1/auth/login", obj{"email": "nobody@example.com", "password": "definitely-wrong-pw"}),
		http.StatusUnauthorized, identity.CodeInvalidCredentials)
}

// Expired questions are dropped when a new one is asked, used or not.
func TestExpiredCaptchasAreDeleted(t *testing.T) {
	e := newEnv(t)
	for range 5 {
		e.newCaptcha()
	}
	wantStatus(t, e.post("/v1/auth/register", registerBody("kept@example.com")), http.StatusAccepted)
	if got := e.identity.CaptchaCount(); got != 6 {
		t.Fatalf("captchas held = %d, want 6", got)
	}
	e.now = e.now.Add(10*time.Minute + time.Second)
	e.newCaptcha()
	if got := e.identity.CaptchaCount(); got != 1 {
		t.Fatalf("captchas held after expiry = %d, want only the new one", got)
	}
}
