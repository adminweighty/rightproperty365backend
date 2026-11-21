package metrics

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	var b bytes.Buffer
	if err := r.Write(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestExpositionFormat(t *testing.T) {
	p := NewProcess(quiet())
	clock := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return clock }
	h := p.Middleware(func(r *http.Request) string {
		if r.URL.Path == "/v1/listings/abc" {
			return "/v1/listings/{id}"
		}
		return ""
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clock = clock.Add(30 * time.Millisecond)
		if r.URL.Path == "/boom" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	for _, path := range []string{"/v1/listings/abc", "/v1/listings/abc", "/boom"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	p.SweepDone("email_delivery", clock.Add(-2*time.Second), nil)
	p.SweepDone("upload_intents", clock, errors.New("db down"))
	p.Gauge("rp365_outbox_pending", "Outbox events waiting.", func(context.Context) ([]Sample, error) {
		return []Sample{{Labels: map[string]string{"type": `with "quote"`}, Value: 3}}, nil
	})
	p.Gauge("rp365_broken", "A gauge that fails.", func(context.Context) ([]Sample, error) { return nil, errors.New("no") })

	out := scrape(t, p.Registry)
	for _, want := range []string{
		"# TYPE rp365_http_requests_total counter",
		`rp365_http_requests_total{method="GET",route="/v1/listings/{id}",status="2xx"} 2`,
		`rp365_http_requests_total{method="GET",route="unmatched",status="5xx"} 1`,
		"# TYPE rp365_http_request_duration_seconds histogram",
		`rp365_http_request_duration_seconds_bucket{method="GET",route="/v1/listings/{id}",le="0.025"} 0`,
		`rp365_http_request_duration_seconds_bucket{method="GET",route="/v1/listings/{id}",le="0.05"} 2`,
		`rp365_http_request_duration_seconds_bucket{method="GET",route="/v1/listings/{id}",le="+Inf"} 2`,
		`rp365_http_request_duration_seconds_count{method="GET",route="/v1/listings/{id}"} 2`,
		`rp365_sweep_runs_total{sweep="upload_intents",result="error"} 1`,
		`rp365_sweep_last_success_timestamp_seconds{sweep="email_delivery"} 1.7952516e+09`,
		`rp365_outbox_pending{type="with \"quote\""} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "rp365_broken") || strings.Contains(out, `sweep="upload_intents"} 1.7`) {
		t.Fatalf("a failed gauge or sweep shows:\n%s", out)
	}
	if strings.Contains(out, "/boom") {
		t.Fatal("a raw path became a label")
	}
}

func TestHandlerAndNil(t *testing.T) {
	p := NewProcess(quiet())
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	var none *Process
	none.SweepDone("x", time.Now(), nil) // no panic
	p.Add("undeclared", 1)
	p.Add(HTTPRequests, 1, "wrong number of labels")
}
