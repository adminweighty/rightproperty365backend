package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
)

// The sign-in code: the second step of every sign-in (customer and
// administrator). A correct password no longer starts a session; it starts a
// challenge and a six-digit code is emailed to the account's address. Only
// POST .../login/verify with that code starts the session.
//
// Rules:
//   - the code is 6 digits from crypto/rand, valid for SignInCodeTTL and
//     usable once;
//   - only a keyed hash of it is stored: HMAC-SHA256 keyed with the raw
//     challenge id, which itself is stored only as its SHA-256. Someone who
//     can read the table cannot test codes without the id the client holds;
//   - a challenge belongs to one account and one scope (customer or admin)
//     and is refused by the other scope's verify endpoint;
//   - SignInCodeMaxAttempts wrong codes kill the challenge; a new code can be
//     sent at most every SignInCodeResendInterval and SignInCodeMaxResends
//     times per challenge.
//
// The code is a credential, so its email is sent from the request path like
// the verification and reset links (see Mailer), never through the outbox.
//
// AUTH_DEMO_CODE (development and staging only; config refuses it anywhere
// else) is a fixed code that is ALSO accepted for any live challenge.
const (
	SignInCodeTTL            = 10 * time.Minute
	SignInCodeLength         = 6
	SignInCodeMaxAttempts    = 5
	SignInCodeResendInterval = 30 * time.Second
	SignInCodeMaxResends     = 5
)

// CodeCodeExpired is the API error code of a challenge that can no longer be used.
const CodeCodeExpired = "CODE_EXPIRED"

// Status of the sign-in answer when the password was right.
const statusCodeRequired = "CODE_REQUIRED"

// Messages shown to people.
const (
	msgCodeRequired = "Enter the code we sent you."
	msgCodeWrong    = "That code is not right."
	msgCodeExpired  = "That code has expired or was used too many times. Sign in again."
	msgResendSoon   = "Please wait a moment before asking for another code."
	msgResendLimit  = "You have asked for a new code too many times. Use the last code we sent, or sign in again."
)

var (
	// ErrCodeExpired: the challenge is unknown, expired, already used, used up
	// by wrong codes, or belongs to the other scope.
	ErrCodeExpired = errors.New("sign-in challenge is unknown, expired or used")
)

// ResendLaterError says when another code may be asked for.
type ResendLaterError struct {
	After time.Duration
	// Exhausted: the challenge has had all its resends.
	Exhausted bool
}

func (e *ResendLaterError) Error() string { return "sign-in code resend refused: too soon" }

// Message is the text for the person.
func (e *ResendLaterError) Message() string {
	if e.Exhausted {
		return msgResendLimit
	}
	return msgResendSoon
}

// LoginChallenge is a stored sign-in challenge.
type LoginChallenge struct {
	// IDHash is the SHA-256 of the opaque challenge id given to the client.
	IDHash      []byte
	UserID      uuid.UUID
	Scope       string // auth.ScopeCustomer or auth.ScopeAdmin
	Delivery    string // how the session is handed over once the code is right
	CodeHash    []byte
	ExpiresAt   time.Time
	Attempts    int
	ResendCount int
	LastSentAt  time.Time
	ConsumedAt  *time.Time
	CreatedAt   time.Time
}

// usable reports whether the challenge can still be answered at now.
func (c LoginChallenge) usable(now time.Time) bool {
	return c.ConsumedAt == nil && now.Before(c.ExpiresAt) && c.Attempts < SignInCodeMaxAttempts
}

// CodeChallenge is the answer of the login (and resend) endpoints when the
// password was right.
type CodeChallenge struct {
	Status      string    `json:"status"`
	ChallengeID string    `json:"challenge_id"`
	SentTo      string    `json:"sent_to"`
	ExpiresAt   time.Time `json:"expires_at"`
	CodeLength  int       `json:"code_length"`
}

// WithDemoCode makes code an accepted answer to every live challenge. The
// caller (cmd/api) only passes a value that config allowed: development or
// staging. Empty disables it.
func WithDemoCode(code string) Option { return func(s *Service) { s.demoCode = code } }

// newSignInCode returns SignInCodeLength uniform random digits.
func newSignInCode() (string, error) {
	var b strings.Builder
	for range SignInCodeLength {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", fmt.Errorf("generate sign-in code: %w", err)
		}
		b.WriteByte(byte('0' + n.Int64()))
	}
	return b.String(), nil
}

