package payments

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/paygateway"
)

// Handler serves the payment routes (see app.go for where they are mounted).
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

var answers = []struct {
	err           error
	status        int
	code, message string
}{
	{ErrNotOffered, http.StatusNotFound, CodeNotOffered, "Online payment is not available. Please pay by bank transfer as shown on your invoice."},
	{ErrProviderNotFound, http.StatusNotFound, CodeProviderNotFound, "Unknown payment provider."},
	{ErrInvalidSignature, http.StatusUnauthorized, CodeInvalidSignature, "The webhook signature is not valid."},
	{ErrProvider, http.StatusBadGateway, CodeProviderError, "The payment provider is not available right now. Nothing was charged. Please try again later."},
	{ErrCheckoutInProgress, http.StatusConflict, CodeCheckoutInProgress, "A payment for this invoice is being prepared. Please try again in a few seconds."},
	{ErrNoGatewayPayment, http.StatusConflict, CodeNoGatewayPayment, "This subscription has no online payment that can be refunded."},
	{ErrRefundExceeds, http.StatusUnprocessableEntity, CodeRefundExceeds, "The refund is more than what remains of the payment."},
	{ErrPaymentNotFound, http.StatusNotFound, CodePaymentNotFound, "This payment does not exist."},
	{subscriptions.ErrNotFound, http.StatusNotFound, subscriptions.CodeSubscriptionNotFound, "This subscription does not exist."},
	{subscriptions.ErrState, http.StatusConflict, subscriptions.CodeSubscriptionState, "The subscription's status does not allow this. Reload it and try again."},
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	if errors.As(err, &verr) {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
		return
	}
	for _, a := range answers {
		if errors.Is(err, a.err) {
			httpx.WriteError(w, r, a.status, a.code, a.message, nil)
			return
		}
	}
	httpx.WriteInternalError(w, r, h.logger, err)
}

func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

func pathID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	return id, err == nil
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// View is a payment as its subscriber sees it.
type View struct {
	ID             uuid.UUID  `json:"id"`
	SubscriptionID uuid.UUID  `json:"subscription_id"`
	InvoiceNumber  string     `json:"invoice_number"`
	Provider       string     `json:"provider"`
	Status         string     `json:"status"`
	Amount         string     `json:"amount"`
	CurrencyCode   string     `json:"currency_code"`
	RefundedAmount string     `json:"refunded_amount"`
	HostedURL      *string    `json:"hosted_url"`
	ExpiresAt      *time.Time `json:"expires_at"`
	CreatedAt      time.Time  `json:"created_at"`
	SucceededAt    *time.Time `json:"succeeded_at"`
	FailedAt       *time.Time `json:"failed_at"`
	ExpiredAt      *time.Time `json:"expired_at"`
	RefundedAt     *time.Time `json:"refunded_at"`
}

// ViewOf builds the subscriber's view of a payment as of now.
func ViewOf(p Payment, now time.Time) View {
	v := View{ID: p.ID, SubscriptionID: p.SubscriptionID, InvoiceNumber: p.InvoiceNumber, Provider: p.Provider, Status: p.Status, Amount: p.Amount,
		CurrencyCode: p.Currency, RefundedAmount: p.RefundedAmount, ExpiresAt: p.ExpiresAt, CreatedAt: p.CreatedAt, SucceededAt: p.SucceededAt,
		FailedAt: p.FailedAt, ExpiredAt: p.ExpiredAt, RefundedAt: p.RefundedAt}
	if v.RefundedAmount == "" {
		v.RefundedAmount = "0.00"
	}
	if p.Status == StatusOpen && p.HostedURL != "" && (p.ExpiresAt == nil || now.Before(*p.ExpiresAt)) {
		v.HostedURL = optional(p.HostedURL)
	}
	return v
}

type personRef struct {
	ID   *uuid.UUID `json:"id"`
	Name string     `json:"name"`
}

