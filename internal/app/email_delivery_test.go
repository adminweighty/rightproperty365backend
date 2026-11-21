package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/emaildelivery"
	"rightproperty365.com/backend/internal/platform/mail"
)

// randomWebhookSecret is a signing secret made for the test run.
func randomWebhookSecret(t *testing.T) string {
	t.Helper()
	key := make([]byte, 24)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(key)
}

// resendWebhook posts a Resend event signed with sig's key (nil: unsigned).
func (e *env) resendWebhook(id string, body string, key []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/email/resend", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("svix-id", id)
	req.Header.Set("svix-timestamp", strconv.FormatInt(e.now.Unix(), 10))
	if key != nil {
		req.Header.Set("svix-signature", emaildelivery.Sign(key, id, e.now, []byte(body)))
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// API 0.59.0, FR-065: Resend's signed webhooks set delivery states and
// suppress hard-bounced addresses; administrators see and clear them.
func TestEmailDeliveryWebhook(t *testing.T) {
	e := newEnv(t)
	key, err := emaildelivery.DecodeSecret(e.emailSecret)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	address := "bounced.person@example.com"
	_, _ = e.emailLog.Begin(ctx, mail.Record{Key: "welcome/1", Kind: "welcome", RecipientSHA256: mail.RecipientHash(address), Provider: "resend"}, e.now)
	_ = e.emailLog.Accepted(ctx, "welcome/1", "resend", "re_e2e_1", e.now)

	bounce := `{"type":"email.bounced","created_at":"2026-01-01T00:00:00Z","data":{"email_id":"re_e2e_1","to":["` + address + `"],"bounce":{"type":"Permanent"}}}`
	wantError(t, e.resendWebhook("msg_e2e_1", bounce, nil), http.StatusUnauthorized, emaildelivery.CodeBadSignature)
	wantStatus(t, e.resendWebhook("msg_e2e_1", bounce, key), http.StatusNoContent)
	wantStatus(t, e.resendWebhook("msg_e2e_1", bounce, key), http.StatusNoContent) // a retry changes nothing
	wantStatus(t, e.resendWebhook("msg_e2e_2", `{"type":"email.opened","data":{"email_id":"re_e2e_1"}}`, key), http.StatusNoContent)
	if ok, _ := e.emailDelivery.IsSuppressed(ctx, mail.RecipientHash(address)); !ok {
		t.Fatal("the hard bounce did not suppress the address")
	}

	support := e.asAdmin(devseed.SupportEmail)
	rec := e.get("/v1/admin/email-deliveries", support)
	wantStatus(t, rec, http.StatusOK)
	type overview struct {
		Configured bool `json:"webhook_configured"`
		Last24h    struct {
			Bounced int `json:"bounced"`
		} `json:"last_24h"`
		Problems []struct {
			DeliveryState *string `json:"delivery_state"`
			Recipient     string  `json:"recipient"`
		} `json:"problems"`
	}
	o := decode[overview](t, rec)
	if !o.Configured || o.Last24h.Bounced != 1 || len(o.Problems) != 1 || o.Problems[0].Recipient != mail.RecipientHash(address)[:12] {
		t.Fatalf("overview %+v", o)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(address)) {
		t.Fatal("the overview shows an address")
	}
	rec = e.get("/v1/admin/email-deliveries/suppressions?email="+address, support)
	wantStatus(t, rec, http.StatusOK)
	type list struct {
		Items []struct {
			Hash   string `json:"hash"`
			Reason string `json:"reason"`
		} `json:"items"`
	}
	items := decode[list](t, rec).Items
	if len(items) != 1 || items[0].Reason != emaildelivery.ReasonHardBounce {
		t.Fatalf("suppressions %+v", items)
	}
	wantStatus(t, e.do(http.MethodDelete, "/v1/admin/email-deliveries/suppressions/"+items[0].Hash, obj{"reason": "Mailbox fixed by the customer"}, support), http.StatusNoContent)
	wantError(t, e.do(http.MethodDelete, "/v1/admin/email-deliveries/suppressions/"+items[0].Hash, obj{"reason": "Mailbox fixed by the customer"}, support),
		http.StatusNotFound, emaildelivery.CodeNotFound)
	found := false
	for _, ev := range e.audit.All() {
		found = found || ev.Action == emaildelivery.ActionSuppressionClear
	}
	if !found {
		t.Fatal("clearing was not audited")
	}

	// A customer session is not an administrator's.
	wantStatus(t, e.get("/v1/admin/email-deliveries", withBearer(e.loginBearer(devseed.EstatesOwnerEmail, seedPassword))), http.StatusForbidden)
}
