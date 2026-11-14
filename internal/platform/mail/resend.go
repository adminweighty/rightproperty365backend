package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ResendAPIBaseURL is the production endpoint of the Resend API.
const ResendAPIBaseURL = "https://api.resend.com"

const (
	resendDefaultAttempts  = 3
	resendRequestTimeout   = 15 * time.Second
	resendMaxRetryAfter    = 10 * time.Second
	resendMaxIdempotencyKB = 256
	resendMaxResponseBytes = 64 << 10
)

// ResendConfig configures a ResendSender.
type ResendConfig struct {
	// APIKey is the Resend API key. It is sent only in the Authorization
	// header and never logged, stored or put in an error.
	APIKey string
	// From is the sender, e.g. "RightProperty365 <no-reply@rightproperty365.com>".
	// Resend only accepts a domain verified in the account.
	From string
	// ReplyTo is optional.
	ReplyTo string

	// BaseURL overrides the API endpoint (tests). Empty means ResendAPIBaseURL.
	BaseURL string
	// HTTPClient overrides the client (tests). Nil means a client with a timeout.
	HTTPClient *http.Client
	// MaxAttempts bounds the tries within one Send call (default 3).
	MaxAttempts int
	// Backoff returns the wait before retry n (1-based). Nil means 300ms, 900ms, ...
	Backoff func(retry int) time.Duration
	// Sleep waits (tests replace it). Nil means a context-aware sleep.
	Sleep func(ctx context.Context, d time.Duration) error
}

// ResendSender sends email through the Resend HTTP API
// (POST /emails, bearer authentication, Idempotency-Key header).
//
// One Send call makes a small, bounded number of attempts with backoff for
// transient failures (network errors, 429 rate limits, 5xx, a concurrent
// request with the same idempotency key). Every attempt carries the same
// idempotency key, so a retry after an ambiguous failure cannot produce a
// second email. Longer retry schedules are the caller's job (the outbox).
//
// Open and click tracking cannot be controlled per message in the Resend API;
// they are settings of the sending domain in the Resend dashboard.
type ResendSender struct {
	cfg  ResendConfig
	http *http.Client
}

// NewResendSender validates cfg and returns the sender.
func NewResendSender(cfg ResendConfig) (*ResendSender, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("resend: API key is empty")
	}
	if strings.TrimSpace(cfg.From) == "" {
		return nil, errors.New("resend: sender address is empty")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = ResendAPIBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = resendDefaultAttempts
	}
	if cfg.Backoff == nil {
		cfg.Backoff = func(retry int) time.Duration {
			d := 300 * time.Millisecond
			for i := 1; i < retry; i++ {
				d *= 3
			}
			return d
		}
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepContext
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: resendRequestTimeout}
	}
	return &ResendSender{cfg: cfg, http: client}, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Name implements Sender.
func (s *ResendSender) Name() string { return "resend" }

type resendTag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resendRequest struct {
	From    string      `json:"from"`
	To      []string    `json:"to"`
	Subject string      `json:"subject"`
	HTML    string      `json:"html,omitempty"`
	Text    string      `json:"text,omitempty"`
	ReplyTo string      `json:"reply_to,omitempty"`
	Tags    []resendTag `json:"tags,omitempty"`
}

var (
	tagValueRe  = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	errorNameRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
)

// Send implements Sender.
func (s *ResendSender) Send(ctx context.Context, idempotencyKey string, m Message) (string, error) {
	if idempotencyKey == "" || len(idempotencyKey) > resendMaxIdempotencyKB {
		return "", &SendError{Name: "invalid_idempotency_key"}
	}
	req := resendRequest{From: s.cfg.From, To: []string{m.To}, Subject: m.Subject, HTML: m.HTML, Text: m.Text, ReplyTo: s.cfg.ReplyTo}
	if kind := tagValueRe.ReplaceAllString(m.Kind, "_"); kind != "" {
		req.Tags = []resendTag{{Name: "kind", Value: kind}}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", &SendError{Name: "invalid_request"}
	}

	var last *SendError
	for attempt := 1; attempt <= s.cfg.MaxAttempts; attempt++ {
		id, retryAfter, sendErr := s.once(ctx, idempotencyKey, body)
		if sendErr == nil {
			return id, nil
		}
		last = sendErr
		// A quota is not going to refill within this call: leave it to the caller's schedule.
		if !sendErr.Transient || sendErr.Name == "daily_quota_exceeded" || sendErr.Name == "monthly_quota_exceeded" || attempt == s.cfg.MaxAttempts {
			break
		}
		wait := s.cfg.Backoff(attempt)
		if retryAfter > wait {
			wait = retryAfter
		}
		if err := s.cfg.Sleep(ctx, wait); err != nil {
			return "", &SendError{Name: "cancelled", Transient: true}
		}
	}
	return "", last
}

