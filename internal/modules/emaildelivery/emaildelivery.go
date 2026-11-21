// Package emaildelivery is what happens to an email after the provider
// accepted it (API 0.59.0, FR-065): Resend's signed delivery webhooks set the
// delivery state of each message (delivered, delayed, bounced, complained);
// a hard bounce or a complaint suppresses the address so it is not emailed
// again (mail.Service skips it, except for the security and account kinds);
// a sweep records bounce and complaint spikes; administrators see the
// figures, the problems, the spikes and the suppressions, and can clear a
// suppression (audited).
//
// Privacy: an address is never stored, only its SHA-256 (mail.RecipientHash),
// as everywhere else in the email log.
package emaildelivery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/mail"
)

// Delivery states.
const (
	StateDelivered  = "DELIVERED"
	StateDelayed    = "DELAYED"
	StateBounced    = "BOUNCED"
	StateComplained = "COMPLAINED"
)

// Suppression reasons.
const (
	ReasonHardBounce = "HARD_BOUNCE"
	ReasonComplaint  = "COMPLAINT"
)

// Alert kinds.
const (
	AlertBounceSpike    = "BOUNCE_SPIKE"
	AlertComplaintSpike = "COMPLAINT_SPIKE"
)

// Audit vocabulary.
const (
	AuditTarget            = "EMAIL_SUPPRESSION"
	ActionSuppressionClear = "EMAIL_SUPPRESSION_CLEARED"
)

// Spike rules: in the last SpikeWindow, at least BounceSpikeMin bounces that
// are at least BounceSpikeRate of the messages accepted, or at least
// ComplaintSpikeMin complaints that are at least ComplaintSpikeRate. One
// alert per kind per window.
const (
	SpikeWindow        = time.Hour
	BounceSpikeMin     = 5
	BounceSpikeRate    = 0.05
	ComplaintSpikeMin  = 3
	ComplaintSpikeRate = 0.001
	// SignatureTolerance is how far a webhook's timestamp may be from now.
	SignatureTolerance = 5 * time.Minute
	// MaxBodyBytes caps a webhook body.
	MaxBodyBytes = 256 << 10
)

// Errors.
var (
	ErrNotConfigured = errors.New("the email webhook secret is not set")
	ErrSignature     = errors.New("the webhook signature does not verify")
	ErrBadPayload    = errors.New("the webhook body is not a delivery event")
	ErrForbidden     = errors.New("not allowed")
	ErrNotFound      = errors.New("no such suppression")
)

// ValidationError carries per-field messages.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// Event is one delivery event, as stored.
type Event struct {
	WebhookID         string
	Type              string // a delivery state
	ProviderMessageID string
	RecipientSHA256   string
	BounceType        string
	Hard              bool
	OccurredAt        time.Time
	ReceivedAt        time.Time
}

// Suppression is one suppressed address.
type Suppression struct {
	RecipientSHA256   string
	Reason            string
	ProviderMessageID string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Alert is one spike.
type Alert struct {
	ID          uuid.UUID
	Kind        string
	WindowStart time.Time
	WindowEnd   time.Time
	Events      int
	Accepted    int
	CreatedAt   time.Time
}

// Counts are the figures of a period.
type Counts struct {
	Accepted   int // messages the provider accepted
	Delivered  int
	Delayed    int
	Bounced    int
	Complained int
	Suppressed int // messages not sent because the address is suppressed
	Failed     int // messages the provider refused (not suppressed)
}

// Problem is a recent message that bounced, complained, was suppressed or failed.
type Problem struct {
	Kind            string
	Status          string
	DeliveryState   string
	BounceType      string
	LastError       string
	RecipientSHA256 string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Store persists the events, states, suppressions and alerts.
type Store interface {
	// Apply records an event once (by WebhookID): it sets the message's
	// delivery state (found by provider message ID, whose recipient hash wins
	// over the event's) and, for a hard bounce or a complaint, suppresses the
	// address. applied is false for a webhook already seen.
	Apply(ctx context.Context, e Event) (applied bool, err error)
	IsSuppressed(ctx context.Context, recipientSHA256 string) (bool, error)
	// Suppressions lists the active ones, newest first; hash filters to one.
	Suppressions(ctx context.Context, hash string, limit int) ([]Suppression, error)
	// Clear clears an active suppression with its audit (ErrNotFound).
	Clear(ctx context.Context, hash string, by uuid.UUID, at time.Time, audit adminaudit.Event) error
	Counts(ctx context.Context, since time.Time) (Counts, error)
	Problems(ctx context.Context, limit int) ([]Problem, error)
	// Window counts bounces, complaints (events received) and accepted
	// messages in [from, to].
	Window(ctx context.Context, from, to time.Time) (bounces, complaints, accepted int, err error)
	// AddAlert stores an alert unless one of its kind was created at or after
	// a.WindowStart; added says whether it did.
	AddAlert(ctx context.Context, a Alert) (added bool, err error)
	Alerts(ctx context.Context, since time.Time, limit int) ([]Alert, error)
}

// Deps wires the service.
type Deps struct {
	Store Store
	// Secret is the webhook signing secret ("whsec_…"); "" disables the
	// webhook (503). Never logged.
	Secret string
	Logger *slog.Logger
	Now    func() time.Time
}

// Service holds the rules.
type Service struct {
	store  Store
	secret []byte
	logger *slog.Logger
	now    func() time.Time
}

// NewService builds the service. A secret that does not decode disables the
// webhook (and is reported once, without its value).
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, logger: d.Logger, now: d.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if d.Secret != "" {
		key, err := DecodeSecret(d.Secret)
		if err != nil {
			s.logger.Error("email webhook: RESEND_WEBHOOK_SECRET does not decode; the webhook is disabled")
		} else {
			s.secret = key
		}
	}
	return s
}

// Configured reports whether the webhook verifies signatures.
func (s *Service) Configured() bool { return len(s.secret) > 0 }

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// IsSuppressed is the mail.SuppressionCheck.
func (s *Service) IsSuppressed(ctx context.Context, hash string) (bool, error) {
	return s.store.IsSuppressed(ctx, hash)
}

// ---------------------------------------------------------------------------
// Webhook
// ---------------------------------------------------------------------------

// DecodeSecret decodes a Svix-style secret: "whsec_" and base64.
func DecodeSecret(secret string) ([]byte, error) {
	raw := strings.TrimPrefix(strings.TrimSpace(secret), "whsec_")
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) < 16 {
		return nil, errors.New("invalid webhook secret")
	}
	return key, nil
}

