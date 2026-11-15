package httpx

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestID(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusTeapot, "X", "x", nil)
	}))

	tests := []struct {
		name, supplied string
		wantEcho       bool
	}{
		{"generated when absent", "", false},
		{"valid id echoed", "abc-123_DEF.9", true},
		{"unsafe id replaced", "bad id\"<script>", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.supplied != "" {
				req.Header.Set(HeaderRequestID, tt.supplied)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get(HeaderRequestID)
			if got == "" {
				t.Fatal("no X-Request-Id on response")
			}
			if tt.wantEcho != (got == tt.supplied) {
				t.Fatalf("X-Request-Id = %q, supplied %q, wantEcho %v", got, tt.supplied, tt.wantEcho)
			}
			var env ErrorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatal(err)
			}
			if env.TraceID != got {
				t.Fatalf("trace_id = %q, want %q", env.TraceID, got)
			}
		})
	}
}

func TestCORS(t *testing.T) {
	called := false
	h := CORS([]string{"http://localhost:3000"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name        string
		method      string
		origin      string
		preflight   bool
		wantStatus  int
		wantAllow   string
		wantHandler bool
	}{
		{"allowed preflight", http.MethodOptions, "http://localhost:3000", true, http.StatusNoContent, "http://localhost:3000", false},
		{"disallowed preflight", http.MethodOptions, "https://evil.example", true, http.StatusForbidden, "", false},
		{"allowed simple request", http.MethodGet, "http://localhost:3000", false, http.StatusOK, "http://localhost:3000", true},
		{"disallowed simple request gets no CORS headers", http.MethodGet, "https://evil.example", false, http.StatusOK, "", true},
		{"no origin", http.MethodGet, "", false, http.StatusOK, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest(tt.method, "/v1/property-requests", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.preflight {
				req.Header.Set("Access-Control-Request-Method", "POST")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tt.wantAllow {
				t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, tt.wantAllow)
			}
			wantCreds := ""
			if tt.wantAllow != "" {
				wantCreds = "true"
			}
			if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != wantCreds {
				t.Fatalf("Access-Control-Allow-Credentials = %q, want %q", got, wantCreds)
			}
			if called != tt.wantHandler {
				t.Fatalf("handler called = %v, want %v", called, tt.wantHandler)
			}
			if tt.preflight && tt.wantStatus == http.StatusNoContent {
				if got := rec.Header().Get("Access-Control-Allow-Headers"); got != corsAllowHeaders {
					t.Fatalf("Access-Control-Allow-Headers = %q", got)
				}
			}
		})
	}
}

func TestRecover(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := RequestID(Recover(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != CodeInternal || env.TraceID == "" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := NewRateLimiter(60, 2) // 1 token/second, burst 2
	l.now = func() time.Time { return now }

	for i := range 2 {
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	ok, wait := l.Allow("1.2.3.4")
	if ok || wait <= 0 {
		t.Fatalf("third request should be limited (ok=%v wait=%v)", ok, wait)
	}
	if ok, _ := l.Allow("5.6.7.8"); !ok {
		t.Fatal("a different key must have its own bucket")
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("bucket should have refilled after one second")
	}
}

func TestLoggedPathHidesInvitationTokens(t *testing.T) {
	for in, want := range map[string]string{
		"/v1/invitations/abc":                   "/v1/invitations/{token}",
		"/v1/invitations/abc/accept":            "/v1/invitations/{token}/accept",
		"/v1/invitations/":                      "/v1/invitations/",
		"/v1/agent/organisations/x/invitations": "/v1/agent/organisations/x/invitations",
		"/v1/listings":                          "/v1/listings",
	} {
		if got := LoggedPath(in); got != want {
			t.Fatalf("LoggedPath(%q) = %q, want %q", in, got, want)
		}
	}
}
