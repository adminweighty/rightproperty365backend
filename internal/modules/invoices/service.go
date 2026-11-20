package invoices

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/payments"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/modules/tax"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
)

// AdminRoles may read invoices (as subscriptions); DecisionRoles change them.
func AdminRoles() []string { return subscriptions.ReadRoles() }

// DecisionRoles create, issue, record payments, credit, void and dispute:
// SUPER_ADMIN and FINANCE_ADMIN.
func DecisionRoles() []string { return subscriptions.DecisionRoles() }

// TaxRules finds the tax rule in force (modules/tax).
type TaxRules interface {
	TaxFor(ctx context.Context, market, appliesTo string, at time.Time) (tax.Rule, bool, error)
}

// Deps are what the service needs.
type Deps struct {
	Store         Store
	Subscriptions *subscriptions.Service
	// Tax may be nil: no tax line.
	Tax TaxRules
	// Zone returns a market's IANA time zone ("" falls back to UTC).
	Zone func(ctx context.Context, market string) string
	// OnlinePayment is the gateway's name ("" when online payment is off).
	OnlinePayment string
	Logger        *slog.Logger
	Now           func() time.Time
}

// Service is the invoice use cases.
type Service struct{ d Deps }

// NewService builds the service.
func NewService(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{d: d}
}

func (s *Service) now() time.Time { return s.d.Now().UTC().Truncate(time.Microsecond) }

// Store returns the store (wiring and tests).
func (s *Service) Store() Store { return s.d.Store }

// OnlinePayment is the gateway's name ("" when off).
func (s *Service) OnlinePayment() string { return s.d.OnlinePayment }

func (s *Service) zone(ctx context.Context, market string) string {
	if s.d.Zone == nil {
		return ""
	}
	return s.d.Zone(ctx, market)
}

func runes(v string) int { return utf8.RuneCountInString(v) }

// Admin is the administrator acting.
type Admin struct {
	UserID uuid.UUID
	Name   string
}

// AdminOf is the acting administrator of a principal.
func AdminOf(p *auth.Principal) Admin { return Admin{UserID: p.UserID, Name: p.Name} }

func (a Admin) sub() subscriptions.Actor { return subscriptions.Actor{UserID: a.UserID, Name: a.Name} }

func label(inv Invoice) string {
	if inv.Number != "" {
		return inv.Number
	}
	return "Draft for " + inv.PayerName
}

func audit(a Admin, action string, inv Invoice, note string, before, after map[string]any, at time.Time) adminaudit.Event {
	return adminaudit.New(a.UserID, a.Name, action, TargetInvoice, inv.ID.String(), label(inv), note, before, after, at)
}

// ---------------------------------------------------------------------------
// Drafts
// ---------------------------------------------------------------------------

// LineInput is one line of a draft.
type LineInput struct {
	Description string `json:"description"`
	Quantity    int    `json:"quantity"`
	UnitAmount  string `json:"unit_amount"`
}

// DraftInput is the body of POST /v1/admin/invoices and of its PATCH.
type DraftInput struct {
	RenewsSubscriptionID string `json:"renews_subscription_id"`
	// Period defaults to the renewed subscription's.
	Period string `json:"period"`
	// DueOn defaults to 14 days after the draft is made (YYYY-MM-DD).
	DueOn *string `json:"due_on"`
	// Terms defaults to DefaultTerms.
	Terms *string `json:"terms"`
	// Lines replace the default line (the plan for the period) when given.
	Lines []LineInput `json:"lines"`
}

func (s *Service) validLines(in []LineInput, fields map[string]string) []Line {
	if len(in) == 0 || len(in) > MaxLines {
		fields["lines"] = fmt.Sprintf("must have 1 to %d lines", MaxLines)
		return nil
	}
	out := make([]Line, 0, len(in))
	for i, l := range in {
		key := fmt.Sprintf("lines.%d", i)
		desc := strings.TrimSpace(l.Description)
		if desc == "" || runes(desc) > MaxLineText {
			fields[key+".description"] = "is required (at most 300 characters)"
		}
		if l.Quantity < 1 || l.Quantity > 1000 {
			fields[key+".quantity"] = "must be 1 to 1000"
		}
		unit, ok := ParseAmount(l.UnitAmount)
		if !ok {
			fields[key+".unit_amount"] = "must be a positive amount with at most two decimals"
		}
		out = append(out, Line{ID: uuid.New(), Description: desc, Quantity: l.Quantity, UnitAmount: unit, Amount: Mul(l.Quantity, unit)})
	}
	return out
}

// applyTax sets the tax line and the total from the subtotal: the market's
// rule for subscriptions in force at, if any.
func (s *Service) applyTax(ctx context.Context, inv *Invoice, at time.Time) error {
	subtotal := "0"
	for _, l := range inv.Lines {
		subtotal = Sum(subtotal, l.Amount)
	}
	inv.Subtotal, inv.Tax, inv.Total = subtotal, nil, subtotal
	if s.d.Tax == nil {
		return nil
	}
	rule, ok, err := s.d.Tax.TaxFor(ctx, inv.MarketCode, tax.AppliesSubscriptions, at)
	if err != nil || !ok {
		return err
	}
	amount, total := ApplyTax(subtotal, rule.Rate, rule.Inclusive)
	inv.Tax = &Tax{Name: rule.TaxName, Rate: rule.Rate, Inclusive: rule.Inclusive, Amount: amount, IsSample: rule.IsSample}
	inv.Total = total
	return nil
}

