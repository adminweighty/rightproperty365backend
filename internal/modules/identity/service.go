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
	"rightproperty365.com/backend/internal/platform/logging"
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
	// Captcha is the answer to a question from GET /v1/auth/captcha. Required.
	Captcha *CaptchaInput `json:"captcha"`
}

// Messages for the "roles" field. Plain language: they are shown to people.
const (
	msgRolesRequired   = "Choose at least one role."
	msgRoleNotAllowed  = "That role cannot be chosen at sign-up."
	msgRoleUnknown     = "Choose roles from the options provided."
	msgRolesDuplicated = "Each role can only be chosen once."
	msgSeekerExclusive = "A property seeker account cannot also have a professional, business or owner role. Choose one or the other."
)

// countSeekerRoles counts the property seeker roles in a selection.
func countSeekerRoles(roles []string) int {
	n := 0
	for _, role := range roles {
		if IsSeekerRole(role) {
			n++
		}
	}
	return n
}

// parseAccountRoles validates the role selection against the allowlist and
// returns it in allowlist order with initial statuses, or a message for the
// "roles" field. The allowlist is the only thing that admits a value: platform
// administrator roles and anything else unknown are refused, as are duplicates.
func parseAccountRoles(raw json.RawMessage, selectable []SelectableRole) ([]AccountRole, string) {
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
		// The platform-role check comes FIRST and does not consult the
		// catalogue: even if someone inserted SUPER_ADMIN into the reference
		// table as a selectable role, it still cannot be chosen at sign-up.
		if auth.IsPlatformRole(role) {
			return nil, msgRoleNotAllowed
		}
		index := slices.IndexFunc(selectable, func(r SelectableRole) bool { return r.Code == role })
		switch {
		case index < 0:
			return nil, msgRoleUnknown // unknown, or deactivated in the reference data
		case slices.Contains(chosen, role):
			return nil, msgRolesDuplicated
		}
		chosen = append(chosen, role)
	}
	// A property seeker is only that: a buyer, a tenant or both, never
	// together with a role from any other group.
	if seekers := countSeekerRoles(chosen); seekers > 0 && seekers < len(chosen) {
		return nil, msgSeekerExclusive
	}
	roles := make([]AccountRole, 0, len(chosen))
	for _, role := range chosen {
		status := AccountRoleActive
		if selectable[slices.IndexFunc(selectable, func(r SelectableRole) bool { return r.Code == role })].RequiresVerification {
			status = AccountRolePendingVerification
		}
		roles = append(roles, AccountRole{Role: role, Status: status})
	}
	sortAccountRoles(roles)
	return roles, ""
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
	hasher Hasher
	logger *slog.Logger
	now    func() time.Time
	// dummyHash is verified against when a login names an unknown account, so
	// that the response takes as long as a real password check.
	dummyHash string
	// roles lists the account roles that can be chosen right now (reference
	// data). Nil means the built-in list.
	roles RoleCatalogue
	// Lifetimes of administrator sessions.
	adminTTL     time.Duration
	adminIdleTTL time.Duration
	// demoCode is AUTH_DEMO_CODE: also accepted as the sign-in code of any
	// live challenge. Empty (always in production) disables it.
	demoCode string
}

// Option customises a Service (used by tests).
type Option func(*Service)

// WithClock overrides the time source.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithHasher overrides the password hashing parameters.
func WithHasher(h Hasher) Option { return func(s *Service) { s.hasher = h } }

// WithAdminSessionTTLs overrides the absolute and idle lifetime of
// administrator sessions. Non-positive values keep the defaults.
func WithAdminSessionTTLs(absolute, idle time.Duration) Option {
	return func(s *Service) {
		if absolute > 0 {
			s.adminTTL = absolute
		}
		if idle > 0 {
			s.adminIdleTTL = idle
		}
	}
}

// WithRoleCatalogue makes registration validate roles against the reference data.
func WithRoleCatalogue(c RoleCatalogue) Option { return func(s *Service) { s.roles = c } }

// selectableRoles returns the roles that may be chosen at sign-up right now.
func (s *Service) selectableRoles(ctx context.Context) ([]SelectableRole, error) {
	if s.roles == nil {
		return DefaultSelectableRoles(), nil
	}
	return s.roles.SelectableRoles(ctx)
}

