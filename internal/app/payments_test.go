package app

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/packages"
	"rightproperty365.com/backend/internal/modules/payments"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/paygateway"
)

const paymentTestSecret = "app-payments-test-secret-0123"

func newPaymentEnv(t *testing.T) *env {
	return newEnvWithConfig(t, func(c *config.Config) {
		c.PaymentGateway = config.PaymentGatewayFake
		c.PaymentFakeWebhookSecret = config.Secret(paymentTestSecret)
	})
}

// signer signs fake webhooks with the server's secret, at the env's time.
func (e *env) paymentSigner() *paygateway.Fake {
	f, err := paygateway.NewFake("development", []byte(paymentTestSecret), func() time.Time { return e.now })
	if err != nil {
		e.t.Fatal(err)
	}
	return f
}

func (e *env) webhook(ev paygateway.Event, signature string) *httptest.ResponseRecorder {
	body := paygateway.EventBody(ev)
	if signature == "" {
		signature = e.paymentSigner().Sign(body, e.now)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/payments/webhooks/fake", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(paygateway.FakeSignatureHeader, signature)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

type checkoutBody struct {
	HostedURL string        `json:"hosted_url"`
	Reused    bool          `json:"reused"`
	Payment   paymentResult `json:"payment"`
}

type paymentResult struct {
	ID             string  `json:"id"`
	Status         string  `json:"status"`
	Amount         string  `json:"amount"`
	RefundedAmount string  `json:"refunded_amount"`
	HostedURL      *string `json:"hosted_url"`
	ProviderRef    *string `json:"provider_ref"`
	PaymentRef     *string `json:"payment_ref"`
	Refunds        []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Amount string `json:"amount"`
	} `json:"refunds"`
}

type subscriptionState struct {
	Subscription struct {
		ID                     string `json:"id"`
		Status                 string `json:"status"`
		InvoiceNumber          string `json:"invoice_number"`
		OnlinePaymentAvailable bool   `json:"online_payment_available"`
		CancelledBy            string `json:"cancelled_by"`
		Payment                *struct {
			Reference  string  `json:"reference"`
			RecordedBy string  `json:"recorded_by"`
			Provider   *string `json:"provider"`
		} `json:"payment"`
	} `json:"subscription"`
	Events []struct {
		Action    string `json:"action"`
		ActorType string `json:"actor_type"`
	} `json:"events"`
}

// freshAgentSubscription withdraws the agent's seeded request and asks for
// Agent Starter, quarterly, in Zimbabwe (USD 30.00).
func (e *env) freshAgentSubscription(agent reqOpt) subscriptionState {
	e.t.Helper()
	wantStatus(e.t, e.post("/v1/me/subscriptions/"+devseed.PendingAgentSubscription.String()+"/cancel", nil, agent), http.StatusOK)
	rec := e.post("/v1/me/subscriptions", obj{"plan_id": devseed.AgentStarterPlanID.String(), "period": "QUARTERLY", "market_code": "ZW"}, agent)
	wantStatus(e.t, rec, http.StatusCreated)
	return decode[subscriptionState](e.t, rec)
}

func (e *env) agentAssignment(sub string) *packages.Assignment {
	e.t.Helper()
	list, err := e.packages.Assignments(context.Background(), packages.Account{Kind: packages.KindDirectOwner, ID: identity.SeedAgentUserID})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range list {
		if a.SubscriptionID != nil && a.SubscriptionID.String() == sub {
			return &a
		}
	}
	return nil
}

func auditActions(log *adminaudit.MemoryLog, target string) string {
	var out []string
	for _, ev := range log.All() {
		if target == "" || ev.TargetID == target {
			out = append(out, ev.Action)
		}
	}
	return strings.Join(out, ",")
}

func pathOf(t *testing.T, raw string) string {
	t.Helper()
	i := strings.Index(raw, "/v1/")
	if i < 0 {
		t.Fatalf("not an API URL: %s", raw)
	}
	return raw[i:]
}

// The fake gateway end to end: checkout, the hosted page, pay, the signed
// webhook, ACTIVE through the approve path with the plan's package granted;
// duplicates and bad signatures; partial and full refunds.
func TestOnlinePaymentFakeEndToEnd(t *testing.T) {
	e := newPaymentEnv(t)
	cookie := e.loginCookie(identity.SeedAgentEmail, seedPassword)
	agent := browser(cookie)
	finance := e.asAdmin(devseed.FinanceAdminEmail)
	support := e.asAdmin(devseed.SupportEmail)

	// The plan grants the Business package (an administrator's edit).
	rec := e.patch("/v1/admin/subscription-plans/"+devseed.AgentStarterPlanID.String(), obj{"package_code": "business"}, finance)
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"package_code":"BUSINESS"`) {
		t.Fatalf("plan: %s", rec.Body.String())
	}
	rec = e.get("/v1/me/subscription-plans?market=ZW", agent)
	if body := rec.Body.String(); !strings.Contains(body, `"online_payment":{"enabled":true,"provider":"fake"}`) {
		t.Fatalf("plans: %s", body)
	}

	sub := e.freshAgentSubscription(agent)
	id := sub.Subscription.ID
	if !sub.Subscription.OnlinePaymentAvailable {
		t.Fatalf("online payment not offered: %+v", sub)
	}
	rec = e.post("/v1/me/subscriptions/"+id+"/checkout", nil, agent)
	wantStatus(t, rec, http.StatusCreated)
	co := decode[checkoutBody](t, rec)
	if !strings.Contains(co.HostedURL, "/v1/payments/fake/checkout/fake_cs_") || co.Reused || co.Payment.Status != payments.StatusOpen {
		t.Fatalf("checkout: %+v", co)
	}
	rec = e.post("/v1/me/subscriptions/"+id+"/checkout", obj{}, agent)
	wantStatus(t, rec, http.StatusOK)
	if again := decode[checkoutBody](t, rec); !again.Reused || again.HostedURL != co.HostedURL {
		t.Fatalf("not idempotent: %+v", again)
	}
	wantError(t, e.post("/v1/me/subscriptions/"+id+"/checkout", nil, browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))),
		http.StatusNotFound, subscriptions.CodeSubscriptionNotFound)

	// The hosted page and the status poll only show.
	page := pathOf(t, co.HostedURL)
	rec = e.get(page)
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, sub.Subscription.InvoiceNumber) || !strings.Contains(body, "USD 30.00") ||
		!strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("page: %s", body)
	}
	rec = e.get("/v1/me/subscriptions/"+id+"/payments", agent)
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"status":"OPEN"`) {
		t.Fatalf("payments: %s", rec.Body.String())
	}
	if s := decode[subscriptionState](t, e.get("/v1/me/subscriptions/"+id, agent)); s.Subscription.Status != subscriptions.StatusPendingPayment {
		t.Fatalf("the return page activated: %s", s.Subscription.Status)
	}

	// A forged webhook is refused and changes nothing.
	forged := paygateway.Event{ID: "evt-forged", Type: paygateway.EventPaymentSucceeded, ProviderRef: strings.TrimPrefix(page, "/v1/payments/fake/checkout/"),
		PaymentRef: "fake_pay_forged", Amount: "30.00", Currency: "USD", OccurredAt: e.now}
	wantError(t, e.webhook(forged, "t=1,v1=deadbeef"), http.StatusUnauthorized, payments.CodeInvalidSignature)

	// Pay, with the browser's session cookie and no Origin (the provider's
	// page): 303 to the web app's return page; the webhook activated it.
	rec = e.do(http.MethodPost, page+"/pay", nil, withCookie(cookie))
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "http://localhost:3000/account/subscriptions/"+id+"/payment?payment_id=") {
		t.Fatalf("pay: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	state := decode[subscriptionState](t, e.get("/v1/me/subscriptions/"+id, agent))
	if state.Subscription.Status != subscriptions.StatusActive || state.Subscription.Payment == nil || state.Subscription.Payment.RecordedBy != "GATEWAY" ||
		state.Subscription.Payment.Provider == nil || *state.Subscription.Payment.Provider != "fake" {
		t.Fatalf("activated: %+v", state.Subscription)
	}
	activations := func() int {
		n := 0
		for _, ev := range decode[subscriptionState](t, e.get("/v1/me/subscriptions/"+id, agent)).Events {
			if ev.Action == subscriptions.ActionActivated {
				n++
				if ev.ActorType != subscriptions.ActorGateway {
					t.Fatalf("actor: %s", ev.ActorType)
				}
			}
		}
		return n
	}
	if activations() != 1 {
		t.Fatal("not activated once")
	}
	a := e.agentAssignment(id)
	if a == nil || a.Source != packages.SourceInvoice || a.MarketCode != "ZW" || !a.InForce(packages.Day(e.now)) {
		t.Fatalf("assignment: %+v", a)
	}
	if body := e.get("/v1/me/subscriptions", agent).Body.String(); !strings.Contains(body, `"limit_enforced":true,"package_code":"BUSINESS"`) {
		t.Fatalf("usage: %s", body)
	}
	var sawActivated bool
	for _, ev := range e.subs.Outbox() {
		sawActivated = sawActivated || (ev.Type == subscriptions.EventActivated && ev.AggregateID.String() == id)
	}
	if !sawActivated {
		t.Fatal("no activation email")
	}

	// A second success for the same checkout, then its exact redelivery:
	// no second activation.
	again := forged
	again.ID, again.PaymentRef = "evt-second-"+uuid.NewString(), state.Subscription.Payment.Reference
	if rec := e.webhook(again, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"duplicate":false`) {
		t.Fatalf("second event: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.webhook(again, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"duplicate":true`) {
		t.Fatalf("redelivery: %d %s", rec.Code, rec.Body.String())
	}
	if activations() != 1 {
		t.Fatal("activated twice")
	}
	for _, want := range []string{payments.ActionCheckoutCreated, payments.ActionConfirmed, adminaudit.ActionSubscriptionApproved} {
		if !strings.Contains(auditActions(e.audit, id), want) {
			t.Fatalf("audit %s lacks %s", auditActions(e.audit, id), want)
		}
	}
	if !strings.Contains(auditActions(e.audit, identity.SeedAgentUserID.String()), adminaudit.ActionPackageAssigned) {
		t.Fatalf("no package audit: %s", auditActions(e.audit, ""))
	}

	// Refunds: SUPPORT may not; partial keeps it running; full cancels it.
	wantError(t, e.post("/v1/admin/subscriptions/"+id+"/refund", obj{"amount": "10.00", "reason": "Goodwill"}, support), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.post("/v1/admin/subscriptions/"+id+"/refund", obj{"amount": "0", "reason": "Goodwill"}, finance), http.StatusBadRequest, "VALIDATION_FAILED")
	rec = e.post("/v1/admin/subscriptions/"+id+"/refund", obj{"amount": "10.00", "reason": "Goodwill"}, finance, withHeader("Idempotency-Key", "refund-1"))
	wantStatus(t, rec, http.StatusOK)
	partial := decode[struct {
		Refund struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"refund"`
		Payment      paymentResult     `json:"payment"`
		Subscription subscriptionState `json:"-"`
	}](t, rec)
	if partial.Refund.Status != payments.RefundSucceeded || partial.Payment.Status != payments.StatusPartiallyRefunded || partial.Payment.RefundedAmount != "10.00" {
		t.Fatalf("partial: %+v", partial)
	}
	rec = e.post("/v1/admin/subscriptions/"+id+"/refund", obj{"amount": "10.00", "reason": "Goodwill"}, finance, withHeader("Idempotency-Key", "refund-1"))
	if !strings.Contains(rec.Body.String(), partial.Refund.ID) || !strings.Contains(rec.Body.String(), `"refunded_amount":"10.00"`) {
		t.Fatalf("not idempotent: %s", rec.Body.String())
	}
	if s := decode[subscriptionState](t, e.get("/v1/me/subscriptions/"+id, agent)); s.Subscription.Status != subscriptions.StatusActive {
		t.Fatalf("a partial refund ended it: %s", s.Subscription.Status)
	}
	if a := e.agentAssignment(id); a == nil || !a.InForce(packages.Day(e.now)) {
		t.Fatalf("a partial refund ended the package: %+v", a)
	}
	wantError(t, e.post("/v1/admin/subscriptions/"+id+"/refund", obj{"amount": "25.00", "reason": "Too much"}, finance),
		http.StatusUnprocessableEntity, payments.CodeRefundExceeds)
	rec = e.post("/v1/admin/subscriptions/"+id+"/refund", obj{"amount": "20.00", "reason": "Customer cancelled"}, finance)
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"status":"REFUNDED"`) || !strings.Contains(body, `"refunded_amount":"30.00"`) {
		t.Fatalf("full: %s", body)
	}
	state = decode[subscriptionState](t, e.get("/v1/me/subscriptions/"+id, agent))
	if state.Subscription.Status != subscriptions.StatusCancelled || state.Subscription.CancelledBy != subscriptions.ActorAdmin {
		t.Fatalf("full refund: %+v", state.Subscription)
	}
	if a := e.agentAssignment(id); a == nil || a.InForce(packages.Day(e.now)) || a.CancelledAt == nil {
		t.Fatalf("the package was not ended: %+v", a)
	}
	for _, want := range []string{payments.ActionRefundRequested, payments.ActionRefunded, adminaudit.ActionSubscriptionCanceled} {
		if !strings.Contains(auditActions(e.audit, id), want) {
			t.Fatalf("audit %s lacks %s", auditActions(e.audit, id), want)
		}
	}
	if !strings.Contains(auditActions(e.audit, identity.SeedAgentUserID.String()), adminaudit.ActionPackageAssignmentEnded) {
		t.Fatal("no package end audit")
	}
	wantError(t, e.post("/v1/admin/subscriptions/"+id+"/refund", obj{"amount": "1.00", "reason": "Again"}, finance), http.StatusConflict,
		payments.CodeNoGatewayPayment)

	// Finance reads and exports.
	rec = e.get("/v1/admin/subscriptions/"+id+"/payments", support)
	wantStatus(t, rec, http.StatusOK)
	list := decode[struct {
		Payments []paymentResult `json:"payments"`
	}](t, rec)
	if len(list.Payments) != 1 || len(list.Payments[0].Refunds) != 2 || list.Payments[0].PaymentRef == nil {
		t.Fatalf("admin payments: %+v", list)
	}
	if body := e.get("/v1/admin/payments?status=REFUNDED", finance).Body.String(); !strings.Contains(body, `"total":1`) {
		t.Fatalf("admin list: %s", body)
	}
	wantError(t, e.get("/v1/admin/reports/payments.csv", support), http.StatusForbidden, "FORBIDDEN")
	rec = e.get("/v1/admin/reports/payments.csv", finance)
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.HasPrefix(body, "id,invoice_number,") || !strings.Contains(body, sub.Subscription.InvoiceNumber+","+id) ||
		!strings.Contains(body, ",REFUNDED,30.00,USD,30.00,USD,30.00,") {
		t.Fatalf("csv: %s", body)
	}
}

// A payment that does not match the invoice is flagged and activates
// nothing; a declined payment leaves the invoice to pay.
func TestOnlinePaymentMismatchAndFailure(t *testing.T) {
	e := newPaymentEnv(t)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	finance := e.asAdmin(devseed.FinanceAdminEmail)
	id := e.freshAgentSubscription(agent).Subscription.ID
	rec := e.post("/v1/me/subscriptions/"+id+"/checkout", nil, agent)
	wantStatus(t, rec, http.StatusCreated)
	page := pathOf(t, decode[checkoutBody](t, rec).HostedURL)
	ref := strings.TrimPrefix(page, "/v1/payments/fake/checkout/")
	short := paygateway.Event{ID: "evt-short", Type: paygateway.EventPaymentSucceeded, ProviderRef: ref, PaymentRef: "fake_pay_short", Amount: "3.00",
		Currency: "USD", OccurredAt: e.now}
	wantStatus(t, e.webhook(short, ""), http.StatusOK)
	if s := decode[subscriptionState](t, e.get("/v1/me/subscriptions/"+id, agent)); s.Subscription.Status != subscriptions.StatusPendingPayment {
		t.Fatalf("a short payment activated: %s", s.Subscription.Status)
	}
	if body := e.get("/v1/admin/payments?status=REVIEW", finance).Body.String(); !strings.Contains(body, `"total":1`) || !strings.Contains(body, "Not activated") {
		t.Fatalf("review queue: %s", body)
	}
	if !strings.Contains(auditActions(e.audit, id), payments.ActionFlagged) {
		t.Fatalf("audit: %s", auditActions(e.audit, id))
	}
	// A wrong currency is flagged too.
	rec = e.post("/v1/me/subscriptions/"+id+"/checkout", nil, agent)
	wantStatus(t, rec, http.StatusCreated)
	page = pathOf(t, decode[checkoutBody](t, rec).HostedURL)
	pounds := paygateway.Event{ID: "evt-gbp", Type: paygateway.EventPaymentSucceeded, ProviderRef: strings.TrimPrefix(page, "/v1/payments/fake/checkout/"),
		PaymentRef: "fake_pay_gbp", Amount: "30.00", Currency: "GBP", OccurredAt: e.now}
	wantStatus(t, e.webhook(pounds, ""), http.StatusOK)
	if body := e.get("/v1/admin/payments?status=REVIEW", finance).Body.String(); !strings.Contains(body, `"total":2`) {
		t.Fatalf("currency mismatch: %s", body)
	}

	// Declined on the provider's page.
	rec = e.post("/v1/me/subscriptions/"+id+"/checkout", nil, agent)
	wantStatus(t, rec, http.StatusCreated)
	page = pathOf(t, decode[checkoutBody](t, rec).HostedURL)
	if rec := e.do(http.MethodPost, page+"/fail", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("fail: %d %s", rec.Code, rec.Body.String())
	}
	if body := e.get("/v1/me/subscriptions/"+id+"/payments", agent).Body.String(); !strings.Contains(body, `"status":"FAILED"`) {
		t.Fatalf("payments: %s", body)
	}
	if s := decode[subscriptionState](t, e.get("/v1/me/subscriptions/"+id, agent)); s.Subscription.Status != subscriptions.StatusPendingPayment ||
		!s.Subscription.OnlinePaymentAvailable {
		t.Fatalf("after a decline: %+v", s.Subscription)
	}
	if !strings.Contains(auditActions(e.audit, id), payments.ActionFailed) {
		t.Fatalf("audit: %s", auditActions(e.audit, id))
	}
	// The page of a finished checkout offers no buttons.
	if body := e.get(page).Body.String(); strings.Contains(body, "/pay\"") {
		t.Fatalf("buttons on a finished checkout: %s", body)
	}
}

// With PAYMENT_GATEWAY=none (the default) nothing is offered and the
// administrator's approval works as before, now granting the plan's package.
func TestOnlinePaymentOffByDefault(t *testing.T) {
	e := newEnv(t)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	finance := e.asAdmin(devseed.FinanceAdminEmail)
	if body := e.get("/v1/me/subscription-plans?market=ZW", agent).Body.String(); !strings.Contains(body, `"online_payment":{"enabled":false,"provider":null}`) {
		t.Fatalf("plans: %s", body)
	}
	wantStatus(t, e.patch("/v1/admin/subscription-plans/"+devseed.AgentStarterPlanID.String(), obj{"package_code": "BUSINESS"}, finance), http.StatusOK)
	wantError(t, e.patch("/v1/admin/subscription-plans/"+devseed.AgentStarterPlanID.String(), obj{"package_code": "not a code"}, finance),
		http.StatusBadRequest, "VALIDATION_FAILED")
	sub := e.freshAgentSubscription(agent)
	id := sub.Subscription.ID
	if sub.Subscription.OnlinePaymentAvailable {
		t.Fatal("online payment offered with none")
	}
	wantError(t, e.post("/v1/me/subscriptions/"+id+"/checkout", nil, agent), http.StatusNotFound, payments.CodeNotOffered)
	wantError(t, e.webhook(paygateway.Event{ID: "x", Type: paygateway.EventPaymentSucceeded}, ""), http.StatusNotFound, payments.CodeProviderNotFound)
	if rec := e.get("/v1/payments/fake/checkout/fake_cs_x"); rec.Code != http.StatusNotFound {
		t.Fatalf("fake page with none: %d", rec.Code)
	}
	wantError(t, e.post("/v1/admin/subscriptions/"+id+"/refund", obj{"amount": "1.00", "reason": "Test"}, finance), http.StatusNotFound, payments.CodeNotOffered)

	// The administrator's approval grants the package; a cancellation ends it.
	wantStatus(t, e.post("/v1/admin/subscriptions/"+id+"/approve", obj{"payment_reference": "BANK-1", "amount": "30.00",
		"received_on": e.now.Format("2006-01-02")}, finance), http.StatusOK)
	a := e.agentAssignment(id)
	if a == nil || a.Source != packages.SourceInvoice || !a.InForce(packages.Day(e.now)) {
		t.Fatalf("approve did not grant: %+v", a)
	}
	state := decode[subscriptionState](t, e.get("/v1/me/subscriptions/"+id, agent))
	if state.Subscription.Payment == nil || state.Subscription.Payment.RecordedBy != "ADMIN" || state.Subscription.Payment.Provider != nil {
		t.Fatalf("payment: %+v", state.Subscription.Payment)
	}
	e.now = e.now.Add(48 * time.Hour)
	finance = e.asAdmin(devseed.FinanceAdminEmail) // the administrator session idled out
	wantStatus(t, e.post("/v1/admin/subscriptions/"+id+"/cancel", obj{"reason": "Withdrawn by finance"}, finance), http.StatusOK)
	a = e.agentAssignment(id)
	if a == nil || a.InForce(packages.Day(e.now)) || a.CancelledAt != nil || a.EndsOn == nil || !a.EndsOn.Equal(packages.Day(e.now).AddDate(0, 0, -1)) {
		t.Fatalf("cancel did not end it yesterday: %+v", a)
	}
}
