package app

import (
	"net/http"
	"strings"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/invoices"
	"rightproperty365.com/backend/internal/platform/httpx"
)

type invoiceEnvelope struct {
	Invoice struct {
		ID             string   `json:"id"`
		Number         *string  `json:"number"`
		Status         string   `json:"status"`
		SubscriptionID *string  `json:"subscription_id"`
		Total          string   `json:"total"`
		Balance        string   `json:"balance"`
		Actions        []string `json:"actions"`
		PayURL         *string  `json:"pay_url"`
		Payments       []struct {
			ReceiptNumber string `json:"receipt_number"`
		} `json:"payments"`
		Events []struct {
			Action string `json:"action"`
		} `json:"events"`
	} `json:"invoice"`
}

// Renewal invoices (API 0.47.0) through the full router: who may do what,
// the payer's view, part and full payment, and the renewal they activate.
func TestRenewalInvoices(t *testing.T) {
	e := newEnv(t)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	other := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))
	finance, support := e.asAdmin(devseed.FinanceAdminEmail), e.asAdmin(devseed.SupportEmail)

	sub := e.freshAgentSubscription(agent)
	id := sub.Subscription.ID
	wantStatus(t, e.post("/v1/admin/subscriptions/"+id+"/approve", obj{"payment_reference": "BANK-1", "amount": "30.00",
		"received_on": e.now.Format("2006-01-02")}, finance), http.StatusOK)

	// Read-only for SUPPORT; nothing for a customer.
	wantStatus(t, e.get("/v1/admin/invoices", support), http.StatusOK)
	wantError(t, e.post("/v1/admin/invoices", obj{"renews_subscription_id": id}, support), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get("/v1/admin/invoices", agent), http.StatusForbidden, httpx.CodeForbidden)

	rec := e.post("/v1/admin/invoices", obj{"renews_subscription_id": id}, finance)
	wantStatus(t, rec, http.StatusCreated)
	draft := decode[invoiceEnvelope](t, rec).Invoice
	if draft.Status != "DRAFT" || draft.Number != nil || draft.Total != "34.50" || strings.Join(draft.Actions, ",") != "edit,issue,discard" {
		t.Fatalf("draft: %+v", draft)
	}
	if list := decode[obj](t, e.get("/v1/me/invoices", agent)); len(list["items"].([]any)) != 0 {
		t.Fatalf("a draft is shown to the payer: %v", list)
	}
	wantError(t, e.get("/v1/me/invoices/"+draft.ID, agent), http.StatusNotFound, invoices.CodeNotFound)

	rec = e.post("/v1/admin/invoices/"+draft.ID+"/issue", nil, finance)
	wantStatus(t, rec, http.StatusOK)
	issued := decode[invoiceEnvelope](t, rec).Invoice
	if issued.Status != "ISSUED" || issued.Number == nil || !strings.HasPrefix(*issued.Number, "INV-ZW-") || issued.SubscriptionID == nil {
		t.Fatalf("issued: %+v", issued)
	}
	wantError(t, e.post("/v1/admin/invoices/"+draft.ID+"/issue", nil, finance), http.StatusConflict, invoices.CodeState)

	// The payer sees it, with the subscription page to pay it on; nobody else does.
	mine := decode[invoiceEnvelope](t, e.get("/v1/me/invoices/"+issued.ID, agent)).Invoice
	if mine.PayURL == nil || !strings.HasSuffix(*mine.PayURL, "/account/subscriptions/"+*issued.SubscriptionID) || mine.Actions != nil {
		t.Fatalf("payer view: %+v", mine)
	}
	wantError(t, e.get("/v1/me/invoices/"+issued.ID, other), http.StatusNotFound, invoices.CodeNotFound)

	// A part payment, then the rest: PAID, receipts, and the renewal is active.
	pay := func(amount string) *invoiceEnvelope {
		rec := e.post("/v1/admin/invoices/"+issued.ID+"/payments", obj{"amount": amount, "method": "BANK_TRANSFER", "reference": "TRF-1",
			"received_on": e.now.Format("2006-01-02")}, finance)
		wantStatus(t, rec, http.StatusCreated)
		v := decode[invoiceEnvelope](t, rec)
		return &v
	}
	wantError(t, e.post("/v1/admin/invoices/"+issued.ID+"/payments", obj{"amount": "34.51", "method": "CASH", "received_on": e.now.Format("2006-01-02")}, finance),
		http.StatusUnprocessableEntity, invoices.CodeOverpayment)
	if v := pay("12.50"); v.Invoice.Status != "PART_PAID" || v.Invoice.Balance != "22.00" || len(v.Invoice.Payments) != 1 {
		t.Fatalf("part: %+v", v.Invoice)
	}
	if renewal := decode[subscriptionState](t, e.get("/v1/me/subscriptions/"+*issued.SubscriptionID, agent)); renewal.Subscription.Status != "PENDING_PAYMENT" {
		t.Fatalf("renewal after a part payment: %s", renewal.Subscription.Status)
	}
	paid := pay("22.00")
	if paid.Invoice.Status != "PAID" || paid.Invoice.Balance != "0.00" || len(paid.Invoice.Payments) != 2 ||
		paid.Invoice.Payments[0].ReceiptNumber == paid.Invoice.Payments[1].ReceiptNumber {
		t.Fatalf("paid: %+v", paid.Invoice)
	}
	if renewal := decode[subscriptionState](t, e.get("/v1/me/subscriptions/"+*issued.SubscriptionID, agent)); renewal.Subscription.Status != "ACTIVE" {
		t.Fatalf("renewal after payment: %s", renewal.Subscription.Status)
	}
	wantError(t, e.post("/v1/admin/invoices/"+issued.ID+"/void", obj{"reason": "Wrong"}, finance), http.StatusConflict, invoices.CodeState)
	if got := decode[invoiceEnvelope](t, e.get("/v1/me/invoices/"+issued.ID, agent)).Invoice; got.PayURL != nil {
		t.Fatalf("a paid invoice still has a payment link: %v", *got.PayURL)
	}
}