func parseDay(raw string) (time.Time, bool) {
	t, err := time.Parse(DayLayout, strings.TrimSpace(raw))
	return t, err == nil
}

// Create drafts a renewal invoice for a subscription.
func (s *Service) Create(ctx context.Context, a Admin, in DraftInput) (Invoice, error) {
	fields := map[string]string{}
	renews, err := uuid.Parse(strings.TrimSpace(in.RenewsSubscriptionID))
	if err != nil {
		return Invoice{}, &ValidationError{Fields: map[string]string{"renews_subscription_id": "must be a subscription id"}}
	}
	cur, err := s.d.Subscriptions.Store().Subscription(ctx, renews)
	if errors.Is(err, subscriptions.ErrNotFound) {
		return Invoice{}, &ValidationError{Fields: map[string]string{"renews_subscription_id": "no such subscription"}}
	}
	if err != nil {
		return Invoice{}, err
	}
	now := s.now()
	if !subscriptions.InvoiceableAt(cur, now) {
		return Invoice{}, ErrNotInvoiceable
	}
	period := strings.ToUpper(strings.TrimSpace(in.Period))
	if period == "" {
		period = cur.Period
	}
	if !slices.Contains(subscriptions.Periods(), period) {
		fields["period"] = "must be QUARTERLY or YEARLY"
	}
	from := day(now)
	if cur.EndsAt != nil && cur.EndsAt.After(now) {
		from = day(*cur.EndsAt)
	}
	inv := Invoice{ID: uuid.New(), Status: StatusDraft, MarketCode: cur.MarketCode, CurrencyCode: cur.CurrencyCode, SubscriberKind: cur.SubscriberKind,
		SubscriberID: cur.SubscriberID, OrganisationID: cur.OrganisationID, PayerName: cur.SubscriberName, RenewsSubscriptionID: cur.ID,
		PlanID: cur.PlanID, PlanName: cur.PlanName, Period: period, ServiceFrom: from, ServiceTo: day(subscriptions.End(period, from)),
		DueOn: day(now).AddDate(0, 0, DefaultDueDays), Terms: DefaultTerms, AmountPaid: "0.00", AmountCredited: "0.00",
		CreatedBy: &a.UserID, CreatedByName: a.Name, CreatedAt: now, UpdatedAt: now}
	if err := s.draftFields(ctx, &inv, in, fields, now); err != nil {
		return Invoice{}, err
	}
	if len(fields) > 0 {
		return Invoice{}, &ValidationError{Fields: fields}
	}
	ev := NewEvent(inv.ID, ActionCreated, ActorAdmin, a.UserID, a.Name, "Renewal of "+cur.InvoiceNumber, now)
	if err := s.d.Store.Create(ctx, inv, ev, audit(a, AuditCreated, inv, "", nil, map[string]any{"total": inv.Total, "currency_code": inv.CurrencyCode}, now)); err != nil {
		return Invoice{}, err
	}
	return s.d.Store.Get(ctx, inv.ID)
}

// draftFields applies the editable fields of a draft (lines default to the
// plan for the period at its price).
func (s *Service) draftFields(ctx context.Context, inv *Invoice, in DraftInput, fields map[string]string, now time.Time) error {
	if in.DueOn != nil {
		due, ok := parseDay(*in.DueOn)
		switch {
		case !ok:
			fields["due_on"] = "must be a date (YYYY-MM-DD)"
		case due.Before(day(now)) || due.After(day(now).AddDate(0, 0, MaxDueDays)):
			fields["due_on"] = "must be from today to a year from now"
		default:
			inv.DueOn = due
		}
	}
	if in.Terms != nil {
		terms := strings.TrimSpace(*in.Terms)
		if runes(terms) > MaxTermsRunes {
			fields["terms"] = "must be at most 2000 characters"
		}
		inv.Terms = terms
	}
	if in.Lines != nil {
		inv.Lines = s.validLines(in.Lines, fields)
	} else if len(inv.Lines) == 0 || in.Period != "" {
		plan, err := s.d.Subscriptions.Store().Plan(ctx, inv.PlanID)
		if err != nil {
			return err
		}
		price, ok := plan.PriceFor(inv.CurrencyCode, inv.Period)
		if !ok {
			return ErrPriceMissing
		}
		desc := fmt.Sprintf("%s, %s subscription, %s to %s", inv.PlanName, strings.ToLower(inv.Period),
			inv.ServiceFrom.Format("2 Jan 2006"), inv.ServiceTo.AddDate(0, 0, -1).Format("2 Jan 2006"))
		inv.Lines = []Line{{ID: uuid.New(), Description: desc, Quantity: 1, UnitAmount: price.Amount, Amount: price.Amount}}
	}
	if len(fields) > 0 {
		return nil
	}
	return s.applyTax(ctx, inv, now)
}

