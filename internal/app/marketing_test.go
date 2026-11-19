package app

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
)

// marketingBody is the body of GET and PUT /v1/me/marketing-preferences.
type marketingBody struct {
	MarketingOptIn            bool       `json:"marketing_opt_in"`
	ChangedAt                 *time.Time `json:"changed_at"`
	Source                    *string    `json:"source"`
	Context                   *string    `json:"context"`
	ConsentTextVersion        *string    `json:"consent_text_version"`
	CurrentConsentTextVersion string     `json:"current_consent_text_version"`
	ConsentText               struct {
		Version string `json:"version"`
		Text    string `json:"text"`
		Status  string `json:"status"`
	} `json:"consent_text"`
	History []struct {
		MarketingOptIn     bool      `json:"marketing_opt_in"`
		ChangedAt          time.Time `json:"changed_at"`
		Source             string    `json:"source"`
		Context            string    `json:"context"`
		ConsentTextVersion *string   `json:"consent_text_version"`
	} `json:"history"`
}

const marketingPath = "/v1/me/marketing-preferences"

func wantPrivate(t *testing.T, h http.Header) {
	t.Helper()
	if got := h.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func str(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

// An account that never chose: false, and every field from the latest
// record null, with an empty history.
func TestMarketingPreferencesNeverChose(t *testing.T) {
	e := newEnv(t)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	rec := e.get(marketingPath, agent)
	wantStatus(t, rec, http.StatusOK)
	wantPrivate(t, rec.Header())
	for _, want := range []string{`"marketing_opt_in":false`, `"changed_at":null`, `"source":null`, `"context":null`,
		`"consent_text_version":null`, `"current_consent_text_version":"marketing-2026-01"`, `"history":[]`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("missing %s in %s", want, rec.Body)
		}
	}
}

// Signed out: 401 on both routes, still private.
func TestMarketingPreferencesSignedOut(t *testing.T) {
	e := newEnv(t)
	rec := e.get(marketingPath)
	wantError(t, rec, http.StatusUnauthorized, "UNAUTHENTICATED")
	wantPrivate(t, rec.Header())
	wantError(t, e.put(marketingPath, obj{"marketing_opt_in": true}), http.StatusUnauthorized, "UNAUTHENTICATED")
}

// Opt in from the web (version defaulted and explicit), out from the app:
// source, context and version recorded; a repeat writes nothing.
func TestMarketingPreferencesChanges(t *testing.T) {
	e := newEnv(t)
	agentID := e.userID(identity.SeedAgentEmail)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))

	optInAt := e.now
	rec := e.put(marketingPath, obj{"marketing_opt_in": true, "consent_text_version": "marketing-2026-01"}, agent, withHeader("X-Client", "web"))
	wantStatus(t, rec, http.StatusOK)
	wantPrivate(t, rec.Header())
	m := decode[marketingBody](t, rec)
	if !m.MarketingOptIn || m.ChangedAt == nil || !m.ChangedAt.Equal(optInAt) || str(m.Source) != "web" || str(m.Context) != "PREFERENCES" ||
		str(m.ConsentTextVersion) != "marketing-2026-01" || len(m.History) != 1 {
		t.Fatalf("opt-in = %s", rec.Body)
	}

	// The same value again (later, from another client): nothing written,
	// the state unchanged including changed_at.
	e.now = e.now.Add(time.Hour)
	rec = e.put(marketingPath, obj{"marketing_opt_in": true}, agent, withHeader("X-Client", "app"))
	wantStatus(t, rec, http.StatusOK)
	m = decode[marketingBody](t, rec)
	if !m.MarketingOptIn || !m.ChangedAt.Equal(optInAt) || str(m.Source) != "web" || len(m.History) != 1 {
		t.Fatalf("no-op = %s", rec.Body)
	}
	if n := len(e.identity.MarketingConsents(agentID)); n != 1 {
		t.Fatalf("records after a no-op = %d", n)
	}

	// Opt out from the app (a bearer client); a version sent is ignored.
	token := e.loginBearer(identity.SeedAgentEmail, seedPassword)
	rec = e.put(marketingPath, obj{"marketing_opt_in": false, "consent_text_version": "anything"}, withBearer(token), withHeader("X-Client", "android"))
	wantStatus(t, rec, http.StatusOK)
	m = decode[marketingBody](t, rec)
	if m.MarketingOptIn || !m.ChangedAt.Equal(e.now) || str(m.Source) != "app" || str(m.Context) != "PREFERENCES" || m.ConsentTextVersion != nil {
		t.Fatalf("opt-out = %s", rec.Body)
	}
	if len(m.History) != 2 || m.History[0].MarketingOptIn || m.History[0].ConsentTextVersion != nil ||
		!m.History[1].MarketingOptIn || str(m.History[1].ConsentTextVersion) != "marketing-2026-01" || m.History[1].Source != "web" {
		t.Fatalf("history = %s", rec.Body)
	}
	// No X-Client: api. The GET shows the same state.
	e.now = e.now.Add(time.Hour)
	wantStatus(t, e.put(marketingPath, obj{"marketing_opt_in": true}, agent), http.StatusOK)
	m = decode[marketingBody](t, e.get(marketingPath, agent))
	if !m.MarketingOptIn || str(m.Source) != "api" || str(m.ConsentTextVersion) != "marketing-2026-01" || len(m.History) != 3 {
		t.Fatalf("api opt-in = %+v", m)
	}
}

