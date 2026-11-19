package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/paygateway"
)

// ExportRoles may export payments.csv: SUPER_ADMIN and FINANCE_ADMIN.
func ExportRoles() []string { return []string{auth.PlatformSuperAdmin, auth.PlatformFinanceAdmin} }

// ReturnPath is the web app's return page of a subscription's payment
// (built on PUBLIC_WEB_URL; it only shows the status).
const ReturnPath = "/account/subscriptions/%s/payment"

// Deps are the service's collaborators.
type Deps struct {
	Store         Store
	Subscriptions *subscriptions.Service
	// Gateway is the payment provider; nil when online payment is off.
	Gateway paygateway.Gateway
	// PublicWebURL is the web app's origin (return URLs).
	PublicWebURL string
	// ReconcileAfter is how old an open checkout is before reconciliation
	// asks the provider about it (default 15 minutes).
	ReconcileAfter time.Duration
	Logger         *slog.Logger
	Now            func() time.Time
}

// Service is the online payment use cases.
type Service struct{ d Deps }

// NewService builds the service.
func NewService(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.ReconcileAfter <= 0 {
		d.ReconcileAfter = 15 * time.Minute
	}
	d.PublicWebURL = strings.TrimRight(d.PublicWebURL, "/")
	return &Service{d: d}
}

func (s *Service) now() time.Time { return s.d.Now().UTC().Truncate(time.Microsecond) }

// Enabled reports whether online payment is offered.
func (s *Service) Enabled() bool { return s.d.Gateway != nil }

// Provider is the gateway's name ("" when off).
func (s *Service) Provider() string {
	if s.d.Gateway == nil {
		return ""
	}
	return s.d.Gateway.Name()
}

// Gateway returns the gateway (nil when off).
func (s *Service) Gateway() paygateway.Gateway { return s.d.Gateway }

// Store returns the store (wiring and tests).
func (s *Service) Store() Store { return s.d.Store }

var amountRe = regexp.MustCompile(`^[0-9]{1,14}(\.[0-9]{1,4})?$`)

func amountLike(s string) bool { return amountRe.MatchString(s) }

func gatewayActor(provider string) (uuid.UUID, string) {
	return uuid.Nil, subscriptions.GatewayName(provider)
}

// ---------------------------------------------------------------------------
// Checkout
// ---------------------------------------------------------------------------

// CheckoutResult is a checkout handed to the subscriber.
type CheckoutResult struct {
	Payment Payment
	Reused  bool
}