// signInCodeHash is the keyed hash under which a code is stored.
func signInCodeHash(challengeID, code string) []byte {
	m := hmac.New(sha256.New, []byte(challengeID))
	m.Write([]byte("sign-in-code:" + code))
	return m.Sum(nil)
}

// MaskEmail shows enough of an address to recognise it: "u•••@example.com".
func MaskEmail(email string) string {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" {
		return "•••"
	}
	first := []rune(local)[0]
	return string(first) + "•••@" + domain
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ParseCodeInput reads the "code" member: a JSON string, or a JSON number for
// clients that send one. Anything else is "" (reported as missing).
func ParseCodeInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

// beginChallenge creates a challenge for user in scope and emails its code.
// A failed email is logged, not returned: the person can ask for another.
func (s *Service) beginChallenge(ctx context.Context, user User, scope, delivery string) (CodeChallenge, error) {
	now := s.clock()
	challengeID, idHash, err := newToken()
	if err != nil {
		return CodeChallenge{}, err
	}
	code, err := newSignInCode()
	if err != nil {
		return CodeChallenge{}, err
	}
	c := LoginChallenge{
		IDHash: idHash, UserID: user.ID, Scope: scope, Delivery: delivery,
		CodeHash: signInCodeHash(challengeID, code), ExpiresAt: now.Add(SignInCodeTTL),
		LastSentAt: now, CreatedAt: now,
	}
	if err := s.store.CreateLoginChallenge(ctx, c); err != nil {
		return CodeChallenge{}, fmt.Errorf("store sign-in challenge: %w", err)
	}
	s.sendCode(ctx, user, scope, code)
	return CodeChallenge{
		Status: statusCodeRequired, ChallengeID: challengeID, SentTo: MaskEmail(user.Email),
		ExpiresAt: c.ExpiresAt, CodeLength: SignInCodeLength,
	}, nil
}

func (s *Service) sendCode(ctx context.Context, user User, scope, code string) {
	if err := s.mailer.SendSignInCode(ctx, user.Email, user.Name, code, scope == auth.ScopeAdmin); err != nil {
		s.logger.ErrorContext(ctx, "sign-in code email failed", slog.String("error", err.Error()))
	}
}

// liveChallenge loads a challenge that is usable now in scope.
func (s *Service) liveChallenge(ctx context.Context, challengeID, scope string, now time.Time) (LoginChallenge, error) {
	challengeID = strings.TrimSpace(challengeID)
	if !plausibleToken(challengeID) {
		return LoginChallenge{}, ErrCodeExpired
	}
	c, err := s.store.LoginChallenge(ctx, hashToken(challengeID))
	switch {
	case errors.Is(err, ErrNotFound):
		return LoginChallenge{}, ErrCodeExpired
	case err != nil:
		return LoginChallenge{}, fmt.Errorf("load sign-in challenge: %w", err)
	case c.Scope != scope || !c.usable(now):
		return LoginChallenge{}, ErrCodeExpired
	}
	return c, nil
}

