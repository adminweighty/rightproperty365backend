package mail

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// scriptedSender returns the given results in order and records the keys it saw.
type scriptedSender struct {
	results []error
	keys    []string
}

func (s *scriptedSender) Name() string { return "fake" }

func (s *scriptedSender) Send(_ context.Context, key string, _ Message) (string, error) {
	s.keys = append(s.keys, key)
	n := len(s.keys) - 1
	if n < len(s.results) && s.results[n] != nil {
		return "", s.results[n]
	}
	return "provider-id-" + key, nil
}

func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func TestDeliverOncePerNotification(t *testing.T) {
	sender := &scriptedSender{}
	log := NewMemoryDeliveryLog()
	svc := NewService(sender, log, quietLogger())
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now })
	n := Notification{Key: "welcome/evt-1/account", Message: Message{To: "Tariro@Example.com", Kind: "welcome"}}

	for i := range 3 {
		if err := svc.Deliver(context.Background(), n); err != nil {
			t.Fatalf("deliver %d: %v", i+1, err)
		}
	}
	if len(sender.keys) != 1 {
		t.Fatalf("the provider was called %d times, want once", len(sender.keys))
	}
	recs := log.Records()
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Status != StatusProviderAccepted || r.ProviderMessageID != "provider-id-welcome/evt-1/account" || r.Provider != "fake" ||
		r.Attempts != 1 || r.Kind != "welcome" || r.Terminal || r.LastError != "" {
		t.Fatalf("unexpected record: %+v", r)
	}
	if r.RecipientSHA256 != RecipientHash("tariro@example.com") || len(r.RecipientSHA256) != 64 {
		t.Fatalf("recipient hash = %q", r.RecipientSHA256)
	}

	// A different notification is a different email.
	other := n
	other.Key = "welcome/evt-2/account"
	if err := svc.Deliver(context.Background(), other); err != nil || len(sender.keys) != 2 {
		t.Fatalf("err=%v calls=%d", err, len(sender.keys))
	}
}

func TestDeliverTransientThenAccepted(t *testing.T) {
	sender := &scriptedSender{results: []error{&SendError{Status: 503, Name: "service_unavailable", Transient: true}}}
	log := NewMemoryDeliveryLog()
	svc := NewService(sender, log, quietLogger())
	n := Notification{Key: "password-changed/evt-1/account", Message: Message{To: "a@example.com", Kind: "password-changed"}}

	err := svc.Deliver(context.Background(), n)
	if err == nil || errors.Is(err, ErrTerminal) {
		t.Fatalf("err = %v, want a retryable failure", err)
	}
	if r := log.Records()[0]; r.Status != StatusFailed || r.Terminal || !strings.Contains(r.LastError, "service_unavailable") {
		t.Fatalf("after the failure: %+v", r)
	}

	if err := svc.Deliver(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	r := log.Records()[0]
	if r.Status != StatusProviderAccepted || r.Attempts != 2 || r.LastError != "" {
		t.Fatalf("after the retry: %+v", r)
	}
	// Both attempts used the same idempotency key.
	if len(sender.keys) != 2 || sender.keys[0] != n.Key || sender.keys[1] != n.Key {
		t.Fatalf("keys = %v", sender.keys)
	}
}

func TestDeliverTerminalIsNotRetried(t *testing.T) {
	sender := &scriptedSender{results: []error{&SendError{Status: 422, Name: "missing_required_field"}}}
	log := NewMemoryDeliveryLog()
	svc := NewService(sender, log, quietLogger())
	n := Notification{Key: "welcome/evt-9/account", Message: Message{To: "a@example.com", Kind: "welcome"}}

	for i := range 2 {
		err := svc.Deliver(context.Background(), n)
		if !errors.Is(err, ErrTerminal) {
			t.Fatalf("deliver %d: err = %v, want ErrTerminal", i+1, err)
		}
	}
	if len(sender.keys) != 1 {
		t.Fatalf("a terminal failure was sent again: %d calls", len(sender.keys))
	}
	if r := log.Records()[0]; r.Status != StatusFailed || !r.Terminal || r.Attempts != 1 {
		t.Fatalf("record: %+v", r)
	}
}

func TestDeliverRejectsBadKeys(t *testing.T) {
	svc := NewService(&scriptedSender{}, NewMemoryDeliveryLog(), quietLogger())
	for _, key := range []string{"", strings.Repeat("k", MaxKeyLength+1)} {
		if err := svc.Deliver(context.Background(), Notification{Key: key, Message: Message{To: "a@example.com"}}); !errors.Is(err, ErrTerminal) {
			t.Fatalf("key of length %d: err = %v", len(key), err)
		}
	}
}

func TestLogSender(t *testing.T) {
	var out bytes.Buffer
	s := NewLogSender(slog.New(slog.NewJSONHandler(&out, nil)))
	if s.Name() != "log" {
		t.Fatal("name")
	}
	const link = "http://localhost:3000/verify-email?token=SAMPLE"
	id, err := s.Send(context.Background(), "verify-email/x", Message{To: "a@example.com", Subject: "Confirm your email address", Kind: "verify-email", DevLink: link, HTML: "<p>secret body</p>"})
	if err != nil || !strings.HasPrefix(id, "log-") {
		t.Fatalf("Send = %q, %v", id, err)
	}
	line := out.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, `"msg":"DEV MAIL (not sent): verify-email link"`) || !strings.Contains(line, link) {
		t.Fatalf("the development mailer must log exactly one line with the link: %s", line)
	}
	if strings.Contains(line, "secret body") {
		t.Fatal("the body is not logged")
	}
	out.Reset()
	_, _ = s.Send(context.Background(), "k", Message{To: "a@example.com", Kind: "reset-password", DevLink: "http://localhost:3000/reset-password?token=S"})
	_, _ = s.Send(context.Background(), "k2", Message{To: "a@example.com", Kind: "welcome"})
	if !strings.Contains(out.String(), "DEV MAIL (not sent): reset-password link") || !strings.Contains(out.String(), "DEV MAIL (not sent): welcome") {
		t.Fatalf("unexpected log lines: %s", out.String())
	}
}