// Sign returns the "v1,<base64>" signature of a body (tests and tools).
func Sign(key []byte, id string, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%d.", id, ts.Unix())
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Verify checks the svix-id, svix-timestamp and svix-signature headers.
func Verify(key []byte, id, timestamp, signatures string, body []byte, now time.Time) error {
	if id == "" || timestamp == "" || signatures == "" {
		return ErrSignature
	}
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrSignature
	}
	ts := time.Unix(secs, 0)
	if math.Abs(now.Sub(ts).Seconds()) > SignatureTolerance.Seconds() {
		return ErrSignature
	}
	want := Sign(key, id, ts, body)
	for _, sig := range strings.Fields(signatures) {
		if hmac.Equal([]byte(sig), []byte(want)) {
			return nil
		}
	}
	return ErrSignature
}

var webhookIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)

type resendPayload struct {
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Data      struct {
		EmailID string   `json:"email_id"`
		To      []string `json:"to"`
		Bounce  *struct {
			Type    string `json:"type"`
			SubType string `json:"subType"`
		} `json:"bounce"`
	} `json:"data"`
}

var resendTypes = map[string]string{
	"email.delivered":        StateDelivered,
	"email.delivery_delayed": StateDelayed,
	"email.bounced":          StateBounced,
	"email.complained":       StateComplained,
}

// ParseResend reads a Resend event. ok is false for an event type that is
// not about delivery (sent, opened, clicked…): it is acknowledged and ignored.
func ParseResend(webhookID string, body []byte, now time.Time) (Event, bool, error) {
	var p resendPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, false, ErrBadPayload
	}
	state, known := resendTypes[p.Type]
	if !known {
		return Event{}, false, nil
	}
	if !webhookIDRe.MatchString(webhookID) {
		return Event{}, false, ErrBadPayload
	}
	e := Event{WebhookID: webhookID, Type: state, ProviderMessageID: strings.TrimSpace(p.Data.EmailID), OccurredAt: now, ReceivedAt: now}
	if t, err := time.Parse(time.RFC3339Nano, p.CreatedAt); err == nil && !t.After(now) {
		e.OccurredAt = t.UTC().Truncate(time.Microsecond)
	}
	if len(p.Data.To) > 0 && strings.TrimSpace(p.Data.To[0]) != "" {
		e.RecipientSHA256 = mail.RecipientHash(p.Data.To[0])
	}
	switch state {
	case StateBounced:
		if p.Data.Bounce != nil {
			e.BounceType = strings.TrimSpace(p.Data.Bounce.Type)
			if len(e.BounceType) > 40 {
				e.BounceType = e.BounceType[:40]
			}
		}
		// Only a permanent bounce suppresses: a transient one (mailbox
		// full, greylisting) may deliver next time.
		e.Hard = strings.EqualFold(e.BounceType, "Permanent") || strings.EqualFold(e.BounceType, "hard")
	case StateComplained:
		e.Hard = true
	}
	if e.ProviderMessageID == "" && e.RecipientSHA256 == "" {
		return Event{}, false, ErrBadPayload
	}
	return e, true, nil
}

// Webhook verifies and applies one Resend webhook. It returns whether the
// event was new.
func (s *Service) Webhook(ctx context.Context, id, timestamp, signature string, body []byte) (bool, error) {
	if !s.Configured() {
		return false, ErrNotConfigured
	}
	now := s.clock()
	if err := Verify(s.secret, id, timestamp, signature, body, now); err != nil {
		return false, err
	}
	e, ok, err := ParseResend(id, body, now)
	if err != nil || !ok {
		return false, err
	}
	applied, err := s.store.Apply(ctx, e)
	if err != nil {
		return false, err
	}
	if applied && e.Hard {
		s.logger.WarnContext(ctx, "email address suppressed", slog.String("event", e.Type), slog.String("provider_message_id", e.ProviderMessageID))
	}
	return applied, nil
}