// VerifyCode is the second step: POST /v1/auth/login/verify (scope customer)
// and POST /v1/admin/auth/login/verify (scope admin). On success the result is
// exactly what the login endpoint used to return.
func (s *Service) VerifyCode(ctx context.Context, scope, challengeID, code, clientIP string) (AuthResult, error) {
	if !allDigits(code) {
		return AuthResult{}, &ValidationError{Fields: map[string]string{"code": msgCodeRequired}}
	}
	now := s.clock()
	c, err := s.liveChallenge(ctx, challengeID, scope, now)
	if err != nil {
		return AuthResult{}, err
	}
	challengeID = strings.TrimSpace(challengeID)

	demo := s.demoCode != "" && subtle.ConstantTimeCompare([]byte(code), []byte(s.demoCode)) == 1
	if !demo {
		if len(code) != SignInCodeLength {
			// Not something we could have sent: not counted as an attempt.
			return AuthResult{}, &ValidationError{Fields: map[string]string{"code": msgCodeRequired}}
		}
		if !hmac.Equal(c.CodeHash, signInCodeHash(challengeID, code)) {
			attempts, err := s.store.FailLoginChallenge(ctx, c.IDHash, now)
			if errors.Is(err, ErrNotFound) {
				return AuthResult{}, ErrCodeExpired
			}
			if err != nil {
				return AuthResult{}, fmt.Errorf("record wrong sign-in code: %w", err)
			}
			s.audit(ctx, slog.LevelWarn, "sign_in_code", "failure", slog.String("scope", scope),
				slog.String("user_id", c.UserID.String()), slog.Int("attempts", attempts), slog.String("ip", clientIP))
			if attempts >= SignInCodeMaxAttempts {
				return AuthResult{}, ErrCodeExpired
			}
			return AuthResult{}, &ValidationError{Fields: map[string]string{"code": msgCodeWrong}}
		}
	}
	// Single use, even under concurrency: only one caller consumes it.
	if err := s.store.ConsumeLoginChallenge(ctx, c.IDHash, now); err != nil {
		if errors.Is(err, ErrNotFound) {
			return AuthResult{}, ErrCodeExpired
		}
		return AuthResult{}, fmt.Errorf("consume sign-in challenge: %w", err)
	}

	// The account is judged again: it may have changed since the password step.
	user, err := s.store.UserByID(ctx, c.UserID)
	if errors.Is(err, ErrNotFound) {
		return AuthResult{}, ErrCodeExpired
	}
	if err != nil {
		return AuthResult{}, fmt.Errorf("load user: %w", err)
	}
	isAdmin := len(user.PlatformRoles) > 0
	if user.Status != StatusActive || !user.Verified() || isAdmin != (scope == auth.ScopeAdmin) {
		return AuthResult{}, ErrCodeExpired
	}
	res, err := s.startSession(ctx, user, c.Delivery, scope, now)
	if err != nil {
		return AuthResult{}, err
	}
	attrs := []slog.Attr{slog.String("user_id", user.ID.String()), slog.String("ip", clientIP), slog.Bool("demo_code", demo)}
	if scope == auth.ScopeAdmin {
		s.audit(ctx, slog.LevelInfo, "admin_sign_in", "success", append(attrs, slog.String("email_sha256", emailFingerprint(user.Email)))...)
	} else {
		s.audit(ctx, slog.LevelInfo, "sign_in_code", "success", attrs...)
	}
	return res, nil
}

// ResendCode emails a fresh code for a live challenge: POST .../login/resend.
// The earlier code stops working; the attempt count is kept.
func (s *Service) ResendCode(ctx context.Context, scope, challengeID string) (CodeChallenge, error) {
	now := s.clock()
	c, err := s.liveChallenge(ctx, challengeID, scope, now)
	if err != nil {
		return CodeChallenge{}, err
	}
	challengeID = strings.TrimSpace(challengeID)
	if c.ResendCount >= SignInCodeMaxResends {
		return CodeChallenge{}, &ResendLaterError{After: c.ExpiresAt.Sub(now), Exhausted: true}
	}
	if wait := c.LastSentAt.Add(SignInCodeResendInterval).Sub(now); wait > 0 {
		return CodeChallenge{}, &ResendLaterError{After: wait}
	}
	user, err := s.store.UserByID(ctx, c.UserID)
	if errors.Is(err, ErrNotFound) {
		return CodeChallenge{}, ErrCodeExpired
	}
	if err != nil {
		return CodeChallenge{}, fmt.Errorf("load user: %w", err)
	}
	code, err := newSignInCode()
	if err != nil {
		return CodeChallenge{}, err
	}
	expires := now.Add(SignInCodeTTL)
	err = s.store.ResendLoginChallenge(ctx, c.IDHash, signInCodeHash(challengeID, code), expires, now, now.Add(-SignInCodeResendInterval))
	if errors.Is(err, ErrNotFound) {
		// Lost a race with another resend or a verify.
		return CodeChallenge{}, &ResendLaterError{After: SignInCodeResendInterval}
	}
	if err != nil {
		return CodeChallenge{}, fmt.Errorf("resend sign-in code: %w", err)
	}
	s.sendCode(ctx, user, scope, code)
	return CodeChallenge{
		Status: statusCodeRequired, ChallengeID: challengeID, SentTo: MaskEmail(user.Email),
		ExpiresAt: expires, CodeLength: SignInCodeLength,
	}, nil
}
