package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A made-up key. It is never sent anywhere but the local fake server.
const testAPIKey = "test-key-FAKE-not-real-0123456789"

type recordedRequest struct {
	Method, Path   string
	Authorization  string
	IdempotencyKey string
	ContentType    string
	Body           map[string]any
}

// fakeResend is a local stand-in for api.resend.com.
type fakeResend struct {
	t        *testing.T
	mu       sync.Mutex
	requests []recordedRequest
	// responses are served in order; the last one repeats.
	responses []fakeResponse
	server    *httptest.Server
}

type fakeResponse struct {
	status     int
	body       string
	retryAfter string
}

func newFakeResend(t *testing.T, responses ...fakeResponse) *fakeResend {
	f := &fakeResend{t: t, responses: responses}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"),
			IdempotencyKey: r.Header.Get("Idempotency-Key"), ContentType: r.Header.Get("Content-Type"), Body: body,
		})
		n := len(f.requests) - 1
		if n >= len(f.responses) {
			n = len(f.responses) - 1
		}
		resp := f.responses[n]
		f.mu.Unlock()
		if resp.retryAfter != "" {
			w.Header().Set("Retry-After", resp.retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = io.WriteString(w, resp.body)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeResend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeResend) sender(t *testing.T, mutate func(*ResendConfig)) (*ResendSender, *[]time.Duration) {
	t.Helper()
	var waits []time.Duration
	cfg := ResendConfig{
		APIKey: testAPIKey, From: "RightProperty365 <no-reply@rightproperty365.com>", BaseURL: f.server.URL,
		Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := NewResendSender(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, &waits
}

var (
	okResponse     = fakeResponse{status: 200, body: `{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`}
	rateLimited    = fakeResponse{status: 429, body: `{"statusCode":429,"name":"rate_limit_exceeded","message":"Too many requests"}`}
	serverError    = fakeResponse{status: 500, body: `{"statusCode":500,"name":"application_error","message":"boom"}`}
	testMessage    = Message{To: "tariro@example.com", Subject: "Request received: RP-7K2M9Q4D", HTML: "<p>Hello</p>", Text: "Hello\n", Kind: "property-request-received"}
	testKey        = "property-request-received/0195c0de-0000-7000-8000-00000000f001/customer"
	unverifiedFrom = fakeResponse{status: 403, body: `{"statusCode":403,"name":"validation_error","message":"The rightproperty365.com domain is not verified. Authorization: Bearer ` + testAPIKey + `"}`}
)

func assertNoKey(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, testAPIKey) || strings.Contains(s, "Bearer") || strings.Contains(strings.ToLower(s), "authorization") {
		t.Fatalf("%s exposes the API key or the Authorization header: %s", where, s)
	}
}

func TestResendSendSuccess(t *testing.T) {
	f := newFakeResend(t, okResponse)
	s, waits := f.sender(t, func(c *ResendConfig) { c.ReplyTo = "help@rightproperty365.com" })

	id, err := s.Send(context.Background(), testKey, testMessage)
	if err != nil || id != "49a3999c-0ce1-4ea6-ab68-afcd6dc2e794" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	if f.count() != 1 || len(*waits) != 0 {
		t.Fatalf("requests=%d waits=%v, want one request and no wait", f.count(), *waits)
	}
	req := f.requests[0]
	if req.Method != http.MethodPost || req.Path != "/emails" {
		t.Fatalf("request = %s %s, want POST /emails", req.Method, req.Path)
	}
	if req.Authorization != "Bearer "+testAPIKey {
		t.Fatal("the request must carry bearer authentication")
	}
	if req.IdempotencyKey != testKey {
		t.Fatalf("Idempotency-Key = %q, want the notification key", req.IdempotencyKey)
	}
	if req.ContentType != "application/json" {
		t.Fatalf("Content-Type = %q", req.ContentType)
	}
	want := map[string]any{
		"from": "RightProperty365 <no-reply@rightproperty365.com>", "subject": testMessage.Subject,
		"html": testMessage.HTML, "text": testMessage.Text, "reply_to": "help@rightproperty365.com",
	}
	for k, v := range want {
		if req.Body[k] != v {
			t.Fatalf("body[%s] = %v, want %v", k, req.Body[k], v)
		}
	}
	if to, ok := req.Body["to"].([]any); !ok || len(to) != 1 || to[0] != "tariro@example.com" {
		t.Fatalf("body[to] = %v", req.Body["to"])
	}
	if tags, ok := req.Body["tags"].([]any); !ok || len(tags) != 1 || tags[0].(map[string]any)["value"] != "property-request-received" {
		t.Fatalf("body[tags] = %v", req.Body["tags"])
	}
	// No tracking switches exist per message, and the development link is never sent.
	for _, k := range []string{"DevLink", "dev_link", "open_tracking", "click_tracking"} {
		if _, present := req.Body[k]; present {
			t.Fatalf("unexpected body field %q", k)
		}
	}
	assertNoKey(t, "String()", fmt.Sprintf("%v %+v %s", s, s, s))
}

func TestResendRetriesTransientFailures(t *testing.T) {
	t.Run("429 then success", func(t *testing.T) {
		f := newFakeResend(t, rateLimited, okResponse)
		s, waits := f.sender(t, nil)
		id, err := s.Send(context.Background(), testKey, testMessage)
		if err != nil || id == "" {
			t.Fatalf("Send = %q, %v", id, err)
		}
		if f.count() != 2 || len(*waits) != 1 {
			t.Fatalf("requests=%d waits=%v, want two requests with one wait", f.count(), *waits)
		}
		// The retry reuses the idempotency key: it cannot create a second email.
		if f.requests[0].IdempotencyKey != testKey || f.requests[1].IdempotencyKey != testKey {
			t.Fatalf("idempotency keys = %q, %q", f.requests[0].IdempotencyKey, f.requests[1].IdempotencyKey)
		}
	})

	t.Run("Retry-After is honoured, up to a cap", func(t *testing.T) {
		limited := rateLimited
		limited.retryAfter = "2"
		f := newFakeResend(t, limited, okResponse)
		s, waits := f.sender(t, nil)
		if _, err := s.Send(context.Background(), testKey, testMessage); err != nil {
			t.Fatal(err)
		}
		if len(*waits) != 1 || (*waits)[0] != 2*time.Second {
			t.Fatalf("waits = %v, want [2s]", *waits)
		}

		limited.retryAfter = "3600"
		f = newFakeResend(t, limited, okResponse)
		s, waits = f.sender(t, nil)
		if _, err := s.Send(context.Background(), testKey, testMessage); err != nil {
			t.Fatal(err)
		}
		if (*waits)[0] != resendMaxRetryAfter {
			t.Fatalf("wait = %v, want it capped at %v", (*waits)[0], resendMaxRetryAfter)
		}
	})

	t.Run("5xx until attempts are exhausted", func(t *testing.T) {
		f := newFakeResend(t, serverError)
		s, waits := f.sender(t, nil)
		_, err := s.Send(context.Background(), testKey, testMessage)
		var se *SendError
		if !errors.As(err, &se) || !se.Transient || se.Status != 500 || se.Name != "application_error" {
			t.Fatalf("err = %v, want a transient 500 application_error", err)
		}
		if f.count() != resendDefaultAttempts || len(*waits) != resendDefaultAttempts-1 {
			t.Fatalf("requests=%d waits=%d, want %d attempts", f.count(), len(*waits), resendDefaultAttempts)
		}
		if (*waits)[1] <= (*waits)[0] {
			t.Fatalf("backoff must grow: %v", *waits)
		}
		if !IsTransient(err) {
			t.Fatal("the caller must be told to retry later")
		}
		assertNoKey(t, "error", err.Error())
	})

	t.Run("a request with the same key still in progress", func(t *testing.T) {
		f := newFakeResend(t, fakeResponse{status: 409, body: `{"name":"concurrent_idempotent_requests"}`}, okResponse)
		s, _ := f.sender(t, nil)
		if _, err := s.Send(context.Background(), testKey, testMessage); err != nil || f.count() != 2 {
			t.Fatalf("err=%v requests=%d", err, f.count())
		}
	})

	t.Run("an exhausted quota is transient but not hammered", func(t *testing.T) {
		f := newFakeResend(t, fakeResponse{status: 429, body: `{"name":"daily_quota_exceeded"}`})
		s, _ := f.sender(t, nil)
		_, err := s.Send(context.Background(), testKey, testMessage)
		if !IsTransient(err) || f.count() != 1 {
			t.Fatalf("err=%v requests=%d, want one request and a transient error", err, f.count())
		}
	})

	t.Run("the server is unreachable", func(t *testing.T) {
		f := newFakeResend(t, okResponse)
		s, _ := f.sender(t, nil)
		f.server.Close()
		_, err := s.Send(context.Background(), testKey, testMessage)
		var se *SendError
		if !errors.As(err, &se) || !se.Transient || se.Status != 0 {
			t.Fatalf("err = %v, want a transient network failure", err)
		}
		// The transport error text (which names the URL) is not passed on.
		if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "http://") {
			t.Fatalf("the error leaks transport details: %v", err)
		}
		assertNoKey(t, "error", err.Error())
	})
}

func TestResendTerminalFailures(t *testing.T) {
	tests := []struct {
		name     string
		resp     fakeResponse
		wantName string
	}{
		{"422 missing field", fakeResponse{status: 422, body: `{"statusCode":422,"name":"missing_required_field","message":"Missing ` + "`to`" + ` field"}`}, "missing_required_field"},
		{"403 unverified domain", unverifiedFrom, "validation_error"},
		{"401 bad key", fakeResponse{status: 401, body: `{"name":"validation_error","message":"API key is invalid"}`}, "validation_error"},
		{"409 key reused with another body", fakeResponse{status: 409, body: `{"name":"invalid_idempotent_request"}`}, "invalid_idempotent_request"},
		{"400 unreadable body", fakeResponse{status: 400, body: `<html>not json</html>`}, "unknown_error"},
		{"400 hostile error name", fakeResponse{status: 400, body: `{"name":"Bearer ` + testAPIKey + `"}`}, "unknown_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeResend(t, tt.resp)
			s, waits := f.sender(t, nil)
			_, err := s.Send(context.Background(), testKey, testMessage)
			var se *SendError
			if !errors.As(err, &se) || se.Transient || se.Name != tt.wantName || se.Status != tt.resp.status {
				t.Fatalf("err = %v, want a terminal %d %s", err, tt.resp.status, tt.wantName)
			}
			if f.count() != 1 || len(*waits) != 0 {
				t.Fatalf("a terminal failure must not be retried: requests=%d", f.count())
			}
			if IsTransient(err) {
				t.Fatal("IsTransient must be false")
			}
			// The provider's message is kept only after sanitising.
			assertNoKey(t, "error", err.Error())
			assertNoKey(t, "detail", se.Detail)
		})
	}
}