// ---------------------------------------------------------------------------
// Spike sweep
// ---------------------------------------------------------------------------

func spike(events, accepted, min int, rate float64) bool {
	if events < min {
		return false
	}
	if accepted == 0 {
		return true
	}
	return float64(events)/float64(accepted) >= rate
}

// Sweep records the spikes of the last SpikeWindow. It returns the alerts added.
func (s *Service) Sweep(ctx context.Context) ([]Alert, error) {
	end := s.clock()
	start := end.Add(-SpikeWindow)
	bounces, complaints, accepted, err := s.store.Window(ctx, start, end)
	if err != nil {
		return nil, err
	}
	var out []Alert
	for _, c := range []struct {
		kind   string
		events int
		hit    bool
	}{
		{AlertBounceSpike, bounces, spike(bounces, accepted, BounceSpikeMin, BounceSpikeRate)},
		{AlertComplaintSpike, complaints, spike(complaints, accepted, ComplaintSpikeMin, ComplaintSpikeRate)},
	} {
		if !c.hit {
			continue
		}
		id, err := uuid.NewV7()
		if err != nil {
			return out, err
		}
		a := Alert{ID: id, Kind: c.kind, WindowStart: start, WindowEnd: end, Events: c.events, Accepted: accepted, CreatedAt: end}
		added, err := s.store.AddAlert(ctx, a)
		if err != nil {
			return out, err
		}
		if added {
			// An ERROR line: the log alerting and the metrics pick it up.
			s.logger.ErrorContext(ctx, "email delivery spike", slog.String("kind", c.kind), slog.Int("events", c.events),
				slog.Int("accepted", accepted), slog.Duration("window", SpikeWindow))
			out = append(out, a)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------

// AdminRoles may see the delivery figures and suppressions and clear one.
func AdminRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformSupport}
}

func allowed(p *auth.Principal) bool { return p != nil && p.HasPlatformRole(AdminRoles()...) }

// Overview is the administrators' page.
type Overview struct {
	Configured bool
	Last24h    Counts
	Last7d     Counts
	Alerts     []Alert
	Problems   []Problem
}

// Overview returns the figures, the spikes of the last 7 days and the recent problems.
func (s *Service) Overview(ctx context.Context, p *auth.Principal) (Overview, error) {
	if !allowed(p) {
		return Overview{}, ErrForbidden
	}
	now := s.clock()
	out := Overview{Configured: s.Configured()}
	var err error
	if out.Last24h, err = s.store.Counts(ctx, now.Add(-24*time.Hour)); err != nil {
		return Overview{}, err
	}
	if out.Last7d, err = s.store.Counts(ctx, now.AddDate(0, 0, -7)); err != nil {
		return Overview{}, err
	}
	if out.Alerts, err = s.store.Alerts(ctx, now.AddDate(0, 0, -7), 50); err != nil {
		return Overview{}, err
	}
	if out.Problems, err = s.store.Problems(ctx, 50); err != nil {
		return Overview{}, err
	}
	return out, nil
}

var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Suppressions lists the active suppressions; email (an address, looked up
// by its hash) narrows to one.
func (s *Service) Suppressions(ctx context.Context, p *auth.Principal, email string) ([]Suppression, error) {
	if !allowed(p) {
		return nil, ErrForbidden
	}
	hash := ""
	if email = strings.TrimSpace(email); email != "" {
		if !strings.Contains(email, "@") || len(email) > 320 {
			return nil, &ValidationError{Fields: map[string]string{"email": "must be an email address"}}
		}
		hash = mail.RecipientHash(email)
	}
	return s.store.Suppressions(ctx, hash, 200)
}

// Clear lifts a suppression (audited with the reason).
func (s *Service) Clear(ctx context.Context, p *auth.Principal, hash, reason string) error {
	if !allowed(p) {
		return ErrForbidden
	}
	hash = strings.ToLower(strings.TrimSpace(hash))
	reason = strings.TrimSpace(reason)
	fields := map[string]string{}
	if !hashRe.MatchString(hash) {
		fields["hash"] = "must be the 64-character hash of a suppressed address"
	}
	if n := len([]rune(reason)); n < 5 || n > 500 {
		fields["reason"] = "must be 5 to 500 characters"
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	now := s.clock()
	audit := adminaudit.New(p.UserID, p.Name, ActionSuppressionClear, AuditTarget, hash, hash[:12]+"…", reason,
		map[string]any{"suppressed": true}, map[string]any{"suppressed": false}, now)
	return s.store.Clear(ctx, hash, p.UserID, now, audit)
}

// Rate is "0.0123" (4 places) or nil when there is no denominator.
func Rate(n, d int) *string {
	if d == 0 {
		return nil
	}
	v := strconv.FormatFloat(float64(n)/float64(d), 'f', 4, 64)
	return &v
}