// Update changes a DRAFT.
func (s *Service) Update(ctx context.Context, a Admin, id uuid.UUID, in DraftInput) (Invoice, error) {
	now := s.now()
	var verr *ValidationError
	out, err := s.d.Store.Change(ctx, id, func(inv *Invoice) (Change, error) {
		if inv.Status != StatusDraft {
			return Change{}, ErrState
		}
		fields := map[string]string{}
		if p := strings.ToUpper(strings.TrimSpace(in.Period)); p != "" && p != inv.Period {
			if !slices.Contains(subscriptions.Periods(), p) {
				fields["period"] = "must be QUARTERLY or YEARLY"
			} else {
				inv.Period, inv.ServiceTo = p, day(subscriptions.End(p, inv.ServiceFrom))
			}
		} else {
			in.Period = ""
		}
		if err := s.draftFields(ctx, inv, in, fields, now); err != nil {
			return Change{}, err
		}
		if len(fields) > 0 {
			verr = &ValidationError{Fields: fields}
			return Change{}, verr
		}
		inv.UpdatedAt = now
		return Change{Lines: true, Events: []Event{NewEvent(inv.ID, ActionEdited, ActorAdmin, a.UserID, a.Name, "", now)},
			Audit: []adminaudit.Event{audit(a, AuditEdited, *inv, "", nil, map[string]any{"total": inv.Total}, now)}}, nil
	})
	return out, err
}

// Discard deletes a DRAFT (it has no number, so nothing is lost from the sequence).
func (s *Service) Discard(ctx context.Context, a Admin, id uuid.UUID) error {
	inv, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	if inv.Status != StatusDraft {
		return ErrState
	}
	return s.d.Store.Delete(ctx, id, audit(a, AuditDiscarded, inv, "", map[string]any{"status": StatusDraft}, nil, s.now()))
}

// ---------------------------------------------------------------------------
// Issue, resend
// ---------------------------------------------------------------------------

func sentPayload(inv Invoice, kind string, at time.Time) OutboxEvent {
	return OutboxEvent{Type: EventSent, Payload: map[string]any{"invoice_id": inv.ID, "kind": kind, "occurred_at": at}}
}

// Issue numbers a DRAFT, creates the renewal subscription it pays and
// emails it. The tax is worked out again at the moment of issue.
func (s *Service) Issue(ctx context.Context, a Admin, id uuid.UUID) (Invoice, error) {
	inv, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return Invoice{}, err
	}
	if inv.Status != StatusDraft {
		return Invoice{}, ErrState
	}
	now := s.now()
	if inv.DueOn.Before(day(now)) {
		return Invoice{}, &ValidationError{Fields: map[string]string{"due_on": "is in the past: change the due date before issuing"}}
	}
	if err := s.applyTax(ctx, &inv, now); err != nil {
		return Invoice{}, err
	}
	sub, err := s.d.Subscriptions.IssueRenewal(ctx, a.sub(), inv.RenewsSubscriptionID, inv.Period, inv.Total, "")
	switch {
	case errors.Is(err, subscriptions.ErrPendingExists), errors.Is(err, subscriptions.ErrActiveExists):
		return Invoice{}, ErrRenewalExists
	case errors.Is(err, subscriptions.ErrState):
		return Invoice{}, ErrNotInvoiceable
	case err != nil:
		return Invoice{}, err
	}
	issued, err := s.d.Store.Change(ctx, id, func(x *Invoice) (Change, error) {
		if x.Status != StatusDraft {
			return Change{}, ErrState
		}
		x.Subtotal, x.Tax, x.Total = inv.Subtotal, inv.Tax, inv.Total
		today := day(now)
		subID := sub.ID
		x.Status, x.SubscriptionID, x.IssueOn, x.IssuedBy = StatusIssued, &subID, &today, &a.UserID
		x.SentAt, x.LastSentAt, x.SendCount, x.UpdatedAt = &now, &now, 1, now
		return Change{Number: true,
			Events: []Event{NewEvent(x.ID, ActionIssued, ActorAdmin, a.UserID, a.Name, "Subscription "+sub.InvoiceNumber, now)},
			Audit: []adminaudit.Event{audit(a, AuditIssued, *x, "", map[string]any{"status": StatusDraft},
				map[string]any{"status": StatusIssued, "total": x.Total, "currency_code": x.CurrencyCode, "subscription": sub.InvoiceNumber}, now)},
			Outbox: []OutboxEvent{sentPayload(*x, ActionIssued, now)}}, nil
	})
	if err != nil {
		// Undo the renewal the invoice was going to pay.
		if cerr := s.d.Subscriptions.CancelInvoiced(ctx, a.sub(), sub.ID, "The invoice could not be issued."); cerr != nil {
			s.d.Logger.ErrorContext(ctx, "invoice issue: renewal not cancelled", slog.String("subscription_id", sub.ID.String()), slog.String("error", cerr.Error()))
		}
		return Invoice{}, err
	}
	return issued, nil
}