// once makes a single request. It never returns the raw transport error: that
// text can contain the URL and, in principle, request details.
func (s *ResendSender) once(ctx context.Context, idempotencyKey string, body []byte) (id string, retryAfter time.Duration, sendErr *SendError) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BaseURL+"/emails", bytes.NewReader(body))
	if err != nil {
		return "", 0, &SendError{Name: "invalid_request"}
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	httpReq.Header.Set("User-Agent", "rightproperty365-backend/1")

	resp, err := s.http.Do(httpReq)
	if err != nil {
		name := "network_error"
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			name = "timeout"
		}
		return "", 0, &SendError{Name: name, Transient: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, resendMaxResponseBytes))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var ok struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &ok); err != nil || ok.ID == "" {
			// Accepted but unreadable: retrying with the same idempotency key is safe.
			return "", 0, &SendError{Status: resp.StatusCode, Name: "invalid_response", Transient: true}
		}
		return ok.ID, 0, nil
	}

	name := resendErrorName(raw)
	se := &SendError{Status: resp.StatusCode, Name: name, Detail: s.errorDetail(raw)}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		se.Transient = true
	case resp.StatusCode >= 500:
		se.Transient = true
	case resp.StatusCode == http.StatusConflict && name == "concurrent_idempotent_requests":
		se.Transient = true
	case resp.StatusCode == http.StatusRequestTimeout:
		se.Transient = true
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
		retryAfter = min(time.Duration(secs)*time.Second, resendMaxRetryAfter)
	}
	return "", retryAfter, se
}

const resendMaxDetailLen = 200

var (
	detailAuthRe   = regexp.MustCompile(`(?i)authorization\s*:?\s*((bearer|apikey|basic)\s+)?\S*`)
	detailSchemeRe = regexp.MustCompile(`(?i)\b(bearer|apikey|basic)\s+\S+`)
	detailKeyRe    = regexp.MustCompile(`re_[A-Za-z0-9_]+`)
	detailEmailRe  = regexp.MustCompile(`[^\s@<>"'(),;:]+@[^\s@<>"'(),;:]+`)
	detailSpaceRe  = regexp.MustCompile(`\s+`)
)

// errorDetail returns the provider's free-text message in a form that is safe
// to log. The message can echo request content, so the configured API key,
// anything shaped like a key or an Authorization value, and every email
// address are replaced before it is kept; what is left is plain printable
// text of at most resendMaxDetailLen characters. Domain names stay, because
// "the example.com domain is not verified" is exactly what an operator needs.
func (s *ResendSender) errorDetail(raw []byte) string {
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Message == "" {
		return ""
	}
	msg := body.Message
	if s.cfg.APIKey != "" {
		msg = strings.ReplaceAll(msg, s.cfg.APIKey, "[redacted]")
	}
	msg = detailAuthRe.ReplaceAllString(msg, "[redacted]")
	msg = detailSchemeRe.ReplaceAllString(msg, "[redacted]")
	msg = detailKeyRe.ReplaceAllString(msg, "[redacted]")
	msg = detailEmailRe.ReplaceAllString(msg, "[email]")
	msg = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, msg)
	msg = strings.TrimSpace(detailSpaceRe.ReplaceAllString(msg, " "))
	if len(msg) > resendMaxDetailLen {
		msg = msg[:resendMaxDetailLen] + "..."
	}
	return msg
}

// resendErrorName extracts the provider's short error name. Only a strictly
// formatted name is kept. The free-text message is handled by errorDetail.
func resendErrorName(raw []byte) string {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && errorNameRe.MatchString(body.Name) {
		return body.Name
	}
	return "unknown_error"
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// String keeps the API key out of accidental %v / %+v formatting.
func (s *ResendSender) String() string {
	return fmt.Sprintf("ResendSender{from: %q}", s.cfg.From)
}
