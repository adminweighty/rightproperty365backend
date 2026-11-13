package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
)

const (
	maxNameRunes      = 120
	maxEmailLength    = 254
	maxPrivacyVersion = 32
)

var (
	emailRe   = regexp.MustCompile(`^[^\s@<>"]+@[^\s@<>"]+\.[^\s@<>".]+$`)
	htmlTagRe = regexp.MustCompile(`<[/!?]?[A-Za-z][^>]*>|<!--`)
)

// RegisterInput is the JSON body of POST /v1/auth/register.
type RegisterInput struct {
	Name           string        `json:"name"`
	Email          string        `json:"email"`
	Password       string        `json:"password"`
	Consent        *ConsentInput `json:"consent"`
	MarketingOptIn bool          `json:"marketing_opt_in"`
	// Roles is the account-role selection. It is kept raw so that a wrong
	// shape (not an array, a non-string entry) is reported as a validation
	// error on the "roles" field instead of a generic INVALID_JSON.
	Roles json.RawMessage `json:"roles"`
}

// Messages for the "roles" field. Plain language: they are shown to people.
const (
	msgRolesRequired   = "Choose at least one role."
	msgRoleNotAllowed  = "That role cannot be chosen at sign-up."
	msgRoleUnknown     = "Choose roles from the options provided."
	msgRolesDuplicated = "Each role can only be chosen once."
)

// parseAccountRoles validates the role selection against the allowlist and
// returns it in allowlist order with initial statuses, or a message for the
// "roles" field. The allowlist is the only thing that admits a value: platform
// administrator roles and anything else unknown are refused, as are duplicates.
func parseAccountRoles(raw json.RawMessage) ([]AccountRole, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, msgRolesRequired
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, msgRoleUnknown // not an array
	}
	if len(entries) == 0 {
		return nil, msgRolesRequired
	}
	chosen := make([]string, 0, len(entries))
	for _, entry := range entries {
		var role string
		if err := json.Unmarshal(entry, &role); err != nil || string(entry) == "null" {
			return nil, msgRoleUnknown // not a string
		}
		switch {
		case isPlatformRole(role):
			return nil, msgRoleNotAllowed
		case !slices.Contains(selectableAccountRoles, role):
			return nil, msgRoleUnknown
		case slices.Contains(chosen, role):
			return nil, msgRolesDuplicated
		}
		chosen = append(chosen, role)
	}
	roles := make([]AccountRole, 0, len(chosen))
	for _, role := range chosen {
		roles = append(roles, AccountRole{Role: role, Status: initialAccountRoleStatus(role)})
	}
	sortAccountRoles(roles)
	return roles, ""
}

// isPlatformRole reports whether role is a platform administrator role. It
// only selects the wording of the rejection; the allowlist does the rejecting.
func isPlatformRole(role string) bool {
	switch role {
	case auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformMarketAdmin, auth.PlatformFinanceAdmin,
		auth.PlatformContentAdmin, auth.PlatformModerator, auth.PlatformSupport:
		return true
	}
	return false
}

// ConsentInput records acceptance of a specific privacy notice version.
type ConsentInput struct {
	PrivacyVersion string `json:"privacy_version"`
	Accepted       bool   `json:"accepted"`
}

// Service holds the identity business rules.
type Service struct {
	store  Store
	mailer Mailer
	links  Links
	hasher Hasher
	logger *slog.Logger
	now    func() time.Time
	// dummyHash is verified against when a login names an unknown account, so
	// that the response takes as long as a real password check.
	dummyHash string
}

// Option customises a Service (used by tests).
type Option func(*Service)

// WithClock overrides the time source.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithHasher overrides the password hashing parameters.
func WithHasher(h Hasher) Option { return func(s *Service) { s.hasher = h } }

