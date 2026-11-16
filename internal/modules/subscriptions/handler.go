package subscriptions

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Handler serves /v1/me/subscription-plans, /v1/me/subscriptions,
// /v1/admin/subscription-plans and /v1/admin/subscriptions.
type Handler struct {
	svc    *Service
	logger *slog.Logger
	now    func() time.Time
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger, now: svc.d.Now}
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

var answers = []struct {
	err           error
	status        int
	code, message string
}{
	{ErrPlanNotFound, http.StatusNotFound, CodePlanNotFound, "This plan does not exist."},
	{ErrPlanNotAvailable, http.StatusUnprocessableEntity, CodePlanNotAvailable, "This plan is no longer offered."},
	{ErrPlanRetired, http.StatusConflict, CodePlanRetired, "A retired plan cannot be changed."},
	{ErrNotEligible, http.StatusForbidden, CodePlanNotEligible, "This plan is not offered to your account. It needs an approved professional role it is offered to (and, for an organisation plan, an organisation you own)."},
	{ErrPriceMissing, http.StatusUnprocessableEntity, CodePriceNotAvailable, "This plan has no price for that period in your country's currency."},
	{ErrNotFound, http.StatusNotFound, CodeSubscriptionNotFound, "This subscription does not exist."},
	{ErrState, http.StatusConflict, CodeSubscriptionState, "The subscription's status does not allow this. Reload it and try again."},
	{ErrPendingExists, http.StatusConflict, CodePendingExists, "A subscription is already waiting for payment. Pay its invoice or cancel it first."},
	{ErrActiveExists, http.StatusConflict, CodeActiveExists, "There is already an active subscription. Renew it from 30 days before it ends."},
	{ErrRenewalNotOpen, http.StatusConflict, CodeRenewalNotOpen, "Renewal opens 30 days before the subscription ends."},
	{ErrPaymentTooLow, http.StatusUnprocessableEntity, CodePaymentTooLow, "The payment recorded is less than the invoice amount."},
	{ErrNotOwner, http.StatusForbidden, CodeNotOrganisationOwner, "Only the organisation's owner can take a plan for it."},
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

type featureView struct {
	Code  string `json:"code"`
	Label string `json:"label"`
	Limit *int   `json:"limit"`
}

// PlanView is a plan in the API.
type PlanView struct {
	ID             uuid.UUID     `json:"id"`
	Name           string        `json:"name"`
	Description    string        `json:"description"`
	SubscriberKind string        `json:"subscriber_kind"`
	EligibleRoles  []string      `json:"eligible_roles"`
	Features       []featureView `json:"features"`
	Benefits       []string      `json:"benefits"`
	Prices         []Price       `json:"prices"`
	Status         string        `json:"status"`
	SortOrder      int           `json:"sort_order"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	RetiredAt      *time.Time    `json:"retired_at"`
}

// PlanViewOf builds the view of a plan.
func PlanViewOf(p Plan) PlanView {
	v := PlanView{ID: p.ID, Name: p.Name, Description: p.Description, SubscriberKind: p.SubscriberKind, EligibleRoles: append([]string{}, p.EligibleRoles...),
		Features: []featureView{}, Benefits: append([]string{}, p.Benefits...), Prices: append([]Price{}, p.Prices...), Status: p.Status,
		SortOrder: p.SortOrder, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, RetiredAt: p.RetiredAt}
	for _, f := range p.Features {
		v.Features = append(v.Features, featureView{Code: f.Code, Label: FeatureLabel(f.Code), Limit: f.Limit})
	}
	return v
}

type personRef struct {
	ID   *uuid.UUID `json:"id"`
	Name string     `json:"name"`
}

type paymentView struct {
	Reference      string `json:"reference"`
	Amount         string `json:"amount"`
	ReceivedOn     string `json:"received_on"`
	Note           string `json:"note,omitempty"`
	RecordedByName string `json:"recorded_by_name"`
}

// View is a subscription in the API.
type View struct {
	ID              uuid.UUID    `json:"id"`
	InvoiceNumber   string       `json:"invoice_number"`
	PlanID          uuid.UUID    `json:"plan_id"`
	PlanName        string       `json:"plan_name"`
	Period          string       `json:"period"`
	Status          string       `json:"status"`
	Subscriber      Subscriber   `json:"subscriber"`
	OrganisationID  *uuid.UUID   `json:"organisation_id"`
	RequestedBy     personRef    `json:"requested_by"`
	MarketCode      string       `json:"market_code"`
	CurrencyCode    string       `json:"currency_code"`
	Amount          string       `json:"amount"`
	RenewalOf       *uuid.UUID   `json:"renewal_of"`
	StartsAt        *time.Time   `json:"starts_at"`
	EndsAt          *time.Time   `json:"ends_at"`
	ActivatedAt     *time.Time   `json:"activated_at"`
	RenewalOpensAt  *time.Time   `json:"renewal_opens_at"`
	Payment         *paymentView `json:"payment"`
	RejectedAt      *time.Time   `json:"rejected_at"`
	RejectionReason *string      `json:"rejection_reason"`
	CancelledAt     *time.Time   `json:"cancelled_at"`
	CancelReason    *string      `json:"cancel_reason"`
	CancelledBy     *string      `json:"cancelled_by"`
	RefundDue       bool         `json:"refund_due"`
	RefundNote      *string      `json:"refund_note"`
	ExpiredAt       *time.Time   `json:"expired_at"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
	// PaymentInstructions is set while the invoice waits for payment.
	PaymentInstructions *string `json:"payment_instructions"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ViewOf builds the view of a subscription as of now.
func ViewOf(x Subscription, now time.Time) View {
	v := View{ID: x.ID, InvoiceNumber: x.InvoiceNumber, PlanID: x.PlanID, PlanName: x.PlanName, Period: x.Period, Status: x.EffectiveStatus(now),
		Subscriber: Subscriber{Kind: x.SubscriberKind, ID: x.SubscriberID, Name: x.SubscriberName}, OrganisationID: x.OrganisationID,
		RequestedBy: personRef{ID: x.RequestedBy, Name: x.RequestedByName}, MarketCode: x.MarketCode, CurrencyCode: x.CurrencyCode, Amount: x.Amount,
		RenewalOf: x.RenewalOf, StartsAt: x.StartsAt, EndsAt: x.EndsAt, ActivatedAt: x.ActivatedAt, RejectedAt: x.RejectedAt,
		RejectionReason: optional(x.RejectionReason), CancelledAt: x.CancelledAt, CancelReason: optional(x.CancelReason), CancelledBy: optional(x.CancelledBy),
		RefundDue: x.RefundDue, RefundNote: optional(x.RefundNote), ExpiredAt: x.ExpiredAt, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt}
	if v.Status == StatusExpired && v.ExpiredAt == nil {
		v.ExpiredAt = x.EndsAt
	}
	if v.Status == StatusActive {
		v.RenewalOpensAt = x.RenewalOpensAt()
	}
	if x.Payment != nil {
		v.Payment = &paymentView{Reference: x.Payment.Reference, Amount: x.Payment.Amount, ReceivedOn: x.Payment.ReceivedOn, Note: x.Payment.Note,
			RecordedByName: x.Payment.RecordedByName}
	}
	if x.Status == StatusPendingPayment {
		instructions := PaymentInstructions
		v.PaymentInstructions = &instructions
	}
	return v
}

type eventView struct {
	ID        uuid.UUID `json:"id"`
	Action    string    `json:"action"`
	ActorType string    `json:"actor_type"`
	ActorName string    `json:"actor_name"`
	Note      string    `json:"note"`
	At        time.Time `json:"at"`
}

func eventsView(events []Event) []eventView {
	out := make([]eventView, 0, len(events))
	for _, e := range events {
		out = append(out, eventView{ID: e.ID, Action: e.Action, ActorType: e.ActorType, ActorName: e.ActorName, Note: e.Note, At: e.At.UTC()})
	}
	return out
}

type usageView struct {
	ActiveListings    int  `json:"active_listings"`
	MaxActiveListings *int `json:"max_active_listings"`
	// LimitEnforced is false: the plan's limit is shown, not enforced.
	LimitEnforced bool `json:"limit_enforced"`
}

type detailView struct {
	Subscription View        `json:"subscription"`
	Events       []eventView `json:"events"`
	Usage        usageView   `json:"usage"`
}

func (h *Handler) detail(d Detail) detailView {
	return detailView{Subscription: ViewOf(d.Subscription, h.now().UTC()), Events: eventsView(d.Events),
		Usage: usageView{ActiveListings: d.ActiveListings, MaxActiveListings: d.MaxActiveListings}}
}

type subscriptionPage struct {
	Items    []View `json:"items"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
	Total    int    `json:"total"`
}

func paging(r *http.Request, fields map[string]string) (int, int) {
	page, size := 1, DefaultPageSize
	if raw := r.URL.Query().Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 100000 {
			fields["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > MaxPageSize {
			fields["page_size"] = fmt.Sprintf("must be a whole number between 1 and %d", MaxPageSize)
		} else {
			size = n
		}
	}
	return page, size
}

// ---------------------------------------------------------------------------
// The subscriber's endpoints
// ---------------------------------------------------------------------------

// AvailablePlans handles GET /v1/me/subscription-plans.
func (h *Handler) AvailablePlans(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	plans, currency, err := h.svc.AvailablePlans(r.Context(), p, r.URL.Query().Get("market"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := struct {
		CurrencyCode *string    `json:"currency_code"`
		Plans        []PlanView `json:"plans"`
		Note         string     `json:"payment_note"`
	}{CurrencyCode: optional(currency), Plans: []PlanView{}, Note: PaymentInstructions}
	for _, plan := range plans {
		out.Plans = append(out.Plans, PlanViewOf(plan))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

type subscriberView struct {
	Subscriber Subscriber `json:"subscriber"`
	Current    *View      `json:"current"`
	Upcoming   *View      `json:"upcoming"`
	Pending    *View      `json:"pending"`
	CanRenew   bool       `json:"can_renew"`
	Usage      usageView  `json:"usage"`
	History    []View     `json:"history"`
}

// Mine handles GET /v1/me/subscriptions.
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	states, err := h.svc.Mine(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := h.now().UTC()
	view := func(x *Subscription) *View {
		if x == nil {
			return nil
		}
		v := ViewOf(*x, now)
		return &v
	}
	out := struct {
		Subscribers []subscriberView `json:"subscribers"`
	}{Subscribers: []subscriberView{}}
	for _, st := range states {
		sv := subscriberView{Subscriber: st.Subscriber, Current: view(st.Current), Upcoming: view(st.Upcoming), Pending: view(st.Pending),
			CanRenew: st.CanRenew, Usage: usageView{ActiveListings: st.ActiveListings, MaxActiveListings: st.MaxActiveListings}, History: []View{}}
		for _, x := range st.History {
			sv.History = append(sv.History, ViewOf(x, now))
		}
		out.Subscribers = append(out.Subscribers, sv)
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// MineOne handles GET /v1/me/subscriptions/{id}.
func (h *Handler) MineOne(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	x, events, err := h.svc.MineOne(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"subscription": ViewOf(x, h.now().UTC()), "events": eventsView(events)})
}

func (h *Handler) writeSubscription(w http.ResponseWriter, status int, x Subscription) {
	private(w)
	httpx.WriteJSON(w, status, map[string]any{"subscription": ViewOf(x, h.now().UTC())})
}

// Request handles POST /v1/me/subscriptions.
func (h *Handler) Request(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var in RequestInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	x, err := h.svc.Request(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeSubscription(w, http.StatusCreated, x)
}

// CancelPending handles POST /v1/me/subscriptions/{id}/cancel.
func (h *Handler) CancelPending(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	x, err := h.svc.CancelPending(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeSubscription(w, http.StatusOK, x)
}

// Renew handles POST /v1/me/subscriptions/{id}/renew.
func (h *Handler) Renew(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	var in RenewInput
	if r.ContentLength != 0 && !httpx.DecodeJSON(w, r, &in) {
		return
	}
	x, err := h.svc.Renew(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeSubscription(w, http.StatusCreated, x)
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------

// AdminPlans handles GET /v1/admin/subscription-plans.
func (h *Handler) AdminPlans(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	include := r.URL.Query().Get("include_retired") != "false"
	plans, err := h.svc.Plans(r.Context(), include)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := struct {
		Plans             []PlanView `json:"plans"`
		FeatureCodes      []string   `json:"feature_codes"`
		ProfessionalRoles []string   `json:"professional_roles"`
	}{Plans: []PlanView{}, FeatureCodes: FeatureCodes(), ProfessionalRoles: ProfessionalRoles()}
	for _, plan := range plans {
		out.Plans = append(out.Plans, PlanViewOf(plan))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) writePlan(w http.ResponseWriter, status int, p Plan) {
	private(w)
	httpx.WriteJSON(w, status, map[string]any{"plan": PlanViewOf(p)})
}

// AdminPlan handles GET /v1/admin/subscription-plans/{id}.
func (h *Handler) AdminPlan(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrPlanNotFound)
		return
	}
	plan, err := h.svc.Plan(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writePlan(w, http.StatusOK, plan)
}

// CreatePlan handles POST /v1/admin/subscription-plans.
func (h *Handler) CreatePlan(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var in PlanInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	plan, err := h.svc.CreatePlan(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writePlan(w, http.StatusCreated, plan)
}

// UpdatePlan handles PATCH /v1/admin/subscription-plans/{id}.
func (h *Handler) UpdatePlan(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrPlanNotFound)
		return
	}
	var in PlanInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	plan, err := h.svc.UpdatePlan(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writePlan(w, http.StatusOK, plan)
}

// RetirePlan handles POST /v1/admin/subscription-plans/{id}/retire.
func (h *Handler) RetirePlan(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrPlanNotFound)
		return
	}
	plan, err := h.svc.RetirePlan(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writePlan(w, http.StatusOK, plan)
}

// AdminList handles GET /v1/admin/subscriptions.
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	fields := map[string]string{}
	page, size := paging(r, fields)
	q := r.URL.Query()
	f := Filter{Status: strings.ToUpper(q.Get("status")), Market: strings.ToUpper(q.Get("market")), Period: strings.ToUpper(q.Get("period")),
		SubscriberKind: strings.ToUpper(q.Get("subscriber_kind")), Q: q.Get("q"), Limit: size, Offset: (page - 1) * size}
	if raw := q.Get("plan_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			fields["plan_id"] = "must be a UUID"
		} else {
			f.PlanID = &id
		}
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	items, total, err := h.svc.List(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := h.now().UTC()
	out := subscriptionPage{Items: make([]View, 0, len(items)), Page: page, PageSize: size, Total: total}
	for _, x := range items {
		out.Items = append(out.Items, ViewOf(x, now))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// AdminGet handles GET /v1/admin/subscriptions/{id}.
func (h *Handler) AdminGet(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	d, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, h.detail(d))
}

// Approve handles POST /v1/admin/subscriptions/{id}/approve.
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	var in ApproveInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	d, err := h.svc.Approve(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, h.detail(d))
}

func (h *Handler) decide(w http.ResponseWriter, r *http.Request, act func(p *auth.Principal, id uuid.UUID, in DecisionInput) (Detail, error)) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	var in DecisionInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	d, err := act(p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, h.detail(d))
}

// Reject handles POST /v1/admin/subscriptions/{id}/reject.
func (h *Handler) Reject(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, func(p *auth.Principal, id uuid.UUID, in DecisionInput) (Detail, error) {
		return h.svc.Reject(r.Context(), p, id, in)
	})
}

// Cancel handles POST /v1/admin/subscriptions/{id}/cancel.
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, func(p *auth.Principal, id uuid.UUID, in DecisionInput) (Detail, error) {
		return h.svc.Cancel(r.Context(), p, id, in)
	})
}