type subscriberRef struct {
	Kind string    `json:"kind"`
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// RefundView is a refund.
type RefundView struct {
	ID                uuid.UUID  `json:"id"`
	Amount            string     `json:"amount"`
	CurrencyCode      string     `json:"currency_code"`
	Reason            string     `json:"reason"`
	Status            string     `json:"status"`
	ProviderRefundRef *string    `json:"provider_refund_ref"`
	RequestedBy       personRef  `json:"requested_by"`
	CreatedAt         time.Time  `json:"created_at"`
	CompletedAt       *time.Time `json:"completed_at"`
	FailureReason     *string    `json:"failure_reason"`
}

func refundView(r Refund) RefundView {
	return RefundView{ID: r.ID, Amount: r.Amount, CurrencyCode: r.Currency, Reason: r.Reason, Status: r.Status, ProviderRefundRef: optional(r.ProviderRefundRef),
		RequestedBy: personRef{ID: r.RequestedBy, Name: r.RequestedByName}, CreatedAt: r.CreatedAt, CompletedAt: r.CompletedAt,
		FailureReason: optional(r.FailureReason)}
}

// AdminView is a payment as the administrators see it.
type AdminView struct {
	View
	Subscriber    subscriberRef `json:"subscriber"`
	MarketCode    string        `json:"market_code"`
	ProviderRef   *string       `json:"provider_ref"`
	PaymentRef    *string       `json:"payment_ref"`
	PaidAmount    *string       `json:"paid_amount"`
	PaidCurrency  *string       `json:"paid_currency_code"`
	ReviewReason  *string       `json:"review_reason"`
	FailureReason *string       `json:"failure_reason"`
	OpenedBy      personRef     `json:"opened_by"`
	Refunds       []RefundView  `json:"refunds"`
}

func adminView(p Payment, refunds []Refund, now time.Time) AdminView {
	v := AdminView{View: ViewOf(p, now), Subscriber: subscriberRef{Kind: p.SubscriberKind, ID: p.SubscriberID, Name: p.SubscriberName},
		MarketCode: p.MarketCode, ProviderRef: optional(p.ProviderRef), PaymentRef: optional(p.PaymentRef), PaidAmount: optional(p.PaidAmount),
		PaidCurrency: optional(p.PaidCurrency), ReviewReason: optional(p.ReviewReason), FailureReason: optional(p.FailureReason),
		OpenedBy: personRef{ID: p.OpenedBy, Name: p.OpenedByName}, Refunds: []RefundView{}}
	for _, r := range refunds {
		v.Refunds = append(v.Refunds, refundView(r))
	}
	return v
}

func (h *Handler) now() time.Time { return h.svc.now() }

// ---------------------------------------------------------------------------
// Subscriber
// ---------------------------------------------------------------------------

// baseURL is the API's origin as the caller reached it.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fp := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); fp == "https" || fp == "http" {
		scheme = fp
	}
	return scheme + "://" + r.Host
}

// Checkout handles POST /v1/me/subscriptions/{id}/checkout.
func (h *Handler) Checkout(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, subscriptions.ErrNotFound)
		return
	}
	res, err := h.svc.Checkout(r.Context(), p, id, baseURL(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if res.Reused {
		status = http.StatusOK
	}
	private(w)
	httpx.WriteJSON(w, status, map[string]any{"hosted_url": res.Payment.HostedURL, "expires_at": res.Payment.ExpiresAt, "reused": res.Reused,
		"payment": ViewOf(res.Payment, h.now())})
}

// Mine handles GET /v1/me/subscriptions/{id}/payments.
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, subscriptions.ErrNotFound)
		return
	}
	list, err := h.svc.Mine(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := h.now()
	out := make([]View, 0, len(list))
	for _, x := range list {
		out = append(out, ViewOf(x, now))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"payments": out})
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

// Webhook handles POST /v1/payments/webhooks/{provider}.
func (h *Handler) Webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "The body could not be read.", nil)
		return
	}
	dup, err := h.svc.Webhook(r.Context(), chi.URLParam(r, "provider"), r.Header, body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"received": true, "duplicate": dup})
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------

// AdminOf handles GET /v1/admin/subscriptions/{id}/payments.
func (h *Handler) AdminOf(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, subscriptions.ErrNotFound)
		return
	}
	list, refunds, err := h.svc.OfSubscription(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := h.now()
	out := make([]AdminView, 0, len(list))
	for _, p := range list {
		out = append(out, adminView(p, refunds[p.ID], now))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"payments": out})
}