// NewService builds a Service.
func NewService(store Store, mailer Mailer, links Links, logger *slog.Logger, opts ...Option) (*Service, error) {
	s := &Service{store: store, mailer: mailer, links: links, hasher: DefaultHasher(), logger: logger, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	// A hash of a random value nobody knows: it can never match.
	secret, _, err := newToken()
	if err != nil {
		return nil, err
	}
	if s.dummyHash, err = s.hasher.Hash(secret); err != nil {
		return nil, fmt.Errorf("prepare dummy hash: %w", err)
	}
	return s, nil
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// NormalizeEmail trims and lower-cases an email address.
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func validEmail(email string) bool {
	return email != "" && len(email) <= maxEmailLength && emailRe.MatchString(email)
}

// passwordProblem returns "" when password is acceptable.
func passwordProblem(password string) string {
	switch n := utf8.RuneCountInString(password); {
	case password == "":
		return "is required"
	case !utf8.ValidString(password):
		return "contains characters that are not allowed"
	case n < MinPasswordLength:
		return fmt.Sprintf("must be at least %d characters", MinPasswordLength)
	case n > MaxPasswordLength:
		return fmt.Sprintf("must be at most %d characters", MaxPasswordLength)
	}
	return ""
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// Register creates an unverified account and sends a verification email.
//
// Enumeration safety: for valid input the caller gets the same outcome (nil)
// whether or not the email already has an account, and the password is hashed
// before the lookup so both paths cost about the same.
//   - new email: account created, verification sent;
//   - existing UNVERIFIED account: its name, password and role selection are
//     replaced by this registration and earlier verification links stop working, then a new
//     verification is sent (so a stranger who registered someone else's
//     address first cannot keep their own password on it);
//   - existing VERIFIED account: nothing changes; the owner gets a "you
//     already have an account" email.
//
// The selected account roles only record what the person says they are: no
// organisation or membership is created and no access is granted.
//
// Register never signs the caller in.
func (s *Service) Register(ctx context.Context, in RegisterInput) error {
	fields := map[string]string{}

	name := strings.TrimSpace(in.Name)
	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		fields["name"] = "is required"
	case n > maxNameRunes:
		fields["name"] = "must be at most 120 characters"
	case !utf8.ValidString(name) || hasControlChars(name) || htmlTagRe.MatchString(name):
		fields["name"] = "contains characters that are not allowed"
	}

	email := NormalizeEmail(in.Email)
	switch {
	case email == "":
		fields["email"] = "is required"
	case !validEmail(email):
		fields["email"] = "must be a valid email address"
	}

	if msg := passwordProblem(in.Password); msg != "" {
		fields["password"] = msg
	}

	privacyVersion := ""
	if in.Consent == nil || !in.Consent.Accepted {
		fields["consent.accepted"] = "must be true"
	}
	if in.Consent != nil {
		privacyVersion = strings.TrimSpace(in.Consent.PrivacyVersion)
		switch {
		case privacyVersion == "":
			fields["consent.privacy_version"] = "is required"
		case len(privacyVersion) > maxPrivacyVersion || !isPrintableASCII(privacyVersion):
			fields["consent.privacy_version"] = "is not a valid privacy notice version"
		}
	}
	accountRoles, msg := parseAccountRoles(in.Roles)
	if msg != "" {
		fields["roles"] = msg
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}

	passwordHash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return err
	}
	now := s.clock()
	nu := NewUser{
		Name: name, Email: email, PasswordHash: passwordHash,
		MarketingOptIn: in.MarketingOptIn, PrivacyVersion: privacyVersion, AccountRoles: accountRoles, CreatedAt: now,
	}

	existing, err := s.store.UserByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		if nu.ID, err = uuid.NewV7(); err != nil {
			return fmt.Errorf("generate user id: %w", err)
		}
		if err := s.store.CreateUser(ctx, nu); err != nil {
			if errors.Is(err, ErrEmailTaken) {
				return nil // lost a race with a concurrent registration; same answer
			}
			return fmt.Errorf("create user: %w", err)
		}
		s.logger.InfoContext(ctx, "user registered", slog.String("registered_user_id", nu.ID.String()))
		return s.sendVerification(ctx, nu.ID, email, name, now)
	case err != nil:
		return fmt.Errorf("look up user: %w", err)
	case existing.Status != StatusActive:
		return nil
	case existing.Verified():
		if err := s.mailer.SendAccountExists(ctx, existing.Email, existing.Name); err != nil {
			s.logger.ErrorContext(ctx, "account-exists email failed", slog.String("error", err.Error()))
		}
		return nil
	default:
		nu.ID = existing.ID
		if err := s.store.UpdatePendingRegistration(ctx, nu); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil // verified in the meantime
			}
			return fmt.Errorf("update pending registration: %w", err)
		}
		return s.sendVerification(ctx, existing.ID, email, name, now)
	}
}