// The provider's explanation is what tells an operator why a send was
// refused, so it is kept, but never with a credential or an address in it.
func TestResendErrorDetailIsSanitised(t *testing.T) {
	long := strings.Repeat("x", 400)
	tests := []struct {
		name string
		body string
		want string
	}{
		{"unverified domain", `{"name":"validation_error","message":"The rightproperty365.com domain is not verified. Please, add and verify your domain on https://resend.com/domains"}`,
			"The rightproperty365.com domain is not verified. Please, add and verify your domain on https://resend.com/domains"},
		{"bad key", `{"name":"validation_error","message":"API key is invalid"}`, "API key is invalid"},
		{"echoed credentials", unverifiedFrom.body, "The rightproperty365.com domain is not verified. [redacted]"},
		{"echoed key alone", `{"name":"x","message":"key ` + testAPIKey + ` rejected"}`, "key [redacted] rejected"},
		// Built at run time so the source holds nothing shaped like a real key.
		{"another key-shaped value", `{"name":"x","message":"got ` + "re" + "_" + "AbCd1234" + "_" + strings.Repeat("z", 24) + `"}`, "got [redacted]"},
		{"recipient address", `{"name":"validation_error","message":"You can only send testing emails to your own email address (owner@example.com)."}`,
			"You can only send testing emails to your own email address ([email])."},
		{"control characters and newlines", "{\"name\":\"x\",\"message\":\"line one\\nline\\ttwo \\u0007 caf\\u00e9\"}", "line one line two caf"},
		{"too long", `{"name":"x","message":"` + long + `"}`, strings.Repeat("x", 200) + "..."},
		{"no message", `{"name":"x"}`, ""},
		{"not json", `<html>nope</html>`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeResend(t, fakeResponse{status: 403, body: tt.body})
			s, _ := f.sender(t, nil)
			_, err := s.Send(context.Background(), testKey, testMessage)
			var se *SendError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want a SendError", err)
			}
			if se.Detail != tt.want {
				t.Fatalf("detail = %q, want %q", se.Detail, tt.want)
			}
			assertNoKey(t, "detail", se.Detail)
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("the error text should carry the detail: %v", err)
			}
			if strings.Contains(se.Detail, "example.com") {
				t.Fatalf("an email address survived: %q", se.Detail)
			}
		})
	}
}