// NewService builds a Service.
func NewService(store Store, mailer Mailer, logger *slog.Logger, opts ...Option) (*Service, error) {
	s := &Service{
		store: store, mailer: mailer, hasher: DefaultHasher(), logger: logger, now: time.Now,
		adminTTL: DefaultAdminSessionTTL, adminIdleTTL: DefaultAdminSessionIdleTTL,
	}
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

// CheckPassword reports why password is not acceptable (10 to 128
// characters), or nil. The message never contains the password.
func CheckPassword(password string) error {
	if msg := passwordProblem(password); msg != "" {
		return errors.New(msg)
	}
	return nil
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
// A registration must answer a captcha (see captcha.go). Code that creates
// accounts without HTTP (cmd/seed, cmd/adminctl) writes to the store directly
// and does not come through here.
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
	selectable, err := s.selectableRoles(ctx)
	if err != nil {
		return fmt.Errorf("load selectable roles: %w", err)
	}
	accountRoles, msg := parseAccountRoles(in.Roles, selectable)
	if msg != "" {
		fields["roles"] = msg
	}
	captchaID, captchaAnswer, msg := parseCaptchaInput(in.Captcha)
	if msg != "" {
		fields["captcha"] = msg
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}

	// The captcha is judged only once every other field is acceptable (a typo
	// in the email does not use it up), and before the password is hashed,
	// the account is looked up or anything is written or emailed: the outcome
	// is the same whether or not the email already has an account.
	now := s.clock()
	msg, err = s.checkCaptcha(ctx, captchaID, captchaAnswer, now)
	if err != nil {
		return err
	}
	if msg != "" {
		return &ValidationError{Fields: map[string]string{"captcha": msg}}
	}

	passwordHash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return err
	}
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
	if err := s.mailer.SendVerification(ctx, email, name, token); err != nil {
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
	if len(user.PlatformRoles) > 0 {
		// Administrators only ever get a session from the administrator login.
		return AuthResult{User: user, NoSession: true}, nil
	}
	return s.startSession(ctx, user, delivery, auth.ScopeCustomer, now)
}

// Login checks the credentials and, when they are right, starts a sign-in
// challenge and emails its code (see signincode.go). No session exists until
// VerifyCode accepts the code; delivery is remembered for that moment.
//
// Unknown email and wrong password are indistinguishable (ErrInvalidCredentials,
// and exactly one Argon2 verification runs in every case). ErrEmailNotVerified
// is only returned once the correct password has been presented.
//
// Accounts holding a platform role are refused here with the same
// ErrInvalidCredentials, whatever the password: administrators sign in only
// through AdminLogin.
func (s *Service) Login(ctx context.Context, email, password, delivery string) (CodeChallenge, error) {
	user, delivery, err := s.checkCustomerPassword(ctx, email, password, delivery)
	if err != nil {
		return CodeChallenge{}, err
	}
	return s.beginChallenge(ctx, user, auth.ScopeCustomer, delivery)
}

// checkCustomerPassword is the password step of the customer sign-in: the
// account, and the normalised token delivery, when the password is right.
func (s *Service) checkCustomerPassword(ctx context.Context, email, password, delivery string) (User, string, error) {
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
		return User{}, "", &ValidationError{Fields: fields}
	}

	user, err := s.store.UserByEmail(ctx, email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return User{}, "", fmt.Errorf("look up user: %w", err)
	}
	if err != nil || user.Status != StatusActive || user.PasswordHash == "" {
		_, _ = VerifyPassword(s.dummyHash, password)
		return User{}, "", ErrInvalidCredentials
	}
	ok, err := VerifyPassword(user.PasswordHash, password)
	if err != nil {
		return User{}, "", fmt.Errorf("verify password for user %s: %w", user.ID, err)
	}
	if len(user.PlatformRoles) > 0 {
		// Decided only after the password check so timing does not reveal it.
		s.audit(ctx, slog.LevelWarn, "customer_sign_in_refused_platform_account", "failure",
			slog.String("user_id", user.ID.String()), slog.Bool("password_correct", ok))
		return User{}, "", ErrInvalidCredentials
	}
	if !ok {
		return User{}, "", ErrInvalidCredentials
	}
	if !user.Verified() {
		return User{}, "", ErrEmailNotVerified
	}
	return user, delivery, nil
}

