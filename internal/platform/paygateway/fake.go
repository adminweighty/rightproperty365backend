package paygateway

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FakeName is the fake driver's name (PAYMENT_GATEWAY=fake).
const FakeName = "fake"

// FakeSignatureHeader carries the fake webhook's signature:
// "t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>".
const FakeSignatureHeader = "Fake-Signature"

// Fake checkout lifetime and webhook timestamp tolerance.
const (
	FakeCheckoutTTL     = 30 * time.Minute
	FakeSignatureMaxAge = 5 * time.Minute
)

// ErrFakeInProduction refuses the fake driver with APP_ENV=production.
var ErrFakeInProduction = errors.New("paygateway: the fake payment gateway is refused in production")

// Fake is the development and staging driver. Its hosted page is served by
// the API (modules/payments: GET /v1/payments/fake/checkout/{ref}); the page's
// buttons call Complete, which records the outcome here and returns the event
// the page signs with Sign and delivers to the webhook handler, exactly as a
// provider would. What it knows lives in this process only (an API restart
// forgets open checkouts: FetchPayment then answers UNKNOWN).
type Fake struct {
	secret []byte
	now    func() time.Time

	mu        sync.Mutex
	checkouts map[string]*fakeCheckout
	byKey     map[string]string
	refunds   map[string]RefundResult
}

type fakeCheckout struct {
	ref, invoice, amount, currency string
	expires                        time.Time
	status, paymentRef             string
	at                             time.Time
}

// NewFake returns the fake driver. env is APP_ENV: production is refused.
// An empty secret is replaced by a random one (this process only).
func NewFake(env string, secret []byte, now func() time.Time) (*Fake, error) {
	if strings.EqualFold(env, "production") {
		return nil, ErrFakeInProduction
	}
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
	}
	if now == nil {
		now = time.Now
	}
	return &Fake{secret: append([]byte{}, secret...), now: now, checkouts: map[string]*fakeCheckout{}, byKey: map[string]string{},
		refunds: map[string]RefundResult{}}, nil
}

// Name implements Gateway.
func (f *Fake) Name() string { return FakeName }