// Resend emails an unpaid invoice again (at most every ResendInterval).
func (s *Service) Resend(ctx context.Context, a Admin, id uuid.UUID) (Invoice, error) {
	now := s.now()
	return s.d.Store.Change(ctx, id, func(inv *Invoice) (Change, error) {
		if !payable(inv.Status) {
			return Change{}, ErrState
		}
		if inv.LastSentAt != nil && now.Sub(*inv.LastSentAt) < ResendInterval {
			return Change{}, ErrResendTooSoon
		}
		inv.LastSentAt, inv.SendCount, inv.UpdatedAt = &now, inv.SendCount+1, now
		return Change{Events: []Event{NewEvent(inv.ID, ActionResent, ActorAdmin, a.UserID, a.Name, "", now)},
			Audit:  []adminaudit.Event{audit(a, AuditResent, *inv, "", nil, map[string]any{"send_count": inv.SendCount}, now)},
			Outbox: []OutboxEvent{sentPayload(*inv, ActionResent, now)}}, nil
	})
}

// ---------------------------------------------------------------------------
// Payments, credit notes
// ---------------------------------------------------------------------------

// PaymentInput is the body of POST /v1/admin/invoices/{id}/payments.
type PaymentInput struct {
	Amount     string `json:"amount"`
	Method     string `json:"method"`
	Reference  string `json:"reference"`
	ReceivedOn string `json:"received_on"`
}

func paymentPayload(inv Invoice, p Payment, at time.Time) OutboxEvent {
	return OutboxEvent{Type: EventPaymentRecorded, Payload: map[string]any{"invoice_id": inv.ID, "payment_id": p.ID, "occurred_at": at}}
}

// settle applies a payment to inv: the totals, the status, the history.
func settle(inv *Invoice, p Payment, actorType string, actorID uuid.UUID, actorName string, now time.Time) []Event {
	inv.AmountPaid, inv.UpdatedAt = Sum(inv.AmountPaid, p.Amount), now
	note := fmt.Sprintf("%s %s, %s", inv.CurrencyCode, p.Amount, methodLabel(p.Method))
	if p.Reference != "" {
		note += ", reference " + p.Reference
	}
	evs := []Event{NewEvent(inv.ID, ActionPaymentRecorded, actorType, actorID, actorName, note, now)}
	if Compare(inv.Balance(), "0") == 0 {
		inv.Status = StatusPaid
		evs = append(evs, NewEvent(inv.ID, ActionPaid, actorType, actorID, actorName, "", now))
	} else if inv.Status == StatusIssued {
		inv.Status = StatusPartPaid
	}
	return evs
}

var methodLabels = map[string]string{
	MethodBankTransfer: "bank transfer", MethodCash: "cash", MethodCardOffline: "card (offline)", MethodGateway: "online payment", MethodOther: "other",
}

func methodLabel(m string) string {
	if l, ok := methodLabels[m]; ok {
		return l
	}
	return strings.ToLower(m)
}

// MethodLabel is a payment method's wording.
func MethodLabel(m string) string { return methodLabel(m) }

// RecordPayment records a full or part payment made offline. The payment
// that settles the invoice activates its renewal subscription first (the
// same path as an administrator's approval), so a settled invoice never
// waits on its subscription; if storing the payment then fails, the
// subscription's activation event settles the invoice (Settle).
func (s *Service) RecordPayment(ctx context.Context, a Admin, id uuid.UUID, in PaymentInput) (Invoice, Payment, error) {
	fields := map[string]string{}
	amount, ok := ParseAmount(in.Amount)
	if !ok {
		fields["amount"] = "must be a positive amount with at most two decimals"
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if !slices.Contains(OfflineMethods(), method) {
		fields["method"] = "must be one of " + strings.Join(OfflineMethods(), ", ")
	}
	ref := strings.TrimSpace(in.Reference)
	if len(ref) > MaxReferenceLen {
		fields["reference"] = "must be at most 100 characters"
	}
	now := s.now()
	received, okDay := parseDay(in.ReceivedOn)
	switch {
	case !okDay:
		fields["received_on"] = "must be a date (YYYY-MM-DD)"
	case received.After(day(now)):
		fields["received_on"] = "cannot be in the future"
	}
	if len(fields) > 0 {
		return Invoice{}, Payment{}, &ValidationError{Fields: fields}
	}
	inv, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return Invoice{}, Payment{}, err
	}
	if !payable(inv.Status) {
		return Invoice{}, Payment{}, ErrState
	}
	if Compare(amount, inv.Balance()) > 0 {
		return Invoice{}, Payment{}, ErrOverpayment
	}
	if Compare(amount, inv.Balance()) == 0 && inv.SubscriptionID != nil {
		pay := subscriptions.Payment{Reference: inv.Number, Amount: Sum(inv.AmountPaid, amount), ReceivedOn: received.Format(DayLayout),
			Note: "Invoice " + inv.Number + " paid in full."}
		if _, err := s.d.Subscriptions.ActivateInvoiced(ctx, a.sub(), *inv.SubscriptionID, pay); err != nil && !errors.Is(err, subscriptions.ErrState) {
			return Invoice{}, Payment{}, err
		}
	}
	p := Payment{ID: uuid.New(), InvoiceID: id, Amount: amount, Method: method, Reference: ref, ReceivedOn: received,
		RecordedBy: &a.UserID, RecordedByName: a.Name, CreatedAt: now}
	out, err := s.d.Store.Change(ctx, id, func(x *Invoice) (Change, error) {
		if !payable(x.Status) {
			return Change{}, ErrState
		}
		if Compare(amount, x.Balance()) > 0 {
			return Change{}, ErrOverpayment
		}
		before := x.Status
		evs := settle(x, p, ActorAdmin, a.UserID, a.Name, now)
		return Change{Payment: &p, Events: evs,
			Audit: []adminaudit.Event{audit(a, AuditPaymentRecorded, *x, ref, map[string]any{"status": before},
				map[string]any{"status": x.Status, "amount": amount, "method": method, "balance": x.Balance()}, now)},
			Outbox: []OutboxEvent{paymentPayload(*x, p, now)}}, nil
	})
	if err != nil {
		return Invoice{}, Payment{}, err
	}
	stored, err := s.payment(ctx, id, p.ID)
	return out, stored, err
}