func TestResendSenderValidation(t *testing.T) {
	if _, err := NewResendSender(ResendConfig{From: "a@example.com"}); err == nil {
		t.Fatal("an empty API key must be refused")
	}
	if _, err := NewResendSender(ResendConfig{APIKey: testAPIKey}); err == nil {
		t.Fatal("an empty sender must be refused")
	}
	f := newFakeResend(t, okResponse)
	s, _ := f.sender(t, nil)
	for _, key := range []string{"", strings.Repeat("k", 257)} {
		if _, err := s.Send(context.Background(), key, testMessage); err == nil || IsTransient(err) {
			t.Fatalf("idempotency key of length %d must be refused as terminal, got %v", len(key), err)
		}
	}
	if f.count() != 0 {
		t.Fatal("no request may be made without a valid idempotency key")
	}
	if got := s.cfg.BaseURL; got != f.server.URL {
		t.Fatalf("base URL = %q", got)
	}
	prod, err := NewResendSender(ResendConfig{APIKey: testAPIKey, From: "a@example.com"})
	if err != nil || prod.cfg.BaseURL != "https://api.resend.com" || prod.http.Timeout <= 0 {
		t.Fatalf("defaults: %v", err)
	}
}

// Whatever happens, the key and the Authorization header stay out of logs,
// errors and stored records — including at debug level.
func TestAPIKeyNeverAppearsInLogsErrorsOrRecords(t *testing.T) {
	// A recipient that is not on a reserved test domain: those are skipped
	// before the transport is reached (see IsReservedTestAddress).
	const to = "tariro@inbox.rightproperty365.com"
	msg := testMessage
	msg.To = to
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	for name, responses := range map[string][]fakeResponse{
		"accepted":      {okResponse},
		"rate limited":  {rateLimited, rateLimited, rateLimited},
		"server error":  {serverError},
		"terminal":      {unverifiedFrom},
		"hostile reply": {{status: 400, body: `{"name":"x","message":"Authorization: Bearer ` + testAPIKey + `"}`}},
	} {
		f := newFakeResend(t, responses...)
		s, _ := f.sender(t, nil)
		log := NewMemoryDeliveryLog()
		svc := NewService(s, log, logger)
		err := svc.Deliver(context.Background(), Notification{Key: "k/" + name, Message: msg})
		if err != nil {
			assertNoKey(t, name+" error", err.Error())
			assertNoKey(t, name+" error %+v", fmt.Sprintf("%+v %#v", err, err))
		}
		for _, rec := range log.Records() {
			assertNoKey(t, name+" record", fmt.Sprintf("%+v", rec))
			if strings.Contains(fmt.Sprintf("%+v", rec), to) {
				t.Fatalf("%s: the delivery record stores the recipient address", name)
			}
		}
	}
	assertNoKey(t, "logs", logs.String())
	if strings.Contains(logs.String(), to) || strings.Contains(logs.String(), "<p>Hello</p>") {
		t.Fatalf("logs contain the recipient address or the message body: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "email accepted by the provider") || strings.Contains(strings.ToLower(logs.String()), "delivered") {
		t.Fatalf("logs must say \"accepted by the provider\" and never \"delivered\": %s", logs.String())
	}
}