// Checkout creates (or hands out again) the hosted checkout of a
// PENDING_PAYMENT invoice for its subscriber or the organisation's owner.
// baseURL is the API's own origin as the caller reached it (fake driver).
func (s *Service) Checkout(ctx context.Context, p *auth.Principal, subscriptionID uuid.UUID, baseURL string) (CheckoutResult, error) {
	gw := s.d.Gateway
	if gw == nil {
		return CheckoutResult{}, ErrNotOffered
	}
	x, err := s.d.Subscriptions.Owned(ctx, p, subscriptionID)
	if err != nil {
		return CheckoutResult{}, err
	}
	if x.Status != subscriptions.StatusPendingPayment {
		return CheckoutResult{}, subscriptions.ErrState
	}
	now := s.now()
	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}
	opener := p.UserID
	ret := s.d.PublicWebURL + fmt.Sprintf(ReturnPath, x.ID) + "?payment_id=" + id.String()
	cand := Payment{ID: id, SubscriptionID: x.ID, InvoiceNumber: x.InvoiceNumber, SubscriberKind: x.SubscriberKind, SubscriberID: x.SubscriberID,
		SubscriberName: x.SubscriberName, MarketCode: x.MarketCode, Provider: gw.Name(), IdempotencyKey: "rp365-checkout-" + id.String(),
		Amount: x.Amount, Currency: x.CurrencyCode, RefundedAmount: "0.00", Status: StatusOpen, ReturnURL: ret + "&result=returned",
		CancelURL: ret + "&result=cancelled", OpenedBy: &opener, OpenedByName: p.Name, CreatedAt: now, UpdatedAt: now}
	stored, reused, err := s.d.Store.ReserveCheckout(ctx, cand, now)
	if err != nil {
		return CheckoutResult{}, err
	}
	if reused {
		return CheckoutResult{Payment: stored, Reused: true}, nil
	}
	co, err := gw.CreateCheckout(ctx, paygateway.CheckoutRequest{PaymentID: stored.ID.String(), InvoiceNumber: x.InvoiceNumber,
		Description: x.PlanName + " (" + strings.ToLower(x.Period) + ")", Amount: x.Amount, Currency: x.CurrencyCode,
		SuccessURL: stored.ReturnURL, CancelURL: stored.CancelURL, IdempotencyKey: stored.IdempotencyKey, BaseURL: baseURL})
	if err != nil {
		s.d.Logger.ErrorContext(ctx, "payment checkout failed", slog.String("provider", gw.Name()), slog.String("payment_id", stored.ID.String()),
			slog.String("error", err.Error()))
		_, _ = s.d.Store.ChangePayment(context.WithoutCancel(ctx), stored.ID, func(x *Payment) ([]adminaudit.Event, error) {
			t := s.now()
			x.Status, x.FailedAt, x.UpdatedAt, x.FailureReason = StatusFailed, &t, t, "The payment provider could not create the checkout."
			return nil, nil
		})
		return CheckoutResult{}, ErrProvider
	}
	out, err := s.d.Store.ChangePayment(ctx, stored.ID, func(x *Payment) ([]adminaudit.Event, error) {
		exp := co.ExpiresAt.UTC()
		x.ProviderRef, x.HostedURL, x.ExpiresAt, x.UpdatedAt = co.ProviderRef, co.HostedURL, &exp, s.now()
		return []adminaudit.Event{auditOf(p.UserID, p.Name, ActionCheckoutCreated, *x, "", nil,
			map[string]any{"payment_id": x.ID.String(), "provider": x.Provider, "provider_ref": x.ProviderRef, "amount": x.Amount,
				"currency_code": x.Currency, "expires_at": exp}, x.UpdatedAt)}, nil
	})
	if err != nil {
		return CheckoutResult{}, err
	}
	return CheckoutResult{Payment: out}, nil
}

// ---------------------------------------------------------------------------
// Webhooks
// ---------------------------------------------------------------------------

// Webhook verifies and applies a provider's webhook. duplicate is true when
// the event was already processed (nothing changed).
func (s *Service) Webhook(ctx context.Context, provider string, header http.Header, body []byte) (bool, error) {
	gw := s.d.Gateway
	if gw == nil || provider != gw.Name() {
		return false, ErrProviderNotFound
	}
	ev, err := gw.VerifyWebhook(ctx, header, body)
	if err != nil {
		if errors.Is(err, paygateway.ErrInvalidSignature) {
			s.d.Logger.WarnContext(ctx, "payment webhook signature rejected", slog.String("provider", provider))
			return false, ErrInvalidSignature
		}
		return false, &ValidationError{Fields: map[string]string{"body": "is not a webhook this provider sends"}}
	}
	return s.process(ctx, ev)
}

// process applies one verified event, once.
func (s *Service) process(ctx context.Context, ev paygateway.Event) (bool, error) {
	provider := s.d.Gateway.Name()
	rec := EventRecord{ID: uuid.New(), Provider: provider, EventID: ev.ID, Type: ev.Type, ProviderRef: ev.ProviderRef, PaymentRef: ev.PaymentRef,
		RefundRef: ev.RefundRef, Amount: ev.Amount, Currency: ev.Currency, OccurredAt: ev.OccurredAt.UTC(), ReceivedAt: s.now()}
	claimed, err := s.d.Store.ClaimEvent(ctx, rec, rec.ReceivedAt)
	if err != nil {
		return false, err
	}
	if !claimed {
		return true, nil
	}
	outcome, paymentID, err := s.apply(ctx, ev)
	if err != nil {
		_ = s.d.Store.ReleaseEvent(context.WithoutCancel(ctx), provider, ev.ID)
		return false, err
	}
	return false, s.d.Store.FinishEvent(ctx, provider, ev.ID, outcome, paymentID, s.now())
}