// audit writes one structured, log-only audit line marked audit=true. It is
// NOT a durable audit record: it lives wherever the process logs go.
func (s *Service) audit(ctx context.Context, level slog.Level, event, outcome string, attrs ...slog.Attr) {
	base := []slog.Attr{
		slog.Bool("audit", true),
		slog.String("event", event),
		slog.String("outcome", outcome),
		slog.String("trace_id", logging.RequestID(ctx)),
	}
	s.logger.LogAttrs(ctx, level, "audit", append(base, attrs...)...)
}

// emailFingerprint is the hex SHA-256 of the normalised email. Audit lines
// carry this instead of the submitted text: what people type into a login
// form is personal data and is sometimes a password pasted into the wrong
// field. The fingerprint still lets operators group repeated attempts on one
// address and test whether a specific known address was targeted.
func emailFingerprint(email string) string {
	return fmt.Sprintf("%x", hashToken(email))
}

// AdminLogin is the password step of the administrator sign-in. It succeeds
// only for an active, verified account that holds at least one platform role,
// and then starts an admin-scope sign-in challenge (signincode.go); the
// session, always an admin-scope COOKIE session (never a bearer token), is
// started by VerifyCode.
//
// Every failure — unknown email, wrong password, an ordinary customer or
// self-registered professional, an unverified or inactive account — is the
// same ErrInvalidCredentials, and exactly one Argon2 verification (real or
// dummy) runs before the outcome is decided, so the endpoint does not reveal
// which accounts are administrators. Every outcome is audited.
func (s *Service) AdminLogin(ctx context.Context, email, password, clientIP string) (CodeChallenge, error) {
	email = NormalizeEmail(email)
	fields := map[string]string{}
	if email == "" {
		fields["email"] = "is required"
	}
	if password == "" {
		fields["password"] = "is required"
	}
	if len(fields) > 0 {
		return CodeChallenge{}, &ValidationError{Fields: fields}
	}

	user, err := s.store.UserByEmail(ctx, email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return CodeChallenge{}, fmt.Errorf("look up user: %w", err)
	}
	found := err == nil

	hash := s.dummyHash
	if found && user.PasswordHash != "" {
		hash = user.PasswordHash
	}
	ok, err := VerifyPassword(hash, password)
	if err != nil {
		return CodeChallenge{}, fmt.Errorf("verify password: %w", err)
	}

	// The reason is for the audit line only; the client always sees the same 401.
	reason := ""
	switch {
	case !found:
		reason = "unknown_email"
	case user.PasswordHash == "" || !ok:
		reason = "wrong_password"
	case user.Status != StatusActive:
		reason = "account_not_active"
	case len(user.PlatformRoles) == 0:
		reason = "not_a_platform_account"
	case !user.Verified():
		reason = "email_not_verified"
	}
	attrs := []slog.Attr{slog.String("email_sha256", emailFingerprint(email)), slog.String("ip", clientIP)}
	if found {
		attrs = append(attrs, slog.String("user_id", user.ID.String()))
	}
	if reason != "" {
		s.audit(ctx, slog.LevelWarn, "admin_sign_in", "failure", append(attrs, slog.String("reason", reason))...)
		return CodeChallenge{}, ErrInvalidCredentials
	}

	challenge, err := s.beginChallenge(ctx, user, auth.ScopeAdmin, DeliveryCookie)
	if err != nil {
		return CodeChallenge{}, err
	}
	// "success" is audited when the code is accepted (VerifyCode).
	s.audit(ctx, slog.LevelInfo, "admin_sign_in", "code_sent", attrs...)
	return challenge, nil
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

// sessionTTLs returns the absolute and idle lifetime for a session scope.
func (s *Service) sessionTTLs(scope string) (absolute, idle time.Duration) {
	if scope == auth.ScopeAdmin {
		return s.adminTTL, s.adminIdleTTL
	}
	return SessionAbsoluteTTL, SessionIdleTTL
}

// touchInterval is how stale last_seen may get before a request refreshes it:
// a minute for customer sessions, and never more than a tenth of the idle
// window so a short administrator idle timeout stays accurate.
func touchInterval(idle time.Duration) time.Duration {
	return min(SessionTouchInterval, idle/10)
}

func (s *Service) startSession(ctx context.Context, user User, delivery, scope string, now time.Time) (AuthResult, error) {
	if scope == auth.ScopeAdmin && delivery != DeliveryCookie {
		return AuthResult{}, errors.New("admin sessions are cookie-only")
	}
	ttl, _ := s.sessionTTLs(scope)
	token, hash, err := newToken()
	if err != nil {
		return AuthResult{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return AuthResult{}, fmt.Errorf("generate session id: %w", err)
	}
	sess := Session{
		ID: id, UserID: user.ID, TokenHash: hash, Scope: scope, Delivery: delivery,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(ttl),
	}
	var events []OutboxEvent
	if scope == auth.ScopeAdmin {
		// The administrator gets a sign-in notice by email (sent by the worker).
		events = append(events, OutboxEvent{Type: EventAdminSignedIn, UserID: user.ID, OccurredAt: now})
	}
	if err := s.store.CreateSession(ctx, sess, events...); err != nil {
		return AuthResult{}, fmt.Errorf("create session: %w", err)
	}
	s.logger.InfoContext(ctx, "session started",
		slog.String("session_user_id", user.ID.String()), slog.String("delivery", delivery), slog.String("scope", scope))
	return AuthResult{User: user, Token: token, Delivery: delivery, Scope: scope, ExpiresAt: sess.ExpiresAt, TTL: ttl}, nil
}

// Logout revokes one session.
func (s *Service) Logout(ctx context.Context, sessionID uuid.UUID) error {
	return s.store.RevokeSession(ctx, sessionID, s.clock())
}

// RequestPasswordReset emails a reset link when email belongs to an active
// account and silently does nothing otherwise.
//
// Accounts holding a platform role never get a reset email: there is no MFA
// yet, so a compromised mailbox must not become an administrator takeover.
// Their passwords are changed with cmd/adminctl.
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
	case len(user.PlatformRoles) > 0:
		s.audit(ctx, slog.LevelWarn, "password_reset_refused_platform_account", "refused", slog.String("user_id", user.ID.String()))
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
	if err := s.mailer.SendPasswordReset(ctx, user.Email, user.Name, token); err != nil {
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
// is not revoked, before its absolute expiry, used within the idle window of
// its scope (14 days for customers, 30 minutes by default for administrators)
// and its user is active. An admin-scope session is also invalid as soon as
// its user no longer holds a platform role. last_seen is refreshed at most
// once per touchInterval, not on every request.
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
	if sess.Scope != auth.ScopeCustomer && sess.Scope != auth.ScopeAdmin {
		return nil, auth.ErrNoSession
	}
	_, idle := s.sessionTTLs(sess.Scope)
	now := s.clock()
	if sess.RevokedAt != nil || !now.Before(sess.ExpiresAt) || !now.Before(sess.LastSeenAt.Add(idle)) {
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
	if sess.Scope == auth.ScopeAdmin && len(user.PlatformRoles) == 0 {
		return nil, auth.ErrNoSession
	}
	if now.Sub(sess.LastSeenAt) >= touchInterval(idle) {
		if err := s.store.TouchSession(ctx, sess.ID, now); err != nil {
			// Not worth failing the request over; the idle timer just runs on.
			s.logger.WarnContext(ctx, "could not refresh session last_seen", slog.String("error", err.Error()))
		}
	}
	return &auth.Principal{
		UserID: user.ID, Name: user.Name, Email: user.Email,
		PlatformRoles: user.PlatformRoles, Memberships: user.Memberships,
		AccountRoles: principalAccountRoles(user.AccountRoles),
		SessionID:    sess.ID, SessionScope: sess.Scope, SessionExpiresAt: sess.ExpiresAt,
	}, nil
}

func principalAccountRoles(roles []AccountRole) []auth.AccountRole {
	out := make([]auth.AccountRole, 0, len(roles))
	for _, r := range roles {
		out = append(out, auth.AccountRole{Role: r.Role, Status: r.Status})
	}
	return out
}
