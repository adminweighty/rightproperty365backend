package subscriptions

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
)

// The subscriptions side of the finance administrators' renewal invoices
// (API 0.47.0, modules/invoices). An issued invoice creates the renewal
// subscription it pays (PENDING_PAYMENT, renewal_of the subscription it
// renews); settling the invoice activates it through the same path as
// Approve and the payment gateway; voiding it cancels it.

// Actor is who acts on an invoiced renewal: a finance administrator.
type Actor struct {
	UserID uuid.UUID
	Name   string
}

// RenewalWindowAfterEnd is how long after its end a subscription can still
// be renewed by an invoice.
const RenewalWindowAfterEnd = 90 * 24 * time.Hour

// SubscribersOf returns the subscribers the caller may act for: themselves
// and the organisations they own (the rule of the customer's own routes).
func SubscribersOf(p *auth.Principal) []Subscriber { return subscribers(p) }

// IssueRenewal creates the PENDING_PAYMENT renewal of the subscription
// renewsID for an invoice: the same plan, the given period and amount (the
// invoice total), requested by the person who requested the renewed one (so
// the activation email reaches the subscriber, not the administrator). The
// history records INVOICED with the invoice number; no subscription-invoice
// email is sent. ErrPendingExists / ErrActiveExists as for Renew.
func (s *Service) IssueRenewal(ctx context.Context, by Actor, renewsID uuid.UUID, period, amount, invoiceNumber string) (Subscription, error) {
	cur, err := s.d.Store.Subscription(ctx, renewsID)
	if err != nil {
		return Subscription{}, err
	}
	now := s.now()
	if !slices.Contains(Periods(), period) {
		return Subscription{}, &ValidationError{Fields: map[string]string{"period": "must be QUARTERLY or YEARLY"}}
	}
	if _, ok := ParseAmount(amount); !ok {
		return Subscription{}, &ValidationError{Fields: map[string]string{"amount": "must be a positive amount"}}
	}
	if !InvoiceableAt(cur, now) {
		return Subscription{}, ErrState
	}
	number, err := s.d.Store.NextInvoiceNumber(ctx, now.Year())
	if err != nil {
		return Subscription{}, err
	}
	of := cur.ID
	x := Subscription{ID: uuid.New(), InvoiceNumber: number, PlanID: cur.PlanID, PlanName: cur.PlanName, Period: period, Status: StatusPendingPayment,
		SubscriberKind: cur.SubscriberKind, SubscriberID: cur.SubscriberID, SubscriberName: cur.SubscriberName, OrganisationID: cur.OrganisationID,
		RequestedBy: cur.RequestedBy, RequestedByName: cur.RequestedByName, MarketCode: cur.MarketCode, CurrencyCode: cur.CurrencyCode, Amount: amount,
		RenewalOf: &of, CreatedAt: now, UpdatedAt: now}
	ev := NewEvent(x.ID, ActionInvoiced, ActorAdmin, by.UserID, by.Name, "Renewal invoiced: "+strings.TrimSpace(invoiceNumber), now)
	err = s.d.Store.CreateSubscription(ctx, x, ev, func(existing []Subscription) error {
		for _, e := range existing {
			switch {
			case e.Status == StatusPendingPayment:
				return ErrPendingExists
			case e.Upcoming(now):
				return ErrActiveExists
			}
		}
		return nil
	})
	if err != nil {
		return Subscription{}, err
	}
	return x, nil
}

// InvoiceableAt reports whether x can be renewed by an invoice at now:
// ACTIVE (current or upcoming), or ended at most 90 days ago.
func InvoiceableAt(x Subscription, now time.Time) bool {
	switch x.EffectiveStatus(now) {
	case StatusActive:
		return true
	case StatusExpired:
		return x.EndsAt != nil && now.Sub(*x.EndsAt) <= RenewalWindowAfterEnd
	}
	return false
}

// ActivateInvoiced activates the PENDING_PAYMENT renewal of a settled
// invoice: the payment recorded is the invoice's (the receipts and the total
// paid). The same path as Approve: the period starts when the subscriber's
// paid period ends, the plan's package is granted and the subscriber emailed.
func (s *Service) ActivateInvoiced(ctx context.Context, by Actor, id uuid.UUID, pay Payment) (Detail, error) {
	rec := recorder{kind: RecordedByAdmin, actorType: ActorAdmin, userID: by.UserID, name: by.Name}
	return s.activate(ctx, id, rec, pay, nil)
}

// CancelInvoiced cancels the PENDING_PAYMENT renewal of a voided invoice,
// with its history and audit record. No email: the invoice is voided, and
// the subscriber was never told about this renewal apart from the invoice.
func (s *Service) CancelInvoiced(ctx context.Context, by Actor, id uuid.UUID, reason string) error {
	now := s.now()
	_, err := s.d.Store.ChangeSubscription(ctx, id, func(x *Subscription) (Change, error) {
		if x.Status != StatusPendingPayment {
			return Change{}, ErrState
		}
		reason = clip(reason, MaxReasonRunes)
		x.Status, x.CancelledAt, x.CancelledBy, x.CancelReason, x.UpdatedAt = StatusCancelled, &now, ActorAdmin, reason, now
		audit := adminaudit.New(by.UserID, by.Name, adminaudit.ActionSubscriptionCanceled, adminaudit.TargetSubscription, x.ID.String(), x.InvoiceNumber,
			reason, map[string]any{"status": StatusPendingPayment}, map[string]any{"status": StatusCancelled}, now)
		return Change{Events: []Event{NewEvent(x.ID, ActionCancelled, ActorAdmin, by.UserID, by.Name, reason, now)}, Audit: &audit}, nil
	})
	return err
}
