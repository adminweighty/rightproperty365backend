package paygateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/platform/config"
)

func TestFakeRefusedInProduction(t *testing.T) {
	if _, err := NewFake("production", []byte("0123456789abcdef"), nil); !errors.Is(err, ErrFakeInProduction) {
		t.Fatalf("NewFake in production: %v", err)
	}
	cfg := config.Config{Env: config.EnvProduction, PaymentGateway: config.PaymentGatewayFake}
	if g, err := Open(cfg, nil, nil); err == nil || g != nil {
		t.Fatalf("Open fake in production: %v %v", g, err)
	}
	if g, err := Open(config.Config{Env: config.EnvProduction, PaymentGateway: config.PaymentGatewayNone}, nil, nil); err != nil || g != nil || Enabled(g) {
		t.Fatalf("none: %v %v", g, err)
	}
}

func TestFakeSignatureAndCheckout(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	f, err := NewFake("staging", []byte("test-secret-0123456789"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := CheckoutRequest{InvoiceNumber: "RPS-2026-000001", Amount: "30.00", Currency: "USD", IdempotencyKey: "k1", BaseURL: "http://api.test"}
	co, err := f.CreateCheckout(ctx, req)
	if err != nil || !strings.HasPrefix(co.HostedURL, "http://api.test/v1/payments/fake/checkout/fake_cs_") || !co.ExpiresAt.Equal(now.Add(FakeCheckoutTTL)) {
		t.Fatalf("checkout: %+v %v", co, err)
	}
	again, _ := f.CreateCheckout(ctx, req)
	if again.ProviderRef != co.ProviderRef {
		t.Fatal("the same idempotency key must return the same checkout")
	}
	if st, err := f.FetchPayment(ctx, co.ProviderRef); err != nil || st.Status != StatusPending {
		t.Fatalf("fetch: %+v %v", st, err)
	}
	ev, err := f.Complete(co.ProviderRef, true)
	if err != nil || ev.Type != EventPaymentSucceeded || ev.Amount != "30.00" || ev.PaymentRef == "" {
		t.Fatalf("complete: %+v %v", ev, err)
	}
	body := EventBody(ev)
	h := http.Header{}
	h.Set(FakeSignatureHeader, f.Sign(body, now))
	got, err := f.VerifyWebhook(ctx, h, body)
	if err != nil || got.ID != ev.ID || got.ProviderRef != co.ProviderRef || got.Currency != "USD" {
		t.Fatalf("verify: %+v %v", got, err)
	}
	// Tampered body, wrong secret, stale timestamp, missing header.
	if _, err := f.VerifyWebhook(ctx, h, []byte(strings.Replace(string(body), "30.00", "3.00", 1))); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered: %v", err)
	}
	other, _ := NewFake("development", []byte("another-secret-0123"), func() time.Time { return now })
	h2 := http.Header{}
	h2.Set(FakeSignatureHeader, other.Sign(body, now))
	if _, err := f.VerifyWebhook(ctx, h2, body); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("wrong secret: %v", err)
	}
	h3 := http.Header{}
	h3.Set(FakeSignatureHeader, f.Sign(body, now.Add(-FakeSignatureMaxAge-time.Second)))
	if _, err := f.VerifyWebhook(ctx, h3, body); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("stale: %v", err)
	}
	if _, err := f.VerifyWebhook(ctx, http.Header{}, body); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("missing: %v", err)
	}
	if st, _ := f.FetchPayment(ctx, co.ProviderRef); st.Status != StatusSucceeded || st.PaymentRef != ev.PaymentRef {
		t.Fatalf("fetch after pay: %+v", st)
	}
	r1, err := f.Refund(ctx, RefundRequest{PaymentRef: ev.PaymentRef, Amount: "10.00", Currency: "USD", IdempotencyKey: "r1"})
	r2, _ := f.Refund(ctx, RefundRequest{PaymentRef: ev.PaymentRef, Amount: "10.00", Currency: "USD", IdempotencyKey: "r1"})
	if err != nil || r1.Status != RefundSucceeded || r1.RefundRef != r2.RefundRef {
		t.Fatalf("refund: %+v %+v %v", r1, r2, err)
	}
}