// namedSender is a scriptedSender that calls itself by another name.
type namedSender struct {
	scriptedSender
	name string
}

func (s *namedSender) Name() string { return s.name }

// The real transport never emails a reserved test domain (the sample
// accounts are @example.test); the development log transport still prints.
func TestReservedTestDomainsAreNeverSent(t *testing.T) {
	for _, tt := range []struct {
		address  string
		reserved bool
	}{
		{"a@example.com", true}, {"a@EXAMPLE.org", true}, {"a@example.net", true}, {"a@mail.example.co.uk", true},
		{"admin@example.test", true}, {"x@foo.test", true}, {"x@foo.example", true}, {"x@foo.invalid", true},
		{"x@foo.localhost", true}, {"x@localhost", true}, {"no-domain", true}, {"Name <a@example.test>", true},
		{"a@rightproperty365.com", false}, {"a@gmail.com", false}, {"a@examples.com", false}, {"a@myexample.com", false},
		{"a@test.co.zw", false}, {"a@example", true},
	} {
		if got := IsReservedTestAddress(tt.address); got != tt.reserved {
			t.Fatalf("IsReservedTestAddress(%q) = %v, want %v", tt.address, got, tt.reserved)
		}
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	resend := &namedSender{name: "resend"}
	deliveries := NewMemoryDeliveryLog()
	svc := NewService(resend, deliveries, logger)
	if err := svc.Deliver(context.Background(), Notification{Key: "sign-in-code/1", Message: Message{To: "consumer@example.test", Kind: "sign-in-code"}}); err != nil {
		t.Fatal(err)
	}
	if len(resend.keys) != 0 || len(deliveries.Records()) != 0 {
		t.Fatalf("a reserved address must not be sent or recorded: sent %v", resend.keys)
	}
	if !strings.Contains(logs.String(), "reserved test domain") || strings.Contains(logs.String(), "consumer@example.test") {
		t.Fatalf("the skip is logged without the address: %s", logs.String())
	}
	if err := svc.Deliver(context.Background(), Notification{Key: "sign-in-code/2", Message: Message{To: "someone@rightproperty365.com", Kind: "sign-in-code"}}); err != nil || len(resend.keys) != 1 {
		t.Fatalf("a real address is sent: %v %v", err, resend.keys)
	}

	logSvc := NewService(NewLogSender(quietLogger()), NewMemoryDeliveryLog(), quietLogger())
	if err := logSvc.Deliver(context.Background(), Notification{Key: "sign-in-code/3", Message: Message{To: "consumer@example.test", Kind: "sign-in-code", DevCode: "000000"}}); err != nil {
		t.Fatal(err)
	}
}