// sendVerification issues a verification token and emails the link. A failed
// send is logged, not returned: the customer can ask for another email.
func (s *Service) sendVerification(ctx context.Context, userID uuid.UUID, email, name string, now time.Time) error {
	token, hash, err := newToken()
	if err != nil {
		return err
	}
	if err := s.store.CreateVerificationToken(ctx, userID, hash, now.Add(VerificationTokenTTL), now); err != nil {
		return fmt.Errorf("store verification token: %w", err)
	}
	if err := s.mailer.SendVerification(ctx, email, name, s.links.VerifyEmail(token)); err != nil {
		s.logger.ErrorContext(ctx, "verification email failed", slog.String("error", err.Error()))
	}
	return nil
}

// ResendVerification sends a fresh verification link when email belongs to an
// active, still-unverified account, and silently does nothing otherwise.
func (s *Service) ResendVerification(ctx context.Context, email string) error {
	email = NormalizeEmail(email)
	if !validEmail(email) {
		return &ValidationError{Fields: map[string]string{"email": "must be a valid email address"}}
	}
	user, err := s.store.UserByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("look up user: %w", err)
	case user.Status != StatusActive || user.Verified():
		return nil
	}
	return s.sendVerification(ctx, user.ID, user.Email, user.Name, s.clock())
}

// VerifyEmail consumes a verification token, marks the email verified and
// starts a session.
func (s *Service) VerifyEmail(ctx context.Context, token, delivery string) (AuthResult, error) {
	delivery, err := normalizeDelivery(delivery)
	if err != nil {
		return AuthResult{}, err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return AuthResult{}, &ValidationError{Fields: map[string]string{"token": "is required"}}
	}
	if !plausibleToken(token) {
		return AuthResult{}, ErrInvalidToken
	}
	now := s.clock()
	userID, err := s.store.ConsumeVerificationToken(ctx, hashToken(token), now)
	if err != nil {
		if errors.Is(err, ErrInvalidToken) {
			return AuthResult{}, ErrInvalidToken
		}
		return AuthResult{}, fmt.Errorf("consume verification token: %w", err)
	}
	user, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return AuthResult{}, fmt.Errorf("load user: %w", err)
	}
	if user.Status != StatusActive {
		return AuthResult{}, ErrInvalidToken
	}
	s.logger.InfoContext(ctx, "email verified", slog.String("verified_user_id", user.ID.String()))
	return s.startSession(ctx, user, delivery, now)
}