// List handles GET /v1/admin/payments.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	q := r.URL.Query()
	fields := map[string]string{}
	page, size := 1, DefaultPageSize
	if raw := q.Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 100000 {
			fields["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := q.Get("page_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > MaxPageSize {
			fields["page_size"] = fmt.Sprintf("must be a whole number between 1 and %d", MaxPageSize)
		} else {
			size = n
		}
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	f := Filter{Provider: strings.ToLower(q.Get("provider")), Q: q.Get("q"), Limit: size, Offset: (page - 1) * size}
	if st := strings.ToUpper(strings.TrimSpace(q.Get("status"))); st != "" {
		f.Statuses = []string{st}
	}
	list, refunds, total, err := h.svc.List(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := h.now()
	items := make([]AdminView, 0, len(list))
	for _, p := range list {
		items = append(items, adminView(p, refunds[p.ID], now))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "page": page, "page_size": size, "total": total})
}

// Refund handles POST /v1/admin/subscriptions/{id}/refund.
func (h *Handler) Refund(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, subscriptions.ErrNotFound)
		return
	}
	var in RefundInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Refund(r.Context(), p, id, in, r.Header.Get("Idempotency-Key"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := h.now()
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"refund": refundView(out.Refund), "payment": adminView(out.Payment, out.Refunds, now),
		"subscription": subscriptions.ViewOf(out.Subscription, now)})
}

const exportDay = "2006-01-02"

// cell neutralises spreadsheet formulas (as the other exports do).
func cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func timeCell(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Export handles GET /v1/admin/reports/payments.csv?from=&to=&limit=
// (SUPER_ADMIN, FINANCE_ADMIN): the payments created in [from, to] (UTC
// days; default the last 30 days), newest first.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	q := r.URL.Query()
	fields := map[string]string{}
	today := h.now().Truncate(24 * time.Hour)
	to, from := today.Add(24*time.Hour), today.AddDate(0, 0, -29)
	if raw := q.Get("to"); raw != "" {
		if t, err := time.Parse(exportDay, raw); err != nil {
			fields["to"] = "must be a date (YYYY-MM-DD)"
		} else {
			to = t.Add(24 * time.Hour)
		}
	}
	if raw := q.Get("from"); raw != "" {
		if t, err := time.Parse(exportDay, raw); err != nil {
			fields["from"] = "must be a date (YYYY-MM-DD)"
		} else {
			from = t
		}
	}
	if !from.Before(to) {
		fields["from"] = "must be on or before to"
	}
	limit := DefaultExportCap
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > MaxExportCap {
			fields["limit"] = fmt.Sprintf("must be a whole number between 1 and %d", MaxExportCap)
		} else {
			limit = n
		}
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	list, total, err := h.svc.d.Store.Payments(r.Context(), Filter{CreatedFrom: from, CreatedTo: to, Limit: limit})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "text/csv; charset=utf-8")
	hd.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="payments-%s-to-%s.csv"`, from.Format(exportDay), to.Add(-24*time.Hour).Format(exportDay)))
	hd.Set("X-Total-Count", strconv.Itoa(total))
	hd.Set("X-Export-Limit", strconv.Itoa(limit))
	hd.Set("X-Export-Truncated", strconv.FormatBool(total > limit))
	private(w)
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "invoice_number", "subscription_id", "subscriber_kind", "subscriber_name", "market_code", "provider", "provider_ref",
		"payment_ref", "status", "amount", "currency_code", "paid_amount", "paid_currency_code", "refunded_amount", "review_reason", "created_at",
		"succeeded_at", "refunded_at"})
	for _, p := range list {
		created := p.CreatedAt
		rec := []string{p.ID.String(), p.InvoiceNumber, p.SubscriptionID.String(), p.SubscriberKind, p.SubscriberName, p.MarketCode, p.Provider,
			p.ProviderRef, p.PaymentRef, p.Status, p.Amount, p.Currency, p.PaidAmount, p.PaidCurrency, p.RefundedAmount, p.ReviewReason,
			timeCell(&created), timeCell(p.SucceededAt), timeCell(p.RefundedAt)}
		for i := range rec {
			rec[i] = cell(rec[i])
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
}

// ---------------------------------------------------------------------------
// The fake provider's hosted page (PAYMENT_GATEWAY=fake only)
// ---------------------------------------------------------------------------

var fakePage = template.Must(template.New("fake").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Fake payment provider</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:2rem auto;padding:0 1rem;color:#1a1a1a}
.box{border:2px dashed #b45309;border-radius:8px;padding:1rem 1.25rem;background:#fffbeb}
button{font-size:1rem;padding:.6rem 1rem;margin:.25rem .25rem .25rem 0;border-radius:6px;border:1px solid #444;cursor:pointer}
.pay{background:#15803d;color:#fff;border-color:#15803d}.fail{background:#b91c1c;color:#fff;border-color:#b91c1c}
form{display:inline}</style></head>
<body><div class="box"><p><strong>FAKE PAYMENT PROVIDER</strong>: development and staging only. Nothing is charged.</p>
<p>Invoice <strong>{{.Invoice}}</strong><br>Amount <strong>{{.Currency}} {{.Amount}}</strong><br>Status: {{.Status}}</p>
{{if .Open}}
<form method="post" action="{{.Base}}/pay"><button class="pay" type="submit">Pay</button></form>
<form method="post" action="{{.Base}}/pay-silently"><button type="submit">Pay (no webhook)</button></form>
<form method="post" action="{{.Base}}/fail"><button class="fail" type="submit">Decline</button></form>
{{end}}
<p><a href="{{.Cancel}}">Cancel and return</a></p></div></body></html>`))