func (s *Service) payment(ctx context.Context, invoiceID, paymentID uuid.UUID) (Payment, error) {
	list, err := s.d.Store.Payments(ctx, invoiceID)
	if err != nil {
		return Payment{}, err
	}
	for _, p := range list {
		if p.ID == paymentID {
			return p, nil
		}
	}
	return Payment{}, ErrNotFound
}

// CreditInput is the body of POST /v1/admin/invoices/{id}/credit-notes.
type CreditInput struct {
	Amount string `json:"amount"`
	Reason string `json:"reason"`
}

func reasonOf(raw string, fields map[string]string) string {
	r := strings.TrimSpace(raw)
	if runes(r) < 3 || runes(r) > MaxReasonRunes {
		fields["reason"] = "is required (3 to 1000 characters)"
	}
	return r
}

// CreditNote lowers what is due on an issued invoice. A credit that leaves
// nothing due settles it: PAID when something was paid (the renewal is
// activated), VOID when nothing was (the renewal is cancelled).
func (s *Service) CreditNote(ctx context.Context, a Admin, id uuid.UUID, in CreditInput) (Invoice, error) {
	fields := map[string]string{}
	amount, ok := ParseAmount(in.Amount)
	if !ok {
		fields["amount"] = "must be a positive amount with at most two decimals"
	}
	reason := reasonOf(in.Reason, fields)
	if len(fields) > 0 {
		return Invoice{}, &ValidationError{Fields: fields}
	}
	now := s.now()
	inv, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return Invoice{}, err
	}
	if !payable(inv.Status) {
		return Invoice{}, ErrState
	}
	if Compare(amount, inv.Balance()) > 0 {
		return Invoice{}, ErrOverpayment
	}
	clears := Compare(amount, inv.Balance()) == 0
	paidSome := Compare(inv.AmountPaid, "0") > 0
	if clears && paidSome && inv.SubscriptionID != nil {
		pay := subscriptions.Payment{Reference: inv.Number, Amount: inv.AmountPaid, ReceivedOn: day(now).Format(DayLayout),
			Note: "Invoice " + inv.Number + " settled with a credit note."}
		if _, err := s.d.Subscriptions.ActivateInvoiced(ctx, a.sub(), *inv.SubscriptionID, pay); err != nil && !errors.Is(err, subscriptions.ErrState) {
			return Invoice{}, err
		}
	}
	c := CreditNote{ID: uuid.New(), InvoiceID: id, Amount: amount, Reason: reason, CreatedBy: &a.UserID, CreatedByName: a.Name, CreatedAt: now}
	out, err := s.d.Store.Change(ctx, id, func(x *Invoice) (Change, error) {
		if !payable(x.Status) || Compare(amount, x.Balance()) > 0 {
			return Change{}, ErrState
		}
		before := x.Status
		x.AmountCredited, x.UpdatedAt = Sum(x.AmountCredited, amount), now
		evs := []Event{NewEvent(x.ID, ActionCreditNote, ActorAdmin, a.UserID, a.Name, fmt.Sprintf("%s %s: %s", x.CurrencyCode, amount, reason), now)}
		if Compare(x.Balance(), "0") == 0 {
			if Compare(x.AmountPaid, "0") > 0 {
				x.Status = StatusPaid
				evs = append(evs, NewEvent(x.ID, ActionPaid, ActorAdmin, a.UserID, a.Name, "", now))
			} else {
				x.Status, x.VoidReason = StatusVoid, "Credited in full: "+reason
				evs = append(evs, NewEvent(x.ID, ActionVoided, ActorAdmin, a.UserID, a.Name, x.VoidReason, now))
			}
		}
		return Change{Credit: &c, Events: evs,
			Audit: []adminaudit.Event{audit(a, AuditCreditNote, *x, reason, map[string]any{"status": before},
				map[string]any{"status": x.Status, "credited": amount, "balance": x.Balance()}, now)}}, nil
	})
	if err != nil {
		return Invoice{}, err
	}
	if out.Status == StatusVoid && out.SubscriptionID != nil {
		s.cancelRenewal(ctx, a, *out.SubscriptionID, "Invoice "+out.Number+" credited in full.")
	}
	return out, nil
}

func (s *Service) cancelRenewal(ctx context.Context, a Admin, subscriptionID uuid.UUID, reason string) {
	if err := s.d.Subscriptions.CancelInvoiced(ctx, a.sub(), subscriptionID, reason); err != nil && !errors.Is(err, subscriptions.ErrState) {
		s.d.Logger.ErrorContext(ctx, "invoice: renewal not cancelled", slog.String("subscription_id", subscriptionID.String()), slog.String("error", err.Error()))
	}
}

