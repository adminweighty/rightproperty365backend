// Package payments is online payment of subscription invoices through the
// provider-neutral gateway adapter (API 0.42.0, BR-015, FR-037-040).
//
// It is OFF unless PAYMENT_GATEWAY names a driver (platform/paygateway): with
// none, no route here offers anything and the subscriptions module works as
// before (bank transfer, recorded by finance).
//
// The flow: the subscriber (or the organisation's owner) asks for a checkout
// of a PENDING_PAYMENT invoice and is sent to the provider's hosted page.
// The provider's signed webhook is the only thing that activates: it is
// verified, deduplicated on (provider, event id) in payment_events, its
// amount and currency compared with the invoice (a mismatch goes to REVIEW
// and activates nothing), and a success activates the subscription through
// subscriptions.Service.ActivatePaid, the same path as an administrator's
// approval. The browser's return to the web app only shows the status.
//
// The platform's finance record of online payments is this module's tables
// (payments, payment_refunds, payment_events) and the admin audit; the agent
// ledger (modules/ledger) is the agents' own books and is never written.
package payments

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// Payment statuses.
const (
	StatusOpen              = "OPEN"
	StatusSucceeded         = "SUCCEEDED"
	StatusFailed            = "FAILED"
	StatusExpired           = "EXPIRED"
	StatusReview            = "REVIEW"
	StatusPartiallyRefunded = "PARTIALLY_REFUNDED"
	StatusRefunded          = "REFUNDED"
)

// Statuses returns every payment status.
func Statuses() []string {
	return []string{StatusOpen, StatusSucceeded, StatusFailed, StatusExpired, StatusReview, StatusPartiallyRefunded, StatusRefunded}
}

// Refund statuses.
const (
	RefundPending   = "PENDING"
	RefundSucceeded = "SUCCEEDED"
	RefundFailed    = "FAILED"
)

// Admin audit actions (target SUBSCRIPTION, labelled with the invoice).
const (
	ActionCheckoutCreated = "PAYMENT_CHECKOUT_CREATED"
	ActionConfirmed       = "PAYMENT_CONFIRMED"
	ActionFailed          = "PAYMENT_FAILED"
	ActionFlagged         = "PAYMENT_FLAGGED"
	ActionRefundRequested = "PAYMENT_REFUND_REQUESTED"
	ActionRefunded        = "PAYMENT_REFUNDED"
)

// Outcomes of a webhook event (payment_events.outcome).
const (
	OutcomeApplied        = "APPLIED"
	OutcomeNoChange       = "NO_CHANGE"
	OutcomeUnknownPayment = "UNKNOWN_PAYMENT"
	OutcomeIgnored        = "IGNORED"
	OutcomeFlagged        = "FLAGGED"
)

// Limits.
const (
	// ReuseMargin: an open checkout that expires sooner than this is not
	// handed out again (a new one is made).
	ReuseMargin = time.Minute
	// CreationGrace: a checkout row without its hosted page yet is another
	// request creating it; after this it is abandoned.
	CreationGrace = time.Minute
	// EventLease is how long a webhook event being processed is not
	// processed again by a redelivery.
	EventLease       = 2 * time.Minute
	MaxReasonRunes   = 1000
	MaxKeyLen        = 100
	DefaultPageSize  = 20
	MaxPageSize      = 100
	FollowUpWindow   = 48 * time.Hour
	DefaultExportCap = 10000
	MaxExportCap     = 50000
)

// Error codes of this module's endpoints.
const (
	CodeNotOffered         = "ONLINE_PAYMENT_NOT_OFFERED"
	CodeProviderNotFound   = "PAYMENT_PROVIDER_NOT_FOUND"
	CodeInvalidSignature   = "INVALID_WEBHOOK_SIGNATURE"
	CodeProviderError      = "PAYMENT_PROVIDER_ERROR"
	CodeCheckoutInProgress = "CHECKOUT_IN_PROGRESS"
	CodeNoGatewayPayment   = "NO_GATEWAY_PAYMENT"
	CodeRefundExceeds      = "REFUND_EXCEEDS_PAYMENT"
	CodePaymentNotFound    = "PAYMENT_NOT_FOUND"
)

