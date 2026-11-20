package invoices

import (
	"errors"
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
)

// Handler serves the invoice routes.
type Handler struct {
	svc    *Service
	logger *slog.Logger
	// webURL is the web app's origin (the payment link).
	webURL string
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger, publicWebURL string) *Handler {
	return &Handler{svc: svc, logger: logger, webURL: strings.TrimRight(publicWebURL, "/")}
}

// PathSubscription is the web page of a subscription (where it is paid);
// PathMyInvoice of a customer's invoice.
const (
	PathSubscription = "/account/subscriptions/"
	PathMyInvoice    = "/account/invoices/"
)

// OfflineInstructions is how to pay when online payment is off or not offered.
const OfflineInstructions = "Pay by bank transfer, or by another method stated on this invoice, quoting the invoice number. " +
	"Your subscription renews once we have recorded the payment. Nothing is charged automatically."

var answers = []struct {
	err           error
	status        int
	code, message string
}{
	{ErrNotFound, http.StatusNotFound, CodeNotFound, "This invoice does not exist."},
	{ErrState, http.StatusConflict, CodeState, "The invoice's status does not allow this."},
	{ErrRenewalExists, http.StatusConflict, CodeRenewalExists, "This subscriber already has a renewal waiting for payment or paid. Cancel it first, or wait until it starts."},
	{ErrNotInvoiceable, http.StatusConflict, CodeNotInvoiceable, "This subscription cannot be renewed by an invoice: it is not active and did not end in the last 90 days."},
	{ErrOverpayment, http.StatusUnprocessableEntity, CodeOverpayment, "The amount is more than what is due on this invoice."},
	{ErrHasPayments, http.StatusConflict, CodeHasPayments, "An invoice with payments cannot be voided. Issue a credit note instead."},
	{ErrResendTooSoon, http.StatusTooManyRequests, CodeResendTooSoon, "This invoice was sent less than 10 minutes ago."},
	{ErrPriceMissing, http.StatusConflict, CodePriceNotDefined, "The plan has no price for this currency and period."},
	{subscriptions.ErrNotFound, http.StatusNotFound, subscriptions.CodeSubscriptionNotFound, "This subscription does not exist."},
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	if errors.As(err, &verr) {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
		return
	}
	var serr *subscriptions.ValidationError
	if errors.As(err, &serr) {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", serr.Fields)
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

func pathID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	return id, err == nil
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

func dayString(t time.Time) string { return t.Format(DayLayout) }

func dayPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := dayString(*t)
	return &s
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// PayerView names who pays.
type PayerView struct {
	Kind string    `json:"kind"`
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// InvoiceView is an invoice as both sides see it.
type InvoiceView struct {
	ID                   uuid.UUID  `json:"id"`
	Number               *string    `json:"number"`
	Status               string     `json:"status"`
	StatusLabel          string     `json:"status_label"`
	MarketCode           string     `json:"market_code"`
	CurrencyCode         string     `json:"currency_code"`
	Payer                PayerView  `json:"payer"`
	RenewsSubscriptionID uuid.UUID  `json:"renews_subscription_id"`
	SubscriptionID       *uuid.UUID `json:"subscription_id"`
	PlanName             string     `json:"plan_name"`
	Period               string     `json:"period"`
	ServiceFrom          string     `json:"service_from"`
	ServiceTo            string     `json:"service_to"`
	IssueOn              *string    `json:"issue_on"`
	DueOn                string     `json:"due_on"`
	Terms                string     `json:"terms"`
	Lines                []Line     `json:"lines"`
	Subtotal             string     `json:"subtotal"`
	Tax                  *Tax       `json:"tax"`
	Total                string     `json:"total"`
	AmountPaid           string     `json:"amount_paid"`
	AmountCredited       string     `json:"amount_credited"`
	Balance              string     `json:"balance"`
	VoidReason           *string    `json:"void_reason"`
	DisputeReason        *string    `json:"dispute_reason"`
	ReplacesInvoiceID    *uuid.UUID `json:"replaces_invoice_id"`
	SentAt               *time.Time `json:"sent_at"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// View builds the shared view of an invoice.
func View(inv Invoice) InvoiceView {
	v := InvoiceView{ID: inv.ID, Number: strPtr(inv.Number), Status: inv.Status, StatusLabel: StatusLabel(inv.Status), MarketCode: inv.MarketCode,
		CurrencyCode: inv.CurrencyCode, Payer: PayerView{Kind: inv.SubscriberKind, ID: inv.SubscriberID, Name: inv.PayerName},
		RenewsSubscriptionID: inv.RenewsSubscriptionID, SubscriptionID: inv.SubscriptionID, PlanName: inv.PlanName, Period: inv.Period,
		ServiceFrom: dayString(inv.ServiceFrom), ServiceTo: dayString(inv.ServiceTo), IssueOn: dayPtr(inv.IssueOn), DueOn: dayString(inv.DueOn),
		Terms: inv.Terms, Lines: inv.Lines, Subtotal: inv.Subtotal, Tax: inv.Tax, Total: inv.Total, AmountPaid: inv.AmountPaid,
		AmountCredited: inv.AmountCredited, Balance: inv.Balance(), VoidReason: strPtr(inv.VoidReason), DisputeReason: strPtr(inv.DisputeReason),
		ReplacesInvoiceID: inv.ReplacesInvoiceID, SentAt: inv.SentAt, CreatedAt: inv.CreatedAt, UpdatedAt: inv.UpdatedAt}
	if v.Lines == nil {
		v.Lines = []Line{}
	}
	return v
}

// PaymentView is a payment with its receipt.
type PaymentView struct {
	ID            uuid.UUID `json:"id"`
	ReceiptNumber string    `json:"receipt_number"`
	Amount        string    `json:"amount"`
	Method        string    `json:"method"`
	MethodLabel   string    `json:"method_label"`
	Reference     string    `json:"reference"`
	ReceivedOn    string    `json:"received_on"`
	CreatedAt     time.Time `json:"created_at"`
}

func paymentViews(list []Payment) []PaymentView {
	out := make([]PaymentView, 0, len(list))
	for _, p := range list {
		out = append(out, PaymentView{ID: p.ID, ReceiptNumber: p.ReceiptNumber, Amount: p.Amount, Method: p.Method, MethodLabel: methodLabel(p.Method),
			Reference: p.Reference, ReceivedOn: dayString(p.ReceivedOn), CreatedAt: p.CreatedAt})
	}
	return out
}

// EventView is one history entry (administrators only).
type EventView struct {
	Action    string    `json:"action"`
	ActorType string    `json:"actor_type"`
	ActorName string    `json:"actor_name"`
	Note      string    `json:"note"`
	At        time.Time `json:"at"`
}

// Admin actions.
const (
	ActEdit           = "edit"
	ActDiscard        = "discard"
	ActIssue          = "issue"
	ActResend         = "resend"
	ActRecordPayment  = "record_payment"
	ActCreditNote     = "credit_note"
	ActVoid           = "void"
	ActReissue        = "reissue"
	ActDispute        = "dispute"
	ActResolveDispute = "resolve_dispute"
)

// Actions are what an administrator may do to inv now.
func Actions(inv Invoice) []string {
	switch inv.Status {
	case StatusDraft:
		return []string{ActEdit, ActIssue, ActDiscard}
	case StatusIssued, StatusPartPaid, StatusOverdue:
		out := []string{ActRecordPayment, ActResend, ActCreditNote}
		if Compare(inv.AmountPaid, "0") == 0 {
			out = append(out, ActVoid)
		}
		return append(out, ActDispute)
	case StatusPaid:
		return []string{ActDispute}
	case StatusDisputed:
		return []string{ActResolveDispute}
	case StatusVoid:
		return []string{ActReissue}
	}
	return []string{}
}

// AdminDetail is GET /v1/admin/invoices/{id}.
type AdminDetail struct {
	InvoiceView
	CreatedByName string        `json:"created_by_name"`
	SendCount     int           `json:"send_count"`
	LastSentAt    *time.Time    `json:"last_sent_at"`
	Payments      []PaymentView `json:"payments"`
	CreditNotes   []CreditNote  `json:"credit_notes"`
	Events        []EventView   `json:"events"`
	// Actions: edit, issue, discard, record_payment, resend, credit_note,
	// void, dispute, resolve_dispute, reissue (empty for read-only roles).
	Actions []string `json:"actions"`
}

func adminDetail(d Detail, canDecide bool) AdminDetail {
	v := AdminDetail{InvoiceView: View(d.Invoice), CreatedByName: d.Invoice.CreatedByName, SendCount: d.Invoice.SendCount,
		LastSentAt: d.Invoice.LastSentAt, Payments: paymentViews(d.Payments), CreditNotes: d.Credits, Events: []EventView{}, Actions: []string{}}
	if v.CreditNotes == nil {
		v.CreditNotes = []CreditNote{}
	}
	for _, e := range d.Events {
		v.Events = append(v.Events, EventView{Action: e.Action, ActorType: e.ActorType, ActorName: e.ActorName, Note: e.Note, At: e.At})
	}
	if canDecide {
		v.Actions = Actions(d.Invoice)
	}
	return v
}

func canDecide(p *auth.Principal) bool {
	for _, r := range DecisionRoles() {
		if p.HasPlatformRole(r) {
			return true
		}
	}
	return false
}

// CustomerDetail is GET /v1/me/invoices/{id}.
type CustomerDetail struct {
	InvoiceView
	Payments    []PaymentView `json:"payments"`
	CreditNotes []CreditNote  `json:"credit_notes"`
	// PayURL is the subscription page where the invoice is paid, while it
	// is payable and the caller may open it; null otherwise.
	PayURL *string `json:"pay_url"`
	// PayOnline: a payment gateway is configured (the page offers "Pay
	// online"); otherwise pay offline as OfflineInstructions says.
	PayOnline           bool   `json:"pay_online"`
	OfflineInstructions string `json:"offline_instructions"`
}

func (h *Handler) customerDetail(d Detail, payer Payer) CustomerDetail {
	v := CustomerDetail{InvoiceView: View(d.Invoice), Payments: paymentViews(d.Payments), CreditNotes: d.Credits,
		PayOnline: h.svc.OnlinePayment() != "", OfflineInstructions: OfflineInstructions}
	if v.CreditNotes == nil {
		v.CreditNotes = []CreditNote{}
	}
	if payable(d.Invoice.Status) && d.Invoice.SubscriptionID != nil && payer.CanPay[d.Invoice.SubscriberID] {
		u := h.webURL + PathSubscription + d.Invoice.SubscriptionID.String()
		v.PayURL = &u
	}
	if Compare(d.Invoice.AmountPaid, "0") > 0 || Compare(d.Invoice.AmountCredited, "0") > 0 {
		v.PayOnline = false // the rest is paid offline (payments.ErrPaidOffline)
	}
	return v
}

// ---------------------------------------------------------------------------
// Administrators
// ---------------------------------------------------------------------------

func paging(r *http.Request) (page, size int) {
	page, size = 1, DefaultPageSize
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("page_size")); err == nil && v > 0 && v <= MaxPageSize {
		size = v
	}
	return page, size
}

func pages(total, size int) int {
	if total == 0 {
		return 1
	}
	return (total + size - 1) / size
}

// AdminList serves GET /v1/admin/invoices?status=&market=&q=&page=&page_size=.
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := Filter{Market: strings.ToUpper(strings.TrimSpace(q.Get("market"))), Q: q.Get("q")}
	for _, s := range strings.Split(q.Get("status"), ",") {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			valid := false
			for _, known := range Statuses() {
				valid = valid || s == known
			}
			if !valid {
				h.fail(w, r, &ValidationError{Fields: map[string]string{"status": "must be one of " + strings.Join(Statuses(), ", ")}})
				return
			}
			f.Statuses = append(f.Statuses, s)
		}
	}
	if raw := q.Get("subscription"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			h.fail(w, r, &ValidationError{Fields: map[string]string{"subscription": "must be a subscription id"}})
			return
		}
		f.RenewsSubscription = &id
	}
	page, size := paging(r)
	f.Limit, f.Offset = size, (page-1)*size
	items, total, err := h.svc.List(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	views := make([]InvoiceView, 0, len(items))
	for _, inv := range items {
		views = append(views, View(inv))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": views, "page": page, "page_size": size, "total": total, "total_pages": pages(total, size)})
}

func (h *Handler) writeAdmin(w http.ResponseWriter, r *http.Request, status int, id uuid.UUID) {
	d, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, status, map[string]any{"invoice": adminDetail(d, canDecide(auth.FromContext(r.Context())))})
}

// AdminGet serves GET /v1/admin/invoices/{id}.
func (h *Handler) AdminGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	h.writeAdmin(w, r, http.StatusOK, id)
}

// Create serves POST /v1/admin/invoices: a DRAFT.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in DraftInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	inv, err := h.svc.Create(r.Context(), AdminOf(auth.FromContext(r.Context())), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeAdmin(w, r, http.StatusCreated, inv.ID)
}

// act runs an administrator's action on {id} and answers with the invoice.
func (h *Handler) act(w http.ResponseWriter, r *http.Request, status int, fn func(a Admin, id uuid.UUID) (uuid.UUID, error)) {
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	out, err := fn(AdminOf(auth.FromContext(r.Context())), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "invoice changed", slog.String("invoice_id", out.String()), slog.String("route", chi.RouteContext(r.Context()).RoutePattern()))
	h.writeAdmin(w, r, status, out)
}

// Update serves PATCH /v1/admin/invoices/{id} (DRAFT only).
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	var in DraftInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	h.act(w, r, http.StatusOK, func(a Admin, id uuid.UUID) (uuid.UUID, error) {
		inv, err := h.svc.Update(r.Context(), a, id, in)
		return inv.ID, err
	})
}

// Discard serves DELETE /v1/admin/invoices/{id} (DRAFT only).
func (h *Handler) Discard(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	if err := h.svc.Discard(r.Context(), AdminOf(auth.FromContext(r.Context())), id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Issue serves POST /v1/admin/invoices/{id}/issue.
func (h *Handler) Issue(w http.ResponseWriter, r *http.Request) {
	h.act(w, r, http.StatusOK, func(a Admin, id uuid.UUID) (uuid.UUID, error) {
		inv, err := h.svc.Issue(r.Context(), a, id)
		return inv.ID, err
	})
}

// Resend serves POST /v1/admin/invoices/{id}/resend.
func (h *Handler) Resend(w http.ResponseWriter, r *http.Request) {
	h.act(w, r, http.StatusOK, func(a Admin, id uuid.UUID) (uuid.UUID, error) {
		inv, err := h.svc.Resend(r.Context(), a, id)
		return inv.ID, err
	})
}

// RecordPayment serves POST /v1/admin/invoices/{id}/payments.
func (h *Handler) RecordPayment(w http.ResponseWriter, r *http.Request) {
	var in PaymentInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	h.act(w, r, http.StatusCreated, func(a Admin, id uuid.UUID) (uuid.UUID, error) {
		inv, _, err := h.svc.RecordPayment(r.Context(), a, id, in)
		return inv.ID, err
	})
}

// CreditNote serves POST /v1/admin/invoices/{id}/credit-notes.
func (h *Handler) CreditNote(w http.ResponseWriter, r *http.Request) {
	var in CreditInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	h.act(w, r, http.StatusCreated, func(a Admin, id uuid.UUID) (uuid.UUID, error) {
		inv, err := h.svc.CreditNote(r.Context(), a, id, in)
		return inv.ID, err
	})
}

func (h *Handler) reasoned(w http.ResponseWriter, r *http.Request, status int, fn func(a Admin, id uuid.UUID, in ReasonInput) (Invoice, error)) {
	var in ReasonInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	h.act(w, r, status, func(a Admin, id uuid.UUID) (uuid.UUID, error) {
		inv, err := fn(a, id, in)
		return inv.ID, err
	})
}

// Void serves POST /v1/admin/invoices/{id}/void.
func (h *Handler) Void(w http.ResponseWriter, r *http.Request) {
	h.reasoned(w, r, http.StatusOK, func(a Admin, id uuid.UUID, in ReasonInput) (Invoice, error) {
		return h.svc.Void(r.Context(), a, id, in)
	})
}

// Dispute serves POST /v1/admin/invoices/{id}/dispute.
func (h *Handler) Dispute(w http.ResponseWriter, r *http.Request) {
	h.reasoned(w, r, http.StatusOK, func(a Admin, id uuid.UUID, in ReasonInput) (Invoice, error) {
		return h.svc.Dispute(r.Context(), a, id, in)
	})
}

// ResolveDispute serves POST /v1/admin/invoices/{id}/resolve-dispute.
func (h *Handler) ResolveDispute(w http.ResponseWriter, r *http.Request) {
	h.reasoned(w, r, http.StatusOK, func(a Admin, id uuid.UUID, in ReasonInput) (Invoice, error) {
		return h.svc.ResolveDispute(r.Context(), a, id, in)
	})
}

// Reissue serves POST /v1/admin/invoices/{id}/reissue: 201 with the new DRAFT.
func (h *Handler) Reissue(w http.ResponseWriter, r *http.Request) {
	h.act(w, r, http.StatusCreated, func(a Admin, id uuid.UUID) (uuid.UUID, error) {
		inv, err := h.svc.Reissue(r.Context(), a, id)
		return inv.ID, err
	})
}

// ---------------------------------------------------------------------------
// The payer
// ---------------------------------------------------------------------------

// Mine serves GET /v1/me/invoices: the caller's issued invoices.
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	page, size := paging(r)
	items, total, _, err := h.svc.Mine(r.Context(), p, size, (page-1)*size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	views := make([]InvoiceView, 0, len(items))
	for _, inv := range items {
		views = append(views, View(inv))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": views, "page": page, "page_size": size, "total": total, "total_pages": pages(total, size)})
}

// MineOne serves GET /v1/me/invoices/{id}.
func (h *Handler) MineOne(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	d, payer, err := h.svc.MineOne(r.Context(), auth.FromContext(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"invoice": h.customerDetail(d, payer)})
}

// Routes mounts the administrators' routes on r (under /v1/admin/invoices):
// reading for ReadRoles, changes for DecisionRoles through decide.
func (h *Handler) AdminRoutes(r chi.Router, decide func(http.Handler) http.Handler) {
	r.Get("/", h.AdminList)
	r.Get("/{id}", h.AdminGet)
	r.Group(func(r chi.Router) {
		r.Use(decide)
		r.Post("/", h.Create)
		r.Patch("/{id}", h.Update)
		r.Delete("/{id}", h.Discard)
		r.Post("/{id}/issue", h.Issue)
		r.Post("/{id}/resend", h.Resend)
		r.Post("/{id}/payments", h.RecordPayment)
		r.Post("/{id}/credit-notes", h.CreditNote)
		r.Post("/{id}/void", h.Void)
		r.Post("/{id}/reissue", h.Reissue)
		r.Post("/{id}/dispute", h.Dispute)
		r.Post("/{id}/resolve-dispute", h.ResolveDispute)
	})
}