// ---------------------------------------------------------------------------
// Void, reissue, disputes
// ---------------------------------------------------------------------------

// ReasonInput is the body of void, dispute and resolve.
type ReasonInput struct {
	Reason string `json:"reason"`
}

// Void cancels an unpaid issued invoice (and its renewal). An invoice with
// payments is credited instead.
func (s *Service) Void(ctx context.Context, a Admin, id uuid.UUID, in ReasonInput) (Invoice, error) {
	fields := map[string]string{}
	reason := reasonOf(in.Reason, fields)
	if len(fields) > 0 {
		return Invoice{}, &ValidationError{Fields: fields}
	}
	now := s.now()
	out, err := s.d.Store.Change(ctx, id, func(x *Invoice) (Change, error) {
		if !payable(x.Status) {
			return Change{}, ErrState
		}
		if Compare(x.AmountPaid, "0") > 0 {
			return Change{}, ErrHasPayments
		}
		before := x.Status
		x.Status, x.VoidReason, x.UpdatedAt = StatusVoid, reason, now
		return Change{Events: []Event{NewEvent(x.ID, ActionVoided, ActorAdmin, a.UserID, a.Name, reason, now)},
			Audit: []adminaudit.Event{audit(a, AuditVoided, *x, reason, map[string]any{"status": before}, map[string]any{"status": StatusVoid}, now)}}, nil
	})
	if err != nil {
		return Invoice{}, err
	}
	if out.SubscriptionID != nil {
		s.cancelRenewal(ctx, a, *out.SubscriptionID, "Invoice "+out.Number+" voided: "+reason)
	}
	return out, nil
}

// Reissue copies a VOID invoice into a new DRAFT (same payer, subscription,
// period and lines; new dates) that replaces it.
func (s *Service) Reissue(ctx context.Context, a Admin, id uuid.UUID) (Invoice, error) {
	old, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return Invoice{}, err
	}
	if old.Status != StatusVoid {
		return Invoice{}, ErrState
	}
	now := s.now()
	inv := old
	oldID := old.ID
	inv.ID, inv.Number, inv.Status, inv.StatusBeforeDispute = uuid.New(), "", StatusDraft, ""
	inv.SubscriptionID, inv.IssueOn, inv.IssuedBy, inv.VoidReason, inv.DisputeReason = nil, nil, nil, "", ""
	inv.SentAt, inv.LastSentAt, inv.SendCount, inv.OverdueNotifiedAt = nil, nil, 0, nil
	inv.AmountPaid, inv.AmountCredited, inv.ReplacesInvoiceID = "0.00", "0.00", &oldID
	inv.DueOn = day(now).AddDate(0, 0, DefaultDueDays)
	inv.CreatedBy, inv.CreatedByName, inv.CreatedAt, inv.UpdatedAt = &a.UserID, a.Name, now, now
	lines := make([]Line, len(old.Lines))
	for i, l := range old.Lines {
		l.ID = uuid.New()
		lines[i] = l
	}
	inv.Lines = lines
	if err := s.applyTax(ctx, &inv, now); err != nil {
		return Invoice{}, err
	}
	ev := NewEvent(inv.ID, ActionCreated, ActorAdmin, a.UserID, a.Name, "Reissue of "+old.Number, now)
	if err := s.d.Store.Create(ctx, inv, ev, audit(a, AuditReissued, inv, "", map[string]any{"replaces": old.Number}, map[string]any{"total": inv.Total}, now)); err != nil {
		return Invoice{}, err
	}
	if _, err := s.d.Store.Change(ctx, oldID, func(x *Invoice) (Change, error) {
		return Change{Events: []Event{NewEvent(x.ID, ActionReissued, ActorAdmin, a.UserID, a.Name, "Replaced by a new draft", now)}}, nil
	}); err != nil {
		return Invoice{}, err
	}
	return s.d.Store.Get(ctx, inv.ID)
}

// Dispute marks an invoice disputed (a contested charge, a chargeback, a
// refund under review); payments and emails stop until it is resolved.
func (s *Service) Dispute(ctx context.Context, a Admin, id uuid.UUID, in ReasonInput) (Invoice, error) {
	fields := map[string]string{}
	reason := reasonOf(in.Reason, fields)
	if len(fields) > 0 {
		return Invoice{}, &ValidationError{Fields: fields}
	}
	return s.dispute(ctx, id, ActorAdmin, a, reason)
}

func (s *Service) dispute(ctx context.Context, id uuid.UUID, actorType string, a Admin, reason string) (Invoice, error) {
	now := s.now()
	return s.d.Store.Change(ctx, id, func(x *Invoice) (Change, error) {
		if !payable(x.Status) && x.Status != StatusPaid {
			return Change{}, ErrState
		}
		before := x.Status
		x.StatusBeforeDispute, x.Status, x.DisputeReason, x.UpdatedAt = before, StatusDisputed, reason, now
		return Change{Events: []Event{NewEvent(x.ID, ActionDisputed, actorType, a.UserID, a.Name, reason, now)},
			Audit: []adminaudit.Event{audit(a, AuditDisputed, *x, reason, map[string]any{"status": before}, map[string]any{"status": StatusDisputed}, now)}}, nil
	})
}

