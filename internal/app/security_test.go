package app

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/httpx"
)

func (m *captureMailer) SendEmailChangeConfirm(_ context.Context, _, _, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.emailChanges = append(m.emailChanges, testOrigin+"/confirm-email-change?token="+url.QueryEscape(token))
	return nil
}

func (m *captureMailer) SendEmailChangeNotice(_ context.Context, to, _, _ string, done bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kind := "requested"
	if done {
		kind = "changed"
	}
	m.emailNotices = append(m.emailNotices, to+" "+kind)
	return nil
}

func TestClientLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36":  "Chrome on Windows",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Version/17.0 Mobile Safari/604.1": "Safari on iOS",
		"RightProperty365/1.4 (Android 14)": "RightProperty365 app on Android",
		"":                                  "Unknown device",
	} {
		if got := identity.ClientLabel(ua); got != want {
			t.Errorf("%q: %q, want %q", ua, got, want)
		}
	}
}

// Account security (API 0.50.0) through the full router.
func TestAccountSecurity(t *testing.T) {
	e := newEnv(t)
	email := identity.SeedConsumerEmail
	first := withBearer(e.loginBearer(email, seedPassword))
	second := withBearer(e.loginBearer(email, seedPassword))

	type sessions struct {
		Items []struct {
			ID      string `json:"id"`
			Client  string `json:"client"`
			Current bool   `json:"current"`
		} `json:"items"`
	}
	list := decode[sessions](t, e.get("/v1/me/sessions", first))
	current := 0
	for _, s := range list.Items {
		if s.Current {
			current++
		}
	}
	if len(list.Items) < 2 || current != 1 {
		t.Fatalf("sessions: %+v", list)
	}

	// The password: wrong current refused; right one keeps this session only.
	env := wantError(t, e.post("/v1/me/password", obj{"current_password": "wrong", "new_password": "a-new-correct-horse"}, first),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["current_password"] == "" {
		t.Fatalf("fields: %+v", env.Error.Fields)
	}
	wantStatus(t, e.post("/v1/me/password", obj{"current_password": seedPassword, "new_password": "a-new-correct-horse"}, first), http.StatusNoContent)
	wantStatus(t, e.get("/v1/me", first), http.StatusOK)
	wantError(t, e.get("/v1/me", second), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	third := withBearer(e.loginBearer(email, "a-new-correct-horse"))
	got := decode[struct {
		Revoked int `json:"revoked"`
	}](t, e.post("/v1/me/sessions/revoke-others", nil, first))
	if got.Revoked < 1 {
		t.Fatalf("revoked: %d", got.Revoked)
	}
	wantError(t, e.get("/v1/me", third), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// The email: a link to the new address, notices to the old one.
	wantStatus(t, e.post("/v1/me/email", obj{"new_email": "casey.new@example.test", "current_password": "a-new-correct-horse"}, first), http.StatusAccepted)
	token := e.mail.lastToken(t, &e.mail.emailChanges, "/confirm-email-change")
	wantError(t, e.post("/v1/auth/email-change/confirm", obj{"token": "nope"}), http.StatusBadRequest, identity.CodeInvalidToken)
	wantStatus(t, e.post("/v1/auth/email-change/confirm", obj{"token": token}), http.StatusOK)
	wantError(t, e.post("/v1/auth/email-change/confirm", obj{"token": token}), http.StatusBadRequest, identity.CodeInvalidToken)
	e.mail.mu.Lock()
	notices := append([]string(nil), e.mail.emailNotices...)
	e.mail.mu.Unlock()
	if len(notices) != 2 || notices[0] != email+" requested" || notices[1] != email+" changed" {
		t.Fatalf("notices: %v", notices)
	}
	wantStatus(t, e.get("/v1/me", first), http.StatusOK)
	e.loginBearer("casey.new@example.test", "a-new-correct-horse")

	// A taken address: the same answer, and no link.
	before := len(e.mail.emailChanges)
	wantStatus(t, e.post("/v1/me/email", obj{"new_email": identity.SeedAgentEmail, "current_password": "a-new-correct-horse"}, first), http.StatusAccepted)
	if len(e.mail.emailChanges) != before {
		t.Fatal("a taken address got a confirmation link")
	}
}
