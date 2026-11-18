// Package push is the port to the mobile push provider (FCM HTTP v1, which
// also carries APNs for iOS).
//
// An accepted push only means the provider took the message: nothing here
// (and nothing built on it) ever claims a device showed it or a person saw it
// (FR-064, FR-065).
package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/config"
)

// Provider names (also reported by the notifications health endpoint).
const (
	ProviderFCM      = "fcm"
	ProviderLog      = "log"
	ProviderDisabled = "disabled"
)

// Message is one push to one device token. Title and Body are lock-screen
// text: callers put safe text only in them (a reference and a place, never a
// customer's name, phone, email or message).
type Message struct {
	Token string
	Title string
	Body  string
	// Data is delivered to the app: type, link and notification_id.
	Data map[string]string
	// Badge is the app icon badge (the unread count) when set.
	Badge *int
	// CollapseKey lets the provider replace an undelivered older message.
	CollapseKey string
}

// Result is what the provider returned for an accepted message.
type Result struct {
	// MessageID is the provider's identifier of the accepted message.
	MessageID string
}

// Pusher sends one message.
type Pusher interface {
	Name() string
	Send(ctx context.Context, m Message) (Result, error)
}

// Kind classifies a failed send.
type Kind int

const (
	// Transient failures are retried with backoff (network, 429, 5xx, an
	// expired OAuth token).
	Transient Kind = iota
	// Permanent failures will not succeed on retry (a malformed message, a
	// project misconfiguration): the delivery is dead-lettered.
	Permanent
	// InvalidToken means the device token is no longer valid (UNREGISTERED,
	// or INVALID_ARGUMENT / SENDER_ID_MISMATCH about the token): the device
	// is removed and the delivery dead-lettered.
	InvalidToken
)

// Error is a failed send. Its text never contains a credential or a token.
type Error struct {
	Kind Kind
	// Status is the HTTP status (0 for a network failure); Code the
	// provider's error code, for example UNREGISTERED.
	Status int
	Code   string
	// RetryAfter is the provider's Retry-After, when it sent one.
	RetryAfter time.Duration
	Msg        string
}

func (e *Error) Error() string {
	s := "push: " + e.Msg
	if e.Status != 0 {
		s += fmt.Sprintf(" (HTTP %d", e.Status)
		if e.Code != "" {
			s += " " + e.Code
		}
		s += ")"
	}
	return s
}

// KindOf returns the kind of err (Transient for anything that is not an *Error).
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return Transient
}

// ---------------------------------------------------------------------------
// Log and disabled pushers
// ---------------------------------------------------------------------------

// LogPusher is the DEVELOPMENT pusher: it sends nothing and logs the safe
// text of each message (never the token).
type LogPusher struct{ logger *slog.Logger }

// NewLogPusher returns the development pusher.
func NewLogPusher(logger *slog.Logger) *LogPusher { return &LogPusher{logger: logger} }

// Name implements Pusher.
func (p *LogPusher) Name() string { return ProviderLog }

// Send implements Pusher. It never fails.
func (p *LogPusher) Send(ctx context.Context, m Message) (Result, error) {
	badge := -1
	if m.Badge != nil {
		badge = *m.Badge
	}
	p.logger.InfoContext(ctx, "DEV PUSH (not sent)", slog.String("title", m.Title), slog.String("body", m.Body),
		slog.String("type", m.Data["type"]), slog.String("link", m.Data["link"]), slog.Int("badge", badge),
		slog.String("token_hint", TokenHint(m.Token)))
	return Result{MessageID: "log-" + uuid.NewString()}, nil
}

// TokenHint is the last 4 characters of a token, for display and logs.
func TokenHint(token string) string {
	if len(token) <= 4 {
		return "…"
	}
	return "…" + token[len(token)-4:]
}

// ---------------------------------------------------------------------------
// Selection from the configuration
// ---------------------------------------------------------------------------

// Open selects the pusher from the configuration and logs exactly one line
// naming it. It never fails: without usable credentials push is disabled
// (staging, production) or logged (development). It returns nil when push is
// disabled. The service account is never logged nor echoed in an error.
func Open(cfg config.Config, logger *slog.Logger) Pusher {
	p, line := selectPusher(cfg, logger)
	if p == nil {
		logger.Warn(line, slog.String("push_provider", ProviderDisabled), slog.String("app_env", cfg.Env))
		return nil
	}
	logger.Info(line, slog.String("push_provider", p.Name()))
	return p
}

func selectPusher(cfg config.Config, logger *slog.Logger) (Pusher, string) {
	dev := cfg.Env == config.EnvDevelopment
	fallback := func(reason string) (Pusher, string) {
		if dev {
			return NewLogPusher(logger), "push: log pusher selected (development; " + reason + "; NOTHING is sent, pushes are written to this log)"
		}
		return nil, "push disabled: " + reason
	}
	if cfg.FCMProjectID == "" {
		return fallback("FCM_PROJECT_ID not set")
	}
	raw := cfg.FCMServiceAccountJSON.Reveal()
	if raw == "" && cfg.FCMServiceAccountFile != "" {
		b, err := os.ReadFile(cfg.FCMServiceAccountFile)
		if err != nil {
			return fallback("FCM_SERVICE_ACCOUNT_FILE could not be read")
		}
		raw = string(b)
	}
	if strings.TrimSpace(raw) == "" {
		return fallback("no FCM service account (set FCM_SERVICE_ACCOUNT_JSON or FCM_SERVICE_ACCOUNT_FILE)")
	}
	sa, err := ParseServiceAccount([]byte(raw))
	if err != nil {
		return fallback("the FCM service account is not usable (" + err.Error() + ")")
	}
	p, err := NewFCM(FCMConfig{ProjectID: cfg.FCMProjectID, Account: sa})
	if err != nil {
		return fallback("the FCM service account is not usable (" + err.Error() + ")")
	}
	return p, "push provider selected: fcm (pushes are sent through FCM HTTP v1; APNs through FCM)"
}