// Errors, mapped to HTTP by the handler.
var (
	ErrNotOffered         = errors.New("online payment is not offered")
	ErrProviderNotFound   = errors.New("unknown payment provider")
	ErrInvalidSignature   = errors.New("invalid webhook signature")
	ErrProvider           = errors.New("the payment provider failed")
	ErrCheckoutInProgress = errors.New("a checkout of this invoice is being created")
	ErrNoGatewayPayment   = errors.New("no paid online payment for this subscription")
	ErrRefundExceeds      = errors.New("the refund exceeds what remains of the payment")
	ErrPaymentNotFound    = errors.New("payment not found")
	ErrRefundNotFound     = errors.New("refund not found")
	// errNoChange tells a store's change function to write nothing.
	errNoChange = errors.New("no change")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// Payment is one checkout of a subscription invoice and what became of it.
// Amounts are decimal strings with two decimals ("" when unknown).
type Payment struct {
	ID             uuid.UUID
	SubscriptionID uuid.UUID
	InvoiceNumber  string
	SubscriberKind string
	SubscriberID   uuid.UUID
	SubscriberName string
	MarketCode     string
	Provider       string
	// ProviderRef is the checkout at the provider ("" while being created);
	// PaymentRef the provider's payment (transaction) reference.
	ProviderRef    string
	PaymentRef     string
	IdempotencyKey string
	// Amount and Currency are the invoice's.
	Amount   string
	Currency string
	// PaidAmount and PaidCurrency are what the provider reported as paid.
	PaidAmount     string
	PaidCurrency   string
	RefundedAmount string
	Status         string
	HostedURL      string
	ExpiresAt      *time.Time
	ReturnURL      string
	CancelURL      string
	ReviewReason   string
	FailureReason  string
	OpenedBy       *uuid.UUID
	OpenedByName   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	SucceededAt    *time.Time
	FailedAt       *time.Time
	ExpiredAt      *time.Time
	RefundedAt     *time.Time
}

// Reference is the payment's reference as recorded on the subscription: the
// provider's payment reference, else its checkout reference.
func (p Payment) Reference() string {
	if p.PaymentRef != "" {
		return p.PaymentRef
	}
	return p.ProviderRef
}

// Refundable reports whether money was received and may be returned.
func (p Payment) Refundable() bool {
	switch p.Status {
	case StatusSucceeded, StatusPartiallyRefunded, StatusReview:
		return p.PaidAmount != "" && p.PaymentRef != ""
	}
	return false
}

// Refund is money returned on a payment.
type Refund struct {
	ID                uuid.UUID
	PaymentID         uuid.UUID
	Amount            string
	Currency          string
	Reason            string
	Status            string
	ProviderRefundRef string
	IdempotencyKey    string
	RequestedBy       *uuid.UUID
	RequestedByName   string
	FailureReason     string
	CreatedAt         time.Time
	CompletedAt       *time.Time
}

// EventRecord is a verified webhook event as kept in payment_events.
type EventRecord struct {
	ID          uuid.UUID
	Provider    string
	EventID     string
	Type        string
	ProviderRef string
	PaymentRef  string
	RefundRef   string
	Amount      string
	Currency    string
	OccurredAt  time.Time
	ReceivedAt  time.Time
}

// Filter selects payments. Empty fields match everything.
type Filter struct {
	Statuses       []string
	Provider       string
	SubscriptionID *uuid.UUID
	// Q matches the invoice number, the subscriber's name or a reference.
	Q string
	// UpdatedSince keeps payments changed at or after it (zero: any).
	UpdatedSince time.Time
	// CreatedFrom and CreatedTo bound created_at ([from, to); zero: open).
	CreatedFrom, CreatedTo time.Time
	Limit, Offset          int
}

// Store persists payments, refunds and webhook events.
type Store interface {
	// ReserveCheckout decides, under the subscription's lock, between the
	// subscription's OPEN checkout (returned with reused true when it can be
	// handed out again; ErrCheckoutInProgress while another request is
	// creating it) and p, stored as OPEN after retiring the stale one.
	ReserveCheckout(ctx context.Context, p Payment, now time.Time) (Payment, bool, error)
	// Payment returns one, or ErrPaymentNotFound.
	Payment(ctx context.Context, id uuid.UUID) (Payment, error)
	// PaymentByRef returns the provider's payment whose checkout or payment
	// reference is ref, or ErrPaymentNotFound.
	PaymentByRef(ctx context.Context, provider, ref string) (Payment, error)
	// Payments returns one page, newest first, and the total.
	Payments(ctx context.Context, f Filter) ([]Payment, int, error)
	// ChangePayment locks the payment, calls change with a copy and saves it
	// with the audit records change returns (errNoChange: nothing written,
	// the payment returned as it is).
	ChangePayment(ctx context.Context, id uuid.UUID, change func(p *Payment) ([]adminaudit.Event, error)) (Payment, error)

	// ClaimEvent records a verified event and claims it for processing:
	// false when it was processed already or another delivery is processing
	// it (a lease of EventLease).
	ClaimEvent(ctx context.Context, e EventRecord, now time.Time) (bool, error)
	// FinishEvent marks a claimed event processed, with its outcome.
	FinishEvent(ctx context.Context, provider, eventID, outcome string, paymentID *uuid.UUID, at time.Time) error
	// ReleaseEvent gives a claimed event up (its processing failed): the
	// provider's next delivery processes it again.
	ReleaseEvent(ctx context.Context, provider, eventID string) error

	// CreateRefund locks the payment and returns the refund with r's
	// idempotency key when there is one (existing true); otherwise it calls
	// check with the payment and its refunds and stores r.
	CreateRefund(ctx context.Context, r Refund, check func(p Payment, refunds []Refund) error) (Refund, bool, error)
	// ChangeRefund locks the payment and the refund, calls change with
	// copies and saves both with the audit records (errNoChange: nothing).
	ChangeRefund(ctx context.Context, id uuid.UUID, change func(r *Refund, p *Payment) ([]adminaudit.Event, error)) (Refund, Payment, error)
	// RefundByProviderRef returns the payment's refund with that provider
	// reference, or ErrRefundNotFound.
	RefundByProviderRef(ctx context.Context, paymentID uuid.UUID, ref string) (Refund, error)
	// Refunds returns the refunds of the payments, oldest first.
	Refunds(ctx context.Context, paymentIDs []uuid.UUID) (map[uuid.UUID][]Refund, error)
}

// reserve decides about the subscription's open checkout: reuse it, refuse
// while it is being created, or retire it (returned changed) before a new one.
func reserve(open *Payment, now time.Time) (reuse bool, retired *Payment, err error) {
	if open == nil {
		return false, nil, nil
	}
	if open.HostedURL == "" {
		if now.Sub(open.CreatedAt) < CreationGrace {
			return false, nil, ErrCheckoutInProgress
		}
		x := *open
		x.Status, x.FailedAt, x.UpdatedAt, x.FailureReason = StatusFailed, &now, now, "The checkout was abandoned while being created."
		return false, &x, nil
	}
	if open.ExpiresAt != nil && now.Add(ReuseMargin).Before(*open.ExpiresAt) {
		return true, nil, nil
	}
	x := *open
	x.Status, x.ExpiredAt, x.UpdatedAt = StatusExpired, &now, now
	return false, &x, nil
}

func auditOf(actorID uuid.UUID, actorName, action string, p Payment, reason string, before, after map[string]any, at time.Time) adminaudit.Event {
	return adminaudit.New(actorID, actorName, action, adminaudit.TargetSubscription, p.SubscriptionID.String(), p.InvoiceNumber, reason, before, after, at)
}