func (h *Handler) fake() *paygateway.Fake {
	f, _ := h.svc.d.Gateway.(*paygateway.Fake)
	return f
}

func fakeHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
}

// FakeCheckout handles GET /v1/payments/fake/checkout/{ref}: the hosted page.
// It only shows; it changes nothing.
func (h *Handler) FakeCheckout(w http.ResponseWriter, r *http.Request) {
	f := h.fake()
	if f == nil {
		h.fail(w, r, ErrProviderNotFound)
		return
	}
	ref := chi.URLParam(r, "ref")
	p, err := h.svc.d.Store.PaymentByRef(r.Context(), f.Name(), ref)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	fakeHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = fakePage.Execute(w, map[string]any{"Invoice": p.InvoiceNumber, "Currency": p.Currency, "Amount": p.Amount, "Status": p.Status,
		"Open": p.Status == StatusOpen, "Base": "/v1/payments/fake/checkout/" + p.ProviderRef, "Cancel": template.URL(p.CancelURL)})
}

// FakeComplete handles POST /v1/payments/fake/checkout/{ref}/{action}: pay,
// pay-silently (paid at the fake provider, no webhook: reconciliation finds
// it) or fail. It signs the webhook with the development secret, delivers it
// to the webhook path in this process, and sends the browser back.
func (h *Handler) FakeComplete(w http.ResponseWriter, r *http.Request) {
	f := h.fake()
	if f == nil {
		h.fail(w, r, ErrProviderNotFound)
		return
	}
	ref, action := chi.URLParam(r, "ref"), chi.URLParam(r, "action")
	p, err := h.svc.d.Store.PaymentByRef(r.Context(), f.Name(), ref)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	succeed := action == "pay" || action == "pay-silently"
	if !succeed && action != "fail" {
		httpx.NotFound(w, r)
		return
	}
	ev, err := f.Complete(p.ProviderRef, succeed)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if action != "pay-silently" {
		body := paygateway.EventBody(ev)
		hdr := http.Header{}
		hdr.Set(paygateway.FakeSignatureHeader, f.Sign(body, h.now()))
		if _, err := h.svc.Webhook(context.WithoutCancel(r.Context()), f.Name(), hdr, body); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	// Back to the return page (it shows SUCCEEDED, OPEN or FAILED from the API).
	fakeHeaders(w)
	http.Redirect(w, r, p.ReturnURL, http.StatusSeeOther)
}
