// Package paygateway is the provider-neutral adapter behind online payment
// of subscription invoices (API 0.42.0, BR-015, FR-037-040).
//
// No payment provider has been chosen. PAYMENT_GATEWAY selects a driver:
//
//   - none (the default): online payment is not offered; Open returns a nil
//     Gateway and every caller treats nil as "off".
//   - fake: development and staging only (refused in production). The hosted
//     checkout page is served by the API itself and its buttons deliver a
//     webhook signed with a development secret (see Fake).
//
// A real provider (Paynow, Stripe, Flutterwave, DPO ...) is one more file in
// this package implementing Gateway, a name in config.PaymentGateways, its
// credentials as config.Secret values, and a case in Open. What it must do is
// written on each method below and in the README ("Online payment").
//
// Money is a decimal string with two decimals and an ISO 4217 code, never a
// float; times are UTC.
package paygateway

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Event types a driver maps the provider's notifications to.
const (
	EventPaymentSucceeded = "PAYMENT_SUCCEEDED"
	EventPaymentFailed    = "PAYMENT_FAILED"
	EventRefunded         = "REFUNDED"
	EventCheckoutExpired  = "CHECKOUT_EXPIRED"
)

// Payment statuses FetchPayment reports.
const (
	StatusPending   = "PENDING"
	StatusSucceeded = "SUCCEEDED"
	StatusFailed    = "FAILED"
	StatusExpired   = "EXPIRED"
	StatusUnknown   = "UNKNOWN"
)

// Refund statuses.
const (
	RefundPending   = "PENDING"
	RefundSucceeded = "SUCCEEDED"
)

// Errors.
var (
	// ErrInvalidSignature is a webhook whose signature (or timestamp) does
	// not verify. Nothing in it may be trusted.
	ErrInvalidSignature = errors.New("paygateway: invalid webhook signature")
	// ErrMalformed is a signed webhook the driver cannot read.
	ErrMalformed = errors.New("paygateway: malformed webhook")
	// ErrProvider is the provider refusing or being unreachable.
	ErrProvider = errors.New("paygateway: provider error")
	// ErrNotFound is an unknown checkout or payment reference.
	ErrNotFound = errors.New("paygateway: not found")
)

// CheckoutRequest is one invoice to be paid on the provider's hosted page.
type CheckoutRequest struct {
	// PaymentID is the platform's payment row (also in the idempotency key).
	PaymentID     string
	InvoiceNumber string
	Description   string
	// Amount ("30.00") and Currency ("USD") of the invoice.
	Amount   string
	Currency string
	// SuccessURL and CancelURL are where the provider sends the browser back.
	SuccessURL string
	CancelURL  string
	// IdempotencyKey must be passed to the provider (or deduplicated on by
	// the driver): the same key never creates two checkouts.
	IdempotencyKey string
	// BaseURL is the API's own origin as the caller reached it; only the fake
	// driver uses it (its hosted page is served by the API).
	BaseURL string
}

// Checkout is the hosted checkout the browser is sent to.
type Checkout struct {
	// ProviderRef identifies the checkout at the provider.
	ProviderRef string
	HostedURL   string
	ExpiresAt   time.Time
}

// Event is a verified webhook, in provider-neutral terms.
type Event struct {
	// ID is the provider's unique event id (the deduplication key).
	ID   string
	Type string
	// ProviderRef is the checkout; PaymentRef the provider's payment
	// (transaction) reference; RefundRef a refund's reference (REFUNDED).
	ProviderRef string
	PaymentRef  string
	RefundRef   string
	// Amount and Currency: what was paid (PAYMENT_SUCCEEDED) or refunded
	// (REFUNDED, this refund only).
	Amount     string
	Currency   string
	OccurredAt time.Time
	// Reason is a failure reason when the provider gives one.
	Reason string
}

// RefundRequest refunds all or part of a payment.
type RefundRequest struct {
	PaymentRef     string
	Amount         string
	Currency       string
	IdempotencyKey string
	Reason         string
}

// RefundResult is the provider's answer to a refund.
type RefundResult struct {
	RefundRef string
	// Status is RefundSucceeded, or RefundPending when a webhook will follow.
	Status string
}

// PaymentStatus is a checkout's state at the provider (reconciliation).
type PaymentStatus struct {
	Status     string
	PaymentRef string
	Amount     string
	Currency   string
	OccurredAt time.Time
}

// Gateway is one payment provider.
type Gateway interface {
	// Name is the PAYMENT_GATEWAY value and the {provider} of the webhook URL.
	Name() string
	// CreateCheckout creates a hosted checkout for an invoice.
	CreateCheckout(ctx context.Context, req CheckoutRequest) (Checkout, error)
	// VerifyWebhook checks the signature of a webhook BEFORE reading it and
	// returns the event (ErrInvalidSignature, ErrMalformed).
	VerifyWebhook(ctx context.Context, header http.Header, body []byte) (Event, error)
	// Refund returns money to the payer (idempotent on the key).
	Refund(ctx context.Context, req RefundRequest) (RefundResult, error)
	// FetchPayment returns a checkout's current state (ErrNotFound).
	FetchPayment(ctx context.Context, providerRef string) (PaymentStatus, error)
}

// Enabled reports whether online payment is offered.
func Enabled(g Gateway) bool { return g != nil }