// Login checks the credentials and starts a session.
//
// Unknown email and wrong password are indistinguishable (ErrInvalidCredentials,
// and an Argon2 verification runs in both cases). ErrEmailNotVerified is only
// returned once the correct password has been presented.
func (s *Service) Login(ctx context.Context, email, password, delivery string) (AuthResult, error) {
	delivery, err := normalizeDelivery(delivery)
	fields := map[string]string{}
	if err != nil {
		fields["token_delivery"] = "must be cookie or bearer"
	}
	email = NormalizeEmail(email)
	if email == "" {
		fields["email"] = "is required"
	}
	if password == "" {
		fields["password"] = "is required"
	}
	if len(fields) > 0 {
		return AuthResult{}, &ValidationError{Fields: fields}
	}

	user, err := s.store.UserByEmail(ctx, email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return AuthResult{}, fmt.Errorf("look up user: %w", err)
	}
	if err != nil || user.Status != StatusActive || user.PasswordHash == "" {
		_, _ = VerifyPassword(s.dummyHash, password)
		return AuthResult{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(user.PasswordHash, password)
	if err != nil {
		return AuthResult{}, fmt.Errorf("verify password for user %s: %w", user.ID, err)
	}
	if !ok {
		return AuthResult{}, ErrInvalidCredentials
	}
	if !user.Verified() {
		return AuthResult{}, ErrEmailNotVerified
	}
	return s.startSession(ctx, user, delivery, s.clock())
}

func normalizeDelivery(delivery string) (string, error) {
	switch delivery {
	case "", DeliveryCookie:
		return DeliveryCookie, nil
	case DeliveryBearer:
		return DeliveryBearer, nil
	}
	return "", &ValidationError{Fields: map[string]string{"token_delivery": "must be cookie or bearer"}}
}

func (s *Service) startSession(ctx context.Context, user User, delivery string, now time.Time) (AuthResult, error) {
	token, hash, err := newToken()
	if err != nil {
		return AuthResult{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return AuthResult{}, fmt.Errorf("generate session id: %w", err)
	}
	sess := Session{
		ID: id, UserID: user.ID, TokenHash: hash, Delivery: delivery,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(SessionAbsoluteTTL),
	}
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return AuthResult{}, fmt.Errorf("create session: %w", err)
	}
	s.logger.InfoContext(ctx, "session started",
		slog.String("session_user_id", user.ID.String()), slog.String("delivery", delivery))
	return AuthResult{User: user, Token: token, Delivery: delivery, ExpiresAt: sess.ExpiresAt}, nil
}

// Logout revokes one session.
func (s *Service) Logout(ctx context.Context, sessionID uuid.UUID) error {
	return s.store.RevokeSession(ctx, sessionID, s.clock())
}

// RequestPasswordReset emails a reset link when email belongs to an active
// account and silently does nothing otherwise.
func (s *Service) RequestPasswordReset(ctx context.Context, email string) error {
	email = NormalizeEmail(email)
	if !validEmail(email) {
		return &ValidationError{Fields: map[string]string{"email": "must be a valid email address"}}
	}
	user, err := s.store.UserByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("look up user: %w", err)
	case user.Status != StatusActive:
		return nil
	}
	now := s.clock()
	token, hash, err := newToken()
	if err != nil {
		return err
	}
	if err := s.store.CreatePasswordResetToken(ctx, user.ID, hash, now.Add(ResetTokenTTL), now); err != nil {
		return fmt.Errorf("store reset token: %w", err)
	}
	if err := s.mailer.SendPasswordReset(ctx, user.Email, user.Name, s.links.ResetPassword(token)); err != nil {
		s.logger.ErrorContext(ctx, "password reset email failed", slog.String("error", err.Error()))
	}
	return nil
}

// ConfirmPasswordReset sets a new password using a reset token and revokes
// every session of that user. Because the token was delivered to the mailbox,
// a successful reset also marks the email verified.
func (s *Service) ConfirmPasswordReset(ctx context.Context, token, password string) error {
	fields := map[string]string{}
	token = strings.TrimSpace(token)
	if token == "" {
		fields["token"] = "is required"
	}
	if msg := passwordProblem(password); msg != "" {
		fields["password"] = msg
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	if !plausibleToken(token) {
		return ErrInvalidToken
	}
	passwordHash, err := s.hasher.Hash(password)
	if err != nil {
		return err
	}
	userID, err := s.store.ResetPassword(ctx, hashToken(token), passwordHash, s.clock())
	if err != nil {
		if errors.Is(err, ErrInvalidToken) {
			return ErrInvalidToken
		}
		return fmt.Errorf("reset password: %w", err)
	}
	s.logger.InfoContext(ctx, "password reset; all sessions revoked", slog.String("reset_user_id", userID.String()))
	return nil
}

// User returns the current state of an account.
func (s *Service) User(ctx context.Context, id uuid.UUID) (User, error) {
	return s.store.UserByID(ctx, id)
}

// ResolveSession implements auth.SessionResolver. A session is valid while it
// is not revoked, younger than SessionAbsoluteTTL, used within SessionIdleTTL
// and its user is active. last_seen is refreshed at most once per
// SessionTouchInterval, not on every request.
func (s *Service) ResolveSession(ctx context.Context, token string) (*auth.Principal, error) {
	if !plausibleToken(token) {
		return nil, auth.ErrNoSession
	}
	sess, err := s.store.SessionByTokenHash(ctx, hashToken(token))
	if errors.Is(err, ErrNotFound) {
		return nil, auth.ErrNoSession
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	now := s.clock()
	if sess.RevokedAt != nil || !now.Before(sess.ExpiresAt) || !now.Before(sess.LastSeenAt.Add(SessionIdleTTL)) {
		return nil, auth.ErrNoSession
	}
	user, err := s.store.UserByID(ctx, sess.UserID)
	if errors.Is(err, ErrNotFound) {
		return nil, auth.ErrNoSession
	}
	if err != nil {
		return nil, fmt.Errorf("load session user: %w", err)
	}
	if user.Status != StatusActive {
		return nil, auth.ErrNoSession
	}
	if now.Sub(sess.LastSeenAt) >= SessionTouchInterval {
		if err := s.store.TouchSession(ctx, sess.ID, now); err != nil {
			// Not worth failing the request over; the idle timer just runs on.
			s.logger.WarnContext(ctx, "could not refresh session last_seen", slog.String("error", err.Error()))
		}
	}
	return &auth.Principal{
		UserID: user.ID, Name: user.Name, Email: user.Email,
		PlatformRoles: user.PlatformRoles, Memberships: user.Memberships,
		SessionID: sess.ID,
	}, nil
}