// ResolveDispute puts a DISPUTED invoice back where it was.
func (s *Service) ResolveDispute(ctx context.Context, a Admin, id uuid.UUID, in ReasonInput) (Invoice, error) {
	fields := map[string]string{}
	reason := reasonOf(in.Reason, fields)
	if len(fields) > 0 {
		return Invoice{}, &ValidationError{Fields: fields}
	}
	now := s.now()
	return s.d.Store.Change(ctx, id, func(x *Invoice) (Change, error) {
		if x.Status != StatusDisputed || x.StatusBeforeDispute == "" {
			return Change{}, ErrState
		}
		x.Status, x.StatusBeforeDispute, x.UpdatedAt = x.StatusBeforeDispute, "", now
		return Change{Events: []Event{NewEvent(x.ID, ActionDisputeResolved, ActorAdmin, a.UserID, a.Name, reason, now)},
			Audit: []adminaudit.Event{audit(a, AuditDisputeResolved, *x, reason, map[string]any{"status": StatusDisputed}, map[string]any{"status": x.Status}, now)}}, nil
	})
}

// ---------------------------------------------------------------------------
// The worker: overdue, settlement by the gateway, refunds
// ---------------------------------------------------------------------------

// SweepInterval is how often the overdue sweep runs.
const SweepInterval = time.Hour

// Sweep marks OVERDUE every ISSUED or PART_PAID invoice whose due date has
// passed in its market's time zone, once, with its reminder email.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	now := s.now()
	ids, err := s.d.Store.DueCandidates(ctx, day(now))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		inv, err := s.d.Store.Get(ctx, id)
		if err != nil {
			return n, err
		}
		if !LocalDay(now, s.zone(ctx, inv.MarketCode)).After(inv.DueOn) {
			continue
		}
		_, err = s.d.Store.Change(ctx, id, func(x *Invoice) (Change, error) {
			if x.Status != StatusIssued && x.Status != StatusPartPaid {
				return Change{}, ErrState
			}
			x.Status, x.OverdueNotifiedAt, x.UpdatedAt = StatusOverdue, &now, now
			return Change{Events: []Event{NewEvent(x.ID, ActionOverdue, ActorSystem, uuid.Nil, "RightProperty365", "Due "+x.DueOn.Format(DayLayout), now)},
				Outbox: []OutboxEvent{{Type: EventOverdue, Payload: map[string]any{"invoice_id": x.ID, "occurred_at": now}}}}, nil
		})
		switch {
		case errors.Is(err, ErrState):
		case err != nil:
			return n, err
		default:
			n++
		}
	}
	return n, nil
}