func (s *Service) apply(ctx context.Context, ev paygateway.Event) (string, *uuid.UUID, error) {
	provider := s.d.Gateway.Name()
	ref := ev.ProviderRef
	if ref == "" {
		ref = ev.PaymentRef
	}
	p, err := s.d.Store.PaymentByRef(ctx, provider, ref)
	if errors.Is(err, ErrPaymentNotFound) && ev.PaymentRef != "" && ev.PaymentRef != ref {
		p, err = s.d.Store.PaymentByRef(ctx, provider, ev.PaymentRef)
	}
	if errors.Is(err, ErrPaymentNotFound) {
		s.d.Logger.WarnContext(ctx, "payment webhook for an unknown payment", slog.String("provider", provider), slog.String("type", ev.Type))
		return OutcomeUnknownPayment, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	id := p.ID
	var outcome string
	switch ev.Type {
	case paygateway.EventPaymentSucceeded:
		outcome, err = s.succeeded(ctx, p, ev)
	case paygateway.EventPaymentFailed:
		outcome, err = s.failed(ctx, p, ev)
	case paygateway.EventCheckoutExpired:
		outcome, err = s.expire(ctx, p.ID)
	case paygateway.EventRefunded:
		outcome, err = s.refundedAtProvider(ctx, p, ev)
	default:
		outcome = OutcomeIgnored
	}
	return outcome, &id, err
}

func (s *Service) succeeded(ctx context.Context, p Payment, ev paygateway.Event) (string, error) {
	now := s.now()
	at := ev.OccurredAt.UTC()
	if at.IsZero() {
		at = now
	}
	actorID, actorName := gatewayActor(p.Provider)
	changed := false
	p, err := s.d.Store.ChangePayment(ctx, p.ID, func(x *Payment) ([]adminaudit.Event, error) {
		switch x.Status {
		case StatusOpen, StatusFailed, StatusExpired:
		default:
			return nil, errNoChange
		}
		changed = true
		from := x.Status
		paid, ok := subscriptions.ParseAmount(ev.Amount)
		x.PaymentRef, x.PaidCurrency, x.SucceededAt, x.UpdatedAt, x.HostedURL = ev.PaymentRef, ev.Currency, &at, now, ""
		if ok {
			x.PaidAmount = paid
		}
		if !ok || ev.Currency != x.Currency || subscriptions.CompareAmounts(paid, x.Amount) != 0 {
			x.Status = StatusReview
			x.ReviewReason = fmt.Sprintf("Paid %s %s; the invoice is %s %s. Not activated.", ev.Currency, ev.Amount, x.Currency, x.Amount)
			return []adminaudit.Event{auditOf(actorID, actorName, ActionFlagged, *x, x.ReviewReason, map[string]any{"status": from},
				map[string]any{"status": StatusReview, "paid_amount": ev.Amount, "paid_currency_code": ev.Currency, "payment_ref": x.PaymentRef}, now)}, nil
		}
		x.Status = StatusSucceeded
		return []adminaudit.Event{auditOf(actorID, actorName, ActionConfirmed, *x, "", map[string]any{"status": from},
			map[string]any{"status": StatusSucceeded, "paid_amount": paid, "currency_code": x.Currency, "payment_ref": x.PaymentRef,
				"provider_ref": x.ProviderRef}, now)}, nil
	})
	if err != nil {
		return "", err
	}
	if p.Status == StatusSucceeded {
		if err := s.ensureActivated(ctx, p); err != nil {
			return "", err
		}
		p, _ = s.d.Store.Payment(ctx, p.ID)
	}
	switch {
	case p.Status == StatusReview:
		return OutcomeFlagged, nil
	case !changed:
		return OutcomeNoChange, nil
	}
	return OutcomeApplied, nil
}

// ensureActivated activates the subscription a SUCCEEDED payment paid for,
// through the administrator's path. When the subscription is no longer
// waiting (cancelled, rejected, paid otherwise), the payment goes to REVIEW.
func (s *Service) ensureActivated(ctx context.Context, p Payment) error {
	paidAt := s.now()
	if p.SucceededAt != nil {
		paidAt = *p.SucceededAt
	}
	_, err := s.d.Subscriptions.ActivatePaid(ctx, p.SubscriptionID, subscriptions.GatewayPayment{Provider: p.Provider, Reference: p.Reference(),
		Amount: p.PaidAmount, Currency: p.PaidCurrency, PaidAt: paidAt})
	reason := ""
	switch {
	case err == nil:
		return nil
	case errors.Is(err, subscriptions.ErrState):
		x, lerr := s.d.Subscriptions.Store().Subscription(ctx, p.SubscriptionID)
		if lerr != nil {
			return lerr
		}
		if x.Payment != nil && x.Payment.Reference == p.Reference() {
			return nil // activated by this payment already
		}
		reason = fmt.Sprintf("Paid while the subscription was %s; it was not activated. Refund or resolve by hand.", x.Status)
	case errors.Is(err, subscriptions.ErrPaymentMismatch):
		reason = "The payment does not match the invoice; it was not activated."
	default:
		return err
	}
	actorID, actorName := gatewayActor(p.Provider)
	_, err = s.d.Store.ChangePayment(ctx, p.ID, func(x *Payment) ([]adminaudit.Event, error) {
		if x.Status != StatusSucceeded {
			return nil, errNoChange
		}
		now := s.now()
		x.Status, x.ReviewReason, x.UpdatedAt = StatusReview, reason, now
		return []adminaudit.Event{auditOf(actorID, actorName, ActionFlagged, *x, reason, map[string]any{"status": StatusSucceeded},
			map[string]any{"status": StatusReview}, now)}, nil
	})
	return err
}

func (s *Service) failed(ctx context.Context, p Payment, ev paygateway.Event) (string, error) {
	actorID, actorName := gatewayActor(p.Provider)
	changed := false
	_, err := s.d.Store.ChangePayment(ctx, p.ID, func(x *Payment) ([]adminaudit.Event, error) {
		if x.Status != StatusOpen {
			return nil, errNoChange
		}
		changed = true
		now := s.now()
		reason := strings.TrimSpace(ev.Reason)
		if reason == "" {
			reason = "Declined by the payment provider."
		}
		x.Status, x.FailedAt, x.UpdatedAt, x.FailureReason, x.HostedURL = StatusFailed, &now, now, clip(reason, MaxReasonRunes), ""
		return []adminaudit.Event{auditOf(actorID, actorName, ActionFailed, *x, x.FailureReason, map[string]any{"status": StatusOpen},
			map[string]any{"status": StatusFailed, "provider_ref": x.ProviderRef}, now)}, nil
	})
	if err != nil {
		return "", err
	}
	if !changed {
		return OutcomeNoChange, nil
	}
	return OutcomeApplied, nil
}

// expire marks an OPEN checkout EXPIRED.
func (s *Service) expire(ctx context.Context, id uuid.UUID) (string, error) {
	changed := false
	_, err := s.d.Store.ChangePayment(ctx, id, func(x *Payment) ([]adminaudit.Event, error) {
		if x.Status != StatusOpen {
			return nil, errNoChange
		}
		changed = true
		now := s.now()
		x.Status, x.ExpiredAt, x.UpdatedAt, x.HostedURL = StatusExpired, &now, now, ""
		return nil, nil
	})
	if err != nil || !changed {
		return OutcomeNoChange, err
	}
	return OutcomeApplied, nil
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ---------------------------------------------------------------------------
// Refunds
// ---------------------------------------------------------------------------

// RefundInput is the body of POST /v1/admin/subscriptions/{id}/refund.
type RefundInput struct {
	Amount string `json:"amount"`
	Reason string `json:"reason"`
}

// RefundOutcome is a refund with the payment and subscription after it.
type RefundOutcome struct {
	Refund       Refund
	Payment      Payment
	Refunds      []Refund
	Subscription subscriptions.Subscription
}

// refundablePayment returns the subscription's newest payment money was
// received for.
func (s *Service) refundablePayment(ctx context.Context, subscriptionID uuid.UUID) (Payment, error) {
	id := subscriptionID
	list, _, err := s.d.Store.Payments(ctx, Filter{SubscriptionID: &id})
	if err != nil {
		return Payment{}, err
	}
	for _, p := range list {
		if p.Refundable() {
			return p, nil
		}
	}
	return Payment{}, ErrNoGatewayPayment
}

// Refund returns all or part of a subscription's online payment through the
// gateway (SUPER_ADMIN, FINANCE_ADMIN). key is the Idempotency-Key header
// ("" derives one from the subscription, amount and reason). A full refund
// cancels the subscription (subscriptions.RecordRefund).
func (s *Service) Refund(ctx context.Context, p *auth.Principal, subscriptionID uuid.UUID, in RefundInput, key string) (RefundOutcome, error) {
	gw := s.d.Gateway
	if gw == nil {
		return RefundOutcome{}, ErrNotOffered
	}
	fields := map[string]string{}
	amount, ok := subscriptions.ParseAmount(in.Amount)
	if !ok {
		fields["amount"] = "must be a positive number with at most two decimals, as a string"
	}
	reason := strings.TrimSpace(in.Reason)
	if n := utf8.RuneCountInString(reason); n < 3 || n > MaxReasonRunes {
		fields["reason"] = "is required (3 to 1000 characters)"
	}
	key = strings.TrimSpace(key)
	if len(key) > MaxKeyLen {
		fields["Idempotency-Key"] = "must be at most 100 characters"
	}
	if len(fields) > 0 {
		return RefundOutcome{}, &ValidationError{Fields: fields}
	}
	if _, err := s.d.Subscriptions.Store().Subscription(ctx, subscriptionID); err != nil {
		return RefundOutcome{}, err
	}
	pay, err := s.refundablePayment(ctx, subscriptionID)
	if err != nil {
		return RefundOutcome{}, err
	}
	if key == "" {
		sum := sha256.Sum256([]byte(subscriptionID.String() + "|" + amount + "|" + reason))
		key = "derived-" + hex.EncodeToString(sum[:16])
	}
	// Scoped to the payment: the same Idempotency-Key sent for another
	// subscription must start that subscription's refund, never answer with
	// this one's.
	key = "rp365-refund-" + pay.ID.String() + "-" + key
	now := s.now()
	by := p.UserID
	r, existing, err := s.d.Store.CreateRefund(ctx, Refund{ID: uuid.New(), PaymentID: pay.ID, Amount: amount, Currency: pay.PaidCurrency, Reason: reason,
		Status: RefundPending, IdempotencyKey: key, RequestedBy: &by, RequestedByName: p.Name, CreatedAt: now},
		func(cur Payment, refunds []Refund) error {
			if !cur.Refundable() {
				return ErrNoGatewayPayment
			}
			committed := []string{cur.RefundedAmount, amount}
			for _, x := range refunds {
				if x.Status == RefundPending {
					committed = append(committed, x.Amount)
				}
			}
			if subscriptions.CompareAmounts(subscriptions.SumAmounts(committed...), cur.PaidAmount) > 0 {
				return ErrRefundExceeds
			}
			return nil
		})
	if err != nil {
		return RefundOutcome{}, err
	}
	if existing {
		return s.refundOutcome(ctx, r)
	}
	res, gerr := gw.Refund(ctx, paygateway.RefundRequest{PaymentRef: pay.PaymentRef, Amount: amount, Currency: pay.PaidCurrency, IdempotencyKey: key,
		Reason: reason})
	r, _, err = s.d.Store.ChangeRefund(ctx, r.ID, func(x *Refund, py *Payment) ([]adminaudit.Event, error) {
		t := s.now()
		after := map[string]any{"refund_id": x.ID.String(), "amount": x.Amount, "currency_code": x.Currency, "payment_id": py.ID.String()}
		if gerr != nil {
			x.Status, x.FailureReason, x.CompletedAt = RefundFailed, "The payment provider refused the refund.", &t
			// Free the key: the same request may be tried again.
			x.IdempotencyKey = clip(x.IdempotencyKey, 150) + "#failed-" + x.ID.String()
			after["status"] = RefundFailed
		} else {
			x.ProviderRefundRef = res.RefundRef
			after["status"], after["provider_refund_ref"] = res.Status, res.RefundRef
		}
		return []adminaudit.Event{auditOf(p.UserID, p.Name, ActionRefundRequested, *py, reason, nil, after, t)}, nil
	})
	if err != nil {
		return RefundOutcome{}, err
	}
	if gerr != nil {
		s.d.Logger.ErrorContext(ctx, "payment refund failed", slog.String("provider", gw.Name()), slog.String("refund_id", r.ID.String()),
			slog.String("error", gerr.Error()))
		return RefundOutcome{}, ErrProvider
	}
	if res.Status == paygateway.RefundSucceeded {
		if err := s.completeRefund(ctx, r.ID, res.RefundRef, subscriptions.ActorAdmin, p.UserID, p.Name); err != nil {
			return RefundOutcome{}, err
		}
	}
	return s.refundOutcome(ctx, r)
}

func (s *Service) refundOutcome(ctx context.Context, r Refund) (RefundOutcome, error) {
	p, err := s.d.Store.Payment(ctx, r.PaymentID)
	if err != nil {
		return RefundOutcome{}, err
	}
	refunds, err := s.d.Store.Refunds(ctx, []uuid.UUID{p.ID})
	if err != nil {
		return RefundOutcome{}, err
	}
	x, err := s.d.Subscriptions.Store().Subscription(ctx, p.SubscriptionID)
	if err != nil {
		return RefundOutcome{}, err
	}
	for _, cur := range refunds[p.ID] {
		if cur.ID == r.ID {
			r = cur
		}
	}
	return RefundOutcome{Refund: r, Payment: p, Refunds: refunds[p.ID], Subscription: x}, nil
}

// completeRefund records that the provider returned the money: the refund
// SUCCEEDED, the payment's refunded total and status, the PAYMENT_REFUNDED
// audit record, and then the subscription's history (a full refund cancels
// it). Completing twice changes nothing.
func (s *Service) completeRefund(ctx context.Context, refundID uuid.UUID, providerRef, actorType string, actorID uuid.UUID, actorName string) error {
	changed := false
	r, p, err := s.d.Store.ChangeRefund(ctx, refundID, func(x *Refund, py *Payment) ([]adminaudit.Event, error) {
		if x.Status == RefundSucceeded {
			return nil, errNoChange
		}
		changed = true
		now := s.now()
		x.Status, x.CompletedAt, x.FailureReason = RefundSucceeded, &now, ""
		if x.ProviderRefundRef == "" {
			x.ProviderRefundRef = providerRef
		}
		from := py.Status
		py.RefundedAmount = subscriptions.SumAmounts(py.RefundedAmount, x.Amount)
		switch {
		case subscriptions.CompareAmounts(py.RefundedAmount, py.PaidAmount) >= 0:
			py.Status = StatusRefunded
		case py.Status != StatusReview:
			py.Status = StatusPartiallyRefunded
		}
		py.RefundedAt, py.UpdatedAt = &now, now
		return []adminaudit.Event{auditOf(actorID, actorName, ActionRefunded, *py, x.Reason, map[string]any{"status": from},
			map[string]any{"status": py.Status, "refund_id": x.ID.String(), "amount": x.Amount, "currency_code": x.Currency,
				"refunded_amount": py.RefundedAmount, "provider_refund_ref": x.ProviderRefundRef}, now)}, nil
	})
	if err != nil || !changed {
		return err
	}
	_, err = s.d.Subscriptions.RecordRefund(ctx, p.SubscriptionID, subscriptions.RefundRecord{Full: p.Status == StatusRefunded, Amount: r.Amount,
		Currency: r.Currency, Reason: r.Reason, PaymentReference: p.Reference(), ActorType: actorType, ActorID: actorID, ActorName: actorName})
	return err
}

// refundedAtProvider applies a REFUNDED webhook: it completes the refund the
// platform asked for, or records one made at the provider.
func (s *Service) refundedAtProvider(ctx context.Context, p Payment, ev paygateway.Event) (string, error) {
	if ev.RefundRef == "" {
		return OutcomeIgnored, nil
	}
	actorID, actorName := gatewayActor(p.Provider)
	r, err := s.d.Store.RefundByProviderRef(ctx, p.ID, ev.RefundRef)
	if errors.Is(err, ErrRefundNotFound) {
		amount, ok := subscriptions.ParseAmount(ev.Amount)
		if !ok {
			return OutcomeIgnored, nil
		}
		var exceeded bool
		r, _, err = s.d.Store.CreateRefund(ctx, Refund{ID: uuid.New(), PaymentID: p.ID, Amount: amount, Currency: ev.Currency,
			Reason: "Refunded at the payment provider.", Status: RefundPending, ProviderRefundRef: ev.RefundRef,
			IdempotencyKey: "rp365-provider-refund-" + p.ID.String() + "-" + p.Provider + "-" + ev.RefundRef, RequestedByName: actorName, CreatedAt: s.now()},
			func(cur Payment, _ []Refund) error {
				if cur.PaidAmount == "" || subscriptions.CompareAmounts(subscriptions.SumAmounts(cur.RefundedAmount, amount), cur.PaidAmount) > 0 {
					exceeded = true
					return ErrRefundExceeds
				}
				return nil
			})
		if exceeded {
			s.d.Logger.WarnContext(ctx, "payment refund webhook exceeds the payment", slog.String("payment_id", p.ID.String()))
			return OutcomeFlagged, nil
		}
	}
	if err != nil {
		return "", err
	}
	if r.Status == RefundSucceeded {
		return OutcomeNoChange, nil
	}
	return OutcomeApplied, s.completeRefund(ctx, r.ID, ev.RefundRef, subscriptions.ActorGateway, actorID, actorName)
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

// ReconcileResult counts what one reconciliation run did.
type ReconcileResult struct {
	Expired, Checked, Activated, Cancelled int
}

// Reconcile is the worker's job: open checkouts older than ReconcileAfter
// are checked with the provider (FetchPayment; a success or failure is
// applied as a synthetic event through the webhook's path), open checkouts
// past their expiry are EXPIRED, and the follow-ups a crash could have left
// (a SUCCEEDED payment whose subscription still waits, a full refund whose
// subscription is still active) are completed. Idempotent.
func (s *Service) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var out ReconcileResult
	now := s.now()
	open, _, err := s.d.Store.Payments(ctx, Filter{Statuses: []string{StatusOpen}})
	if err != nil {
		return out, err
	}
	gw := s.d.Gateway
	for _, p := range open {
		if gw != nil && p.Provider == gw.Name() && p.ProviderRef != "" && !p.CreatedAt.After(now.Add(-s.d.ReconcileAfter)) {
			out.Checked++
			st, err := gw.FetchPayment(ctx, p.ProviderRef)
			switch {
			case err != nil && !errors.Is(err, paygateway.ErrNotFound):
				s.d.Logger.WarnContext(ctx, "payment reconciliation: provider lookup failed", slog.String("payment_id", p.ID.String()),
					slog.String("error", err.Error()))
			case st.Status == paygateway.StatusSucceeded:
				ev := paygateway.Event{ID: "reconcile:" + p.ProviderRef + ":succeeded", Type: paygateway.EventPaymentSucceeded,
					ProviderRef: p.ProviderRef, PaymentRef: st.PaymentRef, Amount: st.Amount, Currency: st.Currency, OccurredAt: st.OccurredAt}
				if _, err := s.process(ctx, ev); err != nil {
					return out, err
				}
				out.Activated++
				continue
			case st.Status == paygateway.StatusFailed:
				ev := paygateway.Event{ID: "reconcile:" + p.ProviderRef + ":failed", Type: paygateway.EventPaymentFailed, ProviderRef: p.ProviderRef,
					OccurredAt: st.OccurredAt}
				if _, err := s.process(ctx, ev); err != nil {
					return out, err
				}
				continue
			}
		}
		stale := (p.ExpiresAt != nil && !now.Before(*p.ExpiresAt)) || (p.HostedURL == "" && now.Sub(p.CreatedAt) > 10*CreationGrace)
		if stale {
			if o, err := s.expire(ctx, p.ID); err != nil {
				return out, err
			} else if o == OutcomeApplied {
				out.Expired++
			}
		}
	}
	since := now.Add(-FollowUpWindow)
	paid, _, err := s.d.Store.Payments(ctx, Filter{Statuses: []string{StatusSucceeded}, UpdatedSince: since})
	if err != nil {
		return out, err
	}
	for _, p := range paid {
		x, err := s.d.Subscriptions.Store().Subscription(ctx, p.SubscriptionID)
		if err != nil {
			return out, err
		}
		if x.Status == subscriptions.StatusPendingPayment {
			if err := s.ensureActivated(ctx, p); err != nil {
				return out, err
			}
			out.Activated++
		}
	}
	refunded, _, err := s.d.Store.Payments(ctx, Filter{Statuses: []string{StatusRefunded}, UpdatedSince: since})
	if err != nil {
		return out, err
	}
	for _, p := range refunded {
		x, err := s.d.Subscriptions.Store().Subscription(ctx, p.SubscriptionID)
		if err != nil {
			return out, err
		}
		if x.EffectiveStatus(now) == subscriptions.StatusActive && x.Payment != nil && x.Payment.Reference == p.Reference() {
			actorID, actorName := gatewayActor(p.Provider)
			if _, err := s.d.Subscriptions.RecordRefund(ctx, x.ID, subscriptions.RefundRecord{Full: true, Amount: p.RefundedAmount, Currency: p.PaidCurrency,
				Reason: "Refunded in full (reconciliation).", PaymentReference: p.Reference(), ActorType: subscriptions.ActorGateway,
				ActorID: actorID, ActorName: actorName}); err != nil {
				return out, err
			}
			out.Cancelled++
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// Mine returns a subscription's payments to its subscriber (or the
// organisation's owner), newest first.
func (s *Service) Mine(ctx context.Context, p *auth.Principal, subscriptionID uuid.UUID) ([]Payment, error) {
	if _, err := s.d.Subscriptions.Owned(ctx, p, subscriptionID); err != nil {
		return nil, err
	}
	id := subscriptionID
	list, _, err := s.d.Store.Payments(ctx, Filter{SubscriptionID: &id})
	return list, err
}

// OfSubscription returns a subscription's payments with their refunds.
func (s *Service) OfSubscription(ctx context.Context, subscriptionID uuid.UUID) ([]Payment, map[uuid.UUID][]Refund, error) {
	if _, err := s.d.Subscriptions.Store().Subscription(ctx, subscriptionID); err != nil {
		return nil, nil, err
	}
	id := subscriptionID
	return s.withRefunds(ctx, Filter{SubscriptionID: &id})
}

func (s *Service) withRefunds(ctx context.Context, f Filter) ([]Payment, map[uuid.UUID][]Refund, error) {
	list, _, err := s.d.Store.Payments(ctx, f)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	refunds, err := s.d.Store.Refunds(ctx, ids)
	return list, refunds, err
}

// List returns one page of payments for the administrators, with refunds.
func (s *Service) List(ctx context.Context, f Filter) ([]Payment, map[uuid.UUID][]Refund, int, error) {
	for _, st := range f.Statuses {
		if !slices.Contains(Statuses(), st) {
			return nil, nil, 0, &ValidationError{Fields: map[string]string{"status": "must be one of " + strings.Join(Statuses(), ", ")}}
		}
	}
	list, total, err := s.d.Store.Payments(ctx, f)
	if err != nil {
		return nil, nil, 0, err
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	refunds, err := s.d.Store.Refunds(ctx, ids)
	return list, refunds, total, err
}