// Validation: a stale or malformed version on opt-in, and a missing or
// non-boolean value, are 400 VALIDATION_FAILED on their field; nothing is
// written.
func TestMarketingPreferencesValidation(t *testing.T) {
	e := newEnv(t)
	agentID := e.userID(identity.SeedAgentEmail)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	for _, c := range []struct {
		body  obj
		field string
	}{
		{obj{"marketing_opt_in": true, "consent_text_version": "marketing-2025-01"}, "consent_text_version"},
		{obj{"marketing_opt_in": true, "consent_text_version": ""}, "consent_text_version"},
		{obj{"marketing_opt_in": true, "consent_text_version": strings.Repeat("v", 41)}, "consent_text_version"},
		{obj{"marketing_opt_in": true, "consent_text_version": 7}, "consent_text_version"},
		{obj{}, "marketing_opt_in"},
		{obj{"marketing_opt_in": nil}, "marketing_opt_in"},
		{obj{"marketing_opt_in": "yes"}, "marketing_opt_in"},
		{obj{"marketing_opt_in": 1}, "marketing_opt_in"},
		{obj{"consent_text_version": "marketing-2026-01"}, "marketing_opt_in"},
	} {
		env := wantError(t, e.put(marketingPath, c.body, agent), http.StatusBadRequest, "VALIDATION_FAILED")
		if env.Error.Fields[c.field] == "" {
			t.Fatalf("%v: fields = %v", c.body, env.Error.Fields)
		}
	}
	env := wantError(t, e.put(marketingPath, obj{"marketing_opt_in": true, "consent_text_version": "marketing-2025-01"}, agent),
		http.StatusBadRequest, "VALIDATION_FAILED")
	if got := env.Error.Fields["consent_text_version"]; got != "The consent wording has changed; show the current wording and try again." {
		t.Fatalf("stale message = %q", got)
	}
	if n := len(e.identity.MarketingConsents(agentID)); n != 0 {
		t.Fatalf("refused changes wrote %d records", n)
	}
}

// History: newest first, at most 20.
func TestMarketingPreferencesHistoryLimit(t *testing.T) {
	e := newEnv(t)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	for i := range 23 {
		e.now = e.now.Add(time.Minute)
		wantStatus(t, e.put(marketingPath, obj{"marketing_opt_in": i%2 == 0}, agent, withHeader("X-Client", "web")), http.StatusOK)
	}
	m := decode[marketingBody](t, e.get(marketingPath, agent))
	if len(m.History) != 20 || !m.History[0].ChangedAt.Equal(e.now) || !m.MarketingOptIn || !m.History[0].MarketingOptIn {
		t.Fatalf("history = %d, newest %s, opt-in %v", len(m.History), m.History[0].ChangedAt, m.MarketingOptIn)
	}
	for i := 1; i < len(m.History); i++ {
		if !m.History[i-1].ChangedAt.After(m.History[i].ChangedAt) {
			t.Fatalf("not newest first at %d", i)
		}
	}
}

// Registration: opting in writes a REGISTRATION record with the X-Client
// source and the current wording; not opting in writes none.
func TestRegistrationRecordsMarketingOptIn(t *testing.T) {
	e := newEnv(t)
	body := registerBody("opted.in@example.com")
	body["marketing_opt_in"] = true
	wantStatus(t, e.post("/v1/auth/register", body, withHeader("X-Client", "ios")), http.StatusAccepted)
	got := e.identity.MarketingConsents(e.userID("opted.in@example.com"))
	if len(got) != 1 || !got[0].OptIn || got[0].Source != "app" || got[0].Context != identity.MarketingContextRegistration ||
		got[0].ConsentTextVersion != identity.CurrentMarketingConsentVersion || !got[0].ChangedAt.Equal(e.now) {
		t.Fatalf("records = %+v", got)
	}
	wantStatus(t, e.post("/v1/auth/register", registerBody("no.choice@example.com")), http.StatusAccepted)
	if got := e.identity.MarketingConsents(e.userID("no.choice@example.com")); len(got) != 0 {
		t.Fatalf("records = %+v", got)
	}
}