// Settle is the handler of SUBSCRIPTION_ACTIVATED: when the activated
// subscription is an open invoice's renewal (paid online, or approved on the
// subscription's own page), the invoice is paid with what remains due, once
// (the source key makes a replay a no-op).
func (s *Service) Settle(ctx context.Context, subscriptionID uuid.UUID) error {
	inv, err := s.d.Store.BySubscription(ctx, subscriptionID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !payable(inv.Status) {
		return nil
	}
	sub, err := s.d.Subscriptions.Store().Subscription(ctx, subscriptionID)
	if err != nil {
		return err
	}
	if sub.Status != subscriptions.StatusActive || sub.Payment == nil {
		return nil
	}
	now := s.now()
	method, actorType, name := MethodOther, ActorSystem, "RightProperty365"
	if sub.Payment.RecordedByKind == subscriptions.RecordedByGateway {
		method, actorType, name = MethodGateway, ActorGateway, subscriptions.GatewayName(sub.Payment.Provider)
	}
	received, ok := parseDay(sub.Payment.ReceivedOn)
	if !ok {
		received = day(now)
	}
	p := Payment{ID: uuid.New(), InvoiceID: inv.ID, Amount: inv.Balance(), Method: method, Reference: sub.Payment.Reference,
		ReceivedOn: received, SourceKey: "activation:" + subscriptionID.String(), RecordedByName: name, CreatedAt: now}
	_, err = s.d.Store.Change(ctx, inv.ID, func(x *Invoice) (Change, error) {
		if !payable(x.Status) || Compare(x.Balance(), "0") <= 0 {
			return Change{}, ErrState
		}
		p.Amount = x.Balance()
		evs := settle(x, p, actorType, uuid.Nil, name, now)
		return Change{Payment: &p, Events: evs, Outbox: []OutboxEvent{paymentPayload(*x, p, now)}}, nil
	})
	if errors.Is(err, ErrState) || errors.Is(err, ErrDuplicatePayment) {
		return nil
	}
	return err
}

// ErrDuplicatePayment is a payment with a source key already recorded.
var ErrDuplicatePayment = errors.New("payment already recorded")

// RefundCancelled is the handler of SUBSCRIPTION_CANCELLED: a renewal
// cancelled by a full refund through the gateway puts its PAID invoice under
// review (DISPUTED), as the Handover asks for refunds.
func (s *Service) RefundCancelled(ctx context.Context, subscriptionID uuid.UUID) error {
	inv, err := s.d.Store.BySubscription(ctx, subscriptionID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if inv.Status != StatusPaid {
		return nil
	}
	sub, err := s.d.Subscriptions.Store().Subscription(ctx, subscriptionID)
	if err != nil {
		return err
	}
	if sub.Status != subscriptions.StatusCancelled || !strings.HasPrefix(sub.CancelReason, "Refunded in full") {
		return nil
	}
	_, err = s.dispute(ctx, inv.ID, ActorSystem, Admin{Name: "RightProperty365"}, "Refunded in full through the payment gateway: review the invoice.")
	if errors.Is(err, ErrState) {
		return nil
	}
	return err
}

// CheckoutGuard refuses the online checkout of an invoice that was partly
// paid or credited offline (payments.Deps.CheckoutGuard).
func (s *Service) CheckoutGuard(ctx context.Context, subscriptionID uuid.UUID) error {
	inv, err := s.d.Store.BySubscription(ctx, subscriptionID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if Compare(inv.AmountPaid, "0") > 0 || Compare(inv.AmountCredited, "0") > 0 || !payable(inv.Status) {
		return payments.ErrPaidOffline
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// Detail is an invoice with its payments, credit notes and history.
type Detail struct {
	Invoice  Invoice
	Payments []Payment
	Credits  []CreditNote
	Events   []Event
}

// Get returns an invoice's detail.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Detail, error) {
	inv, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Invoice: inv}
	if d.Payments, err = s.d.Store.Payments(ctx, id); err != nil {
		return Detail{}, err
	}
	if d.Credits, err = s.d.Store.Credits(ctx, id); err != nil {
		return Detail{}, err
	}
	if d.Events, err = s.d.Store.Events(ctx, id); err != nil {
		return Detail{}, err
	}
	return d, nil
}

// List returns one page of invoices.
func (s *Service) List(ctx context.Context, f Filter) ([]Invoice, int, error) {
	if f.Limit <= 0 || f.Limit > MaxPageSize {
		f.Limit = DefaultPageSize
	}
	return s.d.Store.List(ctx, f)
}

// Payer is how the caller may see an invoice: as its payer (the person, or
// an OWNER or MANAGER of the organisation), and whether they may also pay it
// online (the person, or the organisation's OWNER: who may open the
// subscription page).
type Payer struct {
	Subscribers []uuid.UUID
	CanPay      map[uuid.UUID]bool
}

// PayerOf is the subscribers the caller may see the invoices of.
func PayerOf(p *auth.Principal) Payer {
	out := Payer{Subscribers: []uuid.UUID{p.UserID}, CanPay: map[uuid.UUID]bool{p.UserID: true}}
	for _, m := range p.Memberships {
		if m.Role == auth.RoleOwner || m.Role == auth.RoleManager {
			out.Subscribers = append(out.Subscribers, m.OrganisationID)
			out.CanPay[m.OrganisationID] = m.Role == auth.RoleOwner
		}
	}
	return out
}

// Mine returns one page of the caller's issued invoices (never drafts).
func (s *Service) Mine(ctx context.Context, p *auth.Principal, limit, offset int) ([]Invoice, int, Payer, error) {
	payer := PayerOf(p)
	items, total, err := s.List(ctx, Filter{Subscribers: payer.Subscribers, ExcludeDrafts: true, Limit: limit, Offset: offset})
	return items, total, payer, err
}

// MineOne returns one of the caller's issued invoices; ErrNotFound for a
// draft or anybody else's.
func (s *Service) MineOne(ctx context.Context, p *auth.Principal, id uuid.UUID) (Detail, Payer, error) {
	d, err := s.Get(ctx, id)
	if err != nil {
		return Detail{}, Payer{}, err
	}
	payer := PayerOf(p)
	if d.Invoice.Status == StatusDraft || !slices.Contains(payer.Subscribers, d.Invoice.SubscriberID) {
		return Detail{}, Payer{}, ErrNotFound
	}
	return d, payer, nil
}

// ---------------------------------------------------------------------------
// Notices for the emails
// ---------------------------------------------------------------------------

// Notice is what the invoice emails need, read at send time.
type Notice struct {
	Invoice Invoice
	// Payment is the payment of an INVOICE_PAYMENT_RECORDED event.
	Payment *Payment
	// PaidUntil is the end of the renewed period once the invoice is paid.
	PaidUntil *time.Time
	// PayOnline: a gateway is configured.
	PayOnline bool
}

// Notice loads the data of an invoice email.
func (s *Service) Notice(ctx context.Context, invoiceID, paymentID uuid.UUID) (Notice, error) {
	inv, err := s.d.Store.Get(ctx, invoiceID)
	if err != nil {
		return Notice{}, err
	}
	n := Notice{Invoice: inv, PayOnline: s.d.OnlinePayment != ""}
	if paymentID != uuid.Nil {
		p, err := s.payment(ctx, invoiceID, paymentID)
		if err != nil {
			return Notice{}, err
		}
		n.Payment = &p
	}
	if inv.Status == StatusPaid && inv.SubscriptionID != nil {
		if sub, err := s.d.Subscriptions.Store().Subscription(ctx, *inv.SubscriptionID); err == nil && sub.EndsAt != nil {
			end := *sub.EndsAt
			n.PaidUntil = &end
		}
	}
	return n, nil
}