func randomRef(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// CreateCheckout implements Gateway.
func (f *Fake) CreateCheckout(_ context.Context, req CheckoutRequest) (Checkout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref, ok := f.byKey[req.IdempotencyKey]; ok && req.IdempotencyKey != "" {
		c := f.checkouts[ref]
		return Checkout{ProviderRef: c.ref, HostedURL: f.hostedURL(req.BaseURL, c.ref), ExpiresAt: c.expires}, nil
	}
	c := &fakeCheckout{ref: randomRef("fake_cs_"), invoice: req.InvoiceNumber, amount: req.Amount, currency: req.Currency,
		expires: f.now().UTC().Add(FakeCheckoutTTL).Truncate(time.Second), status: StatusPending}
	f.checkouts[c.ref] = c
	if req.IdempotencyKey != "" {
		f.byKey[req.IdempotencyKey] = c.ref
	}
	return Checkout{ProviderRef: c.ref, HostedURL: f.hostedURL(req.BaseURL, c.ref), ExpiresAt: c.expires}, nil
}

func (f *Fake) hostedURL(base, ref string) string {
	return strings.TrimRight(base, "/") + "/v1/payments/fake/checkout/" + ref
}

// fakeBody is the fake webhook's JSON body.
type fakeBody struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	CheckoutRef string    `json:"checkout_ref"`
	PaymentRef  string    `json:"payment_ref,omitempty"`
	RefundRef   string    `json:"refund_ref,omitempty"`
	Amount      string    `json:"amount,omitempty"`
	Currency    string    `json:"currency,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
}

// EventBody is the JSON body of a fake webhook carrying ev (tests and the
// hosted page).
func EventBody(ev Event) []byte {
	b, _ := json.Marshal(fakeBody{ID: ev.ID, Type: ev.Type, CheckoutRef: ev.ProviderRef, PaymentRef: ev.PaymentRef, RefundRef: ev.RefundRef,
		Amount: ev.Amount, Currency: ev.Currency, Reason: ev.Reason, OccurredAt: ev.OccurredAt.UTC()})
	return b
}

func (f *Fake) mac(t string, body []byte) string {
	m := hmac.New(sha256.New, f.secret)
	m.Write([]byte(t))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Sign returns the Fake-Signature header value for body, signed at at.
func (f *Fake) Sign(body []byte, at time.Time) string {
	t := strconv.FormatInt(at.Unix(), 10)
	return "t=" + t + ",v1=" + f.mac(t, body)
}

// VerifyWebhook implements Gateway: the signature first (constant time,
// timestamp within FakeSignatureMaxAge), then the body.
func (f *Fake) VerifyWebhook(_ context.Context, header http.Header, body []byte) (Event, error) {
	var t, sig string
	for _, part := range strings.Split(header.Get(FakeSignatureHeader), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			t = v
		case "v1":
			sig = v
		}
	}
	unix, err := strconv.ParseInt(t, 10, 64)
	if err != nil || sig == "" {
		return Event{}, ErrInvalidSignature
	}
	if age := f.now().Sub(time.Unix(unix, 0)); age > FakeSignatureMaxAge || age < -FakeSignatureMaxAge {
		return Event{}, ErrInvalidSignature
	}
	if !hmac.Equal([]byte(sig), []byte(f.mac(t, body))) {
		return Event{}, ErrInvalidSignature
	}
	var b fakeBody
	if err := json.Unmarshal(body, &b); err != nil || b.ID == "" || b.Type == "" {
		return Event{}, ErrMalformed
	}
	return Event{ID: b.ID, Type: b.Type, ProviderRef: b.CheckoutRef, PaymentRef: b.PaymentRef, RefundRef: b.RefundRef, Amount: b.Amount,
		Currency: b.Currency, Reason: b.Reason, OccurredAt: b.OccurredAt.UTC()}, nil
}

// Complete records the outcome of the hosted page at the fake provider and
// returns the webhook event to deliver: PAYMENT_SUCCEEDED for the invoice
// amount, or PAYMENT_FAILED. Completing twice returns the same outcome
// under a new event id (a provider's redelivery has the same id; this is a
// second click).
func (f *Fake) Complete(ref string, succeed bool) (Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.checkouts[ref]
	if !ok {
		return Event{}, ErrNotFound
	}
	now := f.now().UTC()
	if c.status == StatusPending {
		c.at = now
		if succeed {
			c.status, c.paymentRef = StatusSucceeded, randomRef("fake_pay_")
		} else {
			c.status = StatusFailed
		}
	}
	ev := Event{ID: randomRef("evt_fake_"), ProviderRef: c.ref, OccurredAt: c.at}
	switch c.status {
	case StatusSucceeded:
		ev.Type, ev.PaymentRef, ev.Amount, ev.Currency = EventPaymentSucceeded, c.paymentRef, c.amount, c.currency
	default:
		ev.Type, ev.Reason = EventPaymentFailed, "Declined on the fake checkout page."
	}
	return ev, nil
}

// Refund implements Gateway: always succeeds at once (idempotent on the key).
func (f *Fake) Refund(_ context.Context, req RefundRequest) (RefundResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.PaymentRef == "" {
		return RefundResult{}, fmt.Errorf("%w: no payment reference", ErrProvider)
	}
	if r, ok := f.refunds[req.IdempotencyKey]; ok {
		return r, nil
	}
	r := RefundResult{RefundRef: randomRef("fake_re_"), Status: RefundSucceeded}
	f.refunds[req.IdempotencyKey] = r
	return r, nil
}

// FetchPayment implements Gateway.
func (f *Fake) FetchPayment(_ context.Context, providerRef string) (PaymentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.checkouts[providerRef]
	if !ok {
		return PaymentStatus{Status: StatusUnknown}, ErrNotFound
	}
	st := PaymentStatus{Status: c.status, PaymentRef: c.paymentRef, OccurredAt: c.at}
	if c.status == StatusPending && !f.now().Before(c.expires) {
		st.Status = StatusExpired
	}
	if c.status == StatusSucceeded {
		st.Amount, st.Currency = c.amount, c.currency
	}
	return st, nil
}