// The seed: the consumer opted in at registration (web); the tenant opted
// in, then out from the app (two records).
func TestMarketingSeed(t *testing.T) {
	e := newEnv(t)
	m := decode[marketingBody](t, e.get(marketingPath, browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))))
	if !m.MarketingOptIn || len(m.History) != 1 || str(m.Source) != "web" || str(m.Context) != "REGISTRATION" ||
		str(m.ConsentTextVersion) != "marketing-2026-01" {
		t.Fatalf("consumer = %+v", m)
	}
	m = decode[marketingBody](t, e.get(marketingPath, browser(e.loginCookie(devseed.TenantEmail, seedPassword))))
	if m.MarketingOptIn || len(m.History) != 2 || str(m.Source) != "app" || str(m.Context) != "PREFERENCES" || m.ConsentTextVersion != nil ||
		!m.History[1].MarketingOptIn || m.History[1].Source != "web" {
		t.Fatalf("tenant = %+v", m)
	}
	// Re-running the memory seed changes nothing.
	if err := devseed.SeedMemoryMarketing(e.identity, e.now); err != nil {
		t.Fatal(err)
	}
	if n := len(e.identity.MarketingConsents(e.userID(devseed.TenantEmail))); n != 2 {
		t.Fatalf("tenant records after a re-seed = %d", n)
	}
}

// Account deletion deletes the records (the users row is anonymised, so
// nothing cascades).
func TestAccountDeletionRemovesMarketingConsent(t *testing.T) {
	e := newEnv(t)
	tenant := e.userID(devseed.TenantEmail)
	if n := len(e.identity.MarketingConsents(tenant)); n != 2 {
		t.Fatalf("seeded records = %d", n)
	}
	cookie := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	wantStatus(t, e.del("/v1/me", obj{"password": seedPassword}, cookie), http.StatusOK)
	if got := e.identity.MarketingConsents(tenant); len(got) != 0 {
		t.Fatalf("records after deletion = %+v", got)
	}
}

// Writes share the settings bucket with notification preferences: 429 once
// it is empty.
func TestMarketingPreferencesRateLimit(t *testing.T) {
	e := newEnv(t)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	for range notificationChangesBurst {
		wantStatus(t, e.do(http.MethodPut, "/v1/me/notification-preferences", obj{"items": []obj{}}, agent), http.StatusOK)
	}
	wantError(t, e.put(marketingPath, obj{"marketing_opt_in": true}, agent), http.StatusTooManyRequests, "RATE_LIMITED")
	// Reading is not limited.
	wantStatus(t, e.get(marketingPath, agent), http.StatusOK)
}

const draftMarketingText = "Send me news, tips and offers from RightProperty365 by email. I can stop them at any time."

func wantCurrentText(t *testing.T, m marketingBody) {
	t.Helper()
	if m.ConsentText.Version != "marketing-2026-01" || m.ConsentText.Text != draftMarketingText || m.ConsentText.Status != "DRAFT" {
		t.Fatalf("consent_text = %+v", m.ConsentText)
	}
}

// GET and PUT carry the current wording; the public route serves any
// version (cacheable), an unknown one is 404.
func TestMarketingConsentText(t *testing.T) {
	e := newEnv(t)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	wantCurrentText(t, decode[marketingBody](t, e.get(marketingPath, agent)))
	rec := e.put(marketingPath, obj{"marketing_opt_in": true}, agent)
	wantStatus(t, rec, http.StatusOK)
	wantCurrentText(t, decode[marketingBody](t, rec))

	rec = e.get("/v1/marketing-consent-texts/marketing-2026-01")
	wantStatus(t, rec, http.StatusOK)
	if !strings.HasPrefix(rec.Header().Get("Cache-Control"), "public") {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"version":"marketing-2026-01","text":"`+draftMarketingText+`","status":"DRAFT"}` {
		t.Fatalf("body = %s", got)
	}
	wantError(t, e.get("/v1/marketing-consent-texts/marketing-1999-01"), http.StatusNotFound, "MARKETING_CONSENT_TEXT_NOT_FOUND")
}
