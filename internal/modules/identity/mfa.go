package identity

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/secretbox"
	"rightproperty365.com/backend/internal/platform/totp"
)

// Administrator MFA (API 0.40.0, FR-001): an authenticator app (TOTP, RFC
// 6238) and ten one-time recovery codes, for accounts holding a platform role.
//
// Sign-in: password, then the emailed code (signincode.go), then — for an
// administrator with a confirmed authenticator — a TOTP or recovery code on
// the SAME challenge (POST /v1/admin/auth/login/mfa). AUTH_DEMO_CODE only
// ever answers the emailed-code step.
//
// When ADMIN_MFA_REQUIRED is on, an administrator without a confirmed
// authenticator is signed in to an enrolment-only session
// (auth.Principal.MFAEnrolmentOnly): it can only set up and confirm an
// authenticator, read its MFA status and sign out. The restriction is judged
// on every request (ResolveSession), so confirming lifts it at once.
//
// Secrets are sealed with AES-256-GCM (secretbox) bound to the user ID;
// recovery codes are stored as Argon2id hashes. Neither a secret nor a code
// is ever logged.
const (
	// MFAIssuer names the account in authenticator apps.
	MFAIssuer = "RightProperty365"
	// MFAMethodTOTP is the only method (WebAuthn is not implemented).
	MFAMethodTOTP = "totp"
	// MFAMethodRecoveryCode names the recovery-code answer of the sign-in step.
	MFAMethodRecoveryCode = "recovery_code"
	// MFASetupTTL is how long a pending setup can be confirmed.
	MFASetupTTL = 15 * time.Minute
	// RecoveryCodeCount codes are issued at confirm and regeneration.
	RecoveryCodeCount = 10
	// recoveryCodeLength characters of recoveryAlphabet (about 49 bits).
	recoveryCodeLength = 10
	recoveryAlphabet   = "abcdefghjkmnpqrstuvwxyz23456789"
	// MFA reset reasons, in characters.
	minMFAResetReason = 3
	maxMFAResetReason = 1000
)

// Status of the emailed-code answer when a second factor is needed.
const statusMFARequired = "MFA_REQUIRED"

// API error codes of administrator MFA.
const (
	CodeMFAEnrolmentRequired = auth.CodeMFAEnrolmentRequired
	CodeMFAUnavailable       = "MFA_UNAVAILABLE"
	CodeMFAAlreadyEnrolled   = "MFA_ALREADY_ENROLLED"
	CodeMFANotEnrolled       = "MFA_NOT_ENROLLED"
	CodeMFASetupRequired     = "MFA_SETUP_REQUIRED"
	CodeMFARemovalNotAllowed = "MFA_REMOVAL_NOT_ALLOWED"
)

// Messages shown to people.
const (
	msgTOTPRequired      = "Enter the 6-digit code from your authenticator app."
	msgTOTPReused        = "That code was already used. Wait for the next code."
	msgRecoveryCodeWrong = "That recovery code is not right or was already used."
	msgMFAEnterOne       = "Enter either a code from your authenticator app or a recovery code."
)

// Log audit events and outcomes; the display position of the recovery code dash.
const (
	outcomeFailure            = "failure"
	auditEventAdminMFA        = "admin_mfa"
	auditEventAdminMFASignIn  = "admin_mfa_sign_in"
	recoveryCodeDisplayDashAt = 5
)

// Outbox events of administrator MFA (security notices by email).
const (
	// EventAdminMFARecoveryCodeUsed: an administrator signed in with a recovery code.
	EventAdminMFARecoveryCodeUsed = "ADMIN_MFA_RECOVERY_CODE_USED"
	// EventAdminMFAReset: a SUPER_ADMIN reset an administrator's MFA.
	EventAdminMFAReset = "ADMIN_MFA_RESET"
)

var (
	// ErrMFAUnavailable: no encryption key is configured.
	ErrMFAUnavailable = errors.New("admin mfa is unavailable: no encryption key")
	// ErrMFAAlreadyEnrolled: a confirmed authenticator exists.
	ErrMFAAlreadyEnrolled = errors.New("admin mfa already enrolled")
	// ErrMFANotEnrolled: no confirmed authenticator.
	ErrMFANotEnrolled = errors.New("admin mfa not enrolled")
	// ErrMFASetupRequired: no pending setup (or it expired).
	ErrMFASetupRequired = errors.New("admin mfa setup required")
	// ErrMFARemovalNotAllowed: MFA is required, so it cannot be removed.
	ErrMFARemovalNotAllowed = errors.New("admin mfa removal not allowed while required")
	// ErrMFACodeReused: the code's step is not newer than the last accepted one.
	ErrMFACodeReused = errors.New("admin mfa code already used")
	// ErrMFAForbidden: the caller may not do this (own reset, not an admin session).
	ErrMFAForbidden = errors.New("admin mfa action forbidden")
)

// AdminMFA is an administrator's stored authenticator.
type AdminMFA struct {
	UserID       uuid.UUID
	SecretSealed []byte
	// ConfirmedAt is nil while the setup is pending.
	ConfirmedAt  *time.Time
	LastUsedStep int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RecoveryCode is one stored (hashed) recovery code.
type RecoveryCode struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Hash      string
	UsedAt    *time.Time
	CreatedAt time.Time
}

// MFAStore persists administrator MFA. Every method that changes state and
// takes an adminaudit.Event writes it in the same transaction.
type MFAStore interface {
	// AdminMFA returns the user's authenticator (pending or confirmed), or ErrNotFound.
	AdminMFA(ctx context.Context, userID uuid.UUID) (AdminMFA, error)
	// PutPendingTOTP stores (or replaces) a pending setup. ErrMFAAlreadyEnrolled
	// when a confirmed authenticator exists.
	PutPendingTOTP(ctx context.Context, userID uuid.UUID, sealed []byte, now time.Time) error
	// ConfirmTOTP confirms the pending setup created after notBefore, records
	// step as the last used step and replaces the recovery codes with
	// hashes. ErrNotFound when there is no such pending setup.
	ConfirmTOTP(ctx context.Context, userID uuid.UUID, step int64, hashes []string, notBefore, now time.Time, audit adminaudit.Event) error
	// UseTOTPStep records step as used when it is newer than the last used
	// one: ErrMFACodeReused otherwise, ErrNotFound without a confirmed authenticator.
	UseTOTPStep(ctx context.Context, userID uuid.UUID, step int64, now time.Time) error
	// UnusedRecoveryCodes returns the user's unused recovery codes.
	UnusedRecoveryCodes(ctx context.Context, userID uuid.UUID) ([]RecoveryCode, error)
	// UseRecoveryCode marks an unused code used and writes the audit record
	// and the notice event. ErrNotFound when it is gone or already used.
	UseRecoveryCode(ctx context.Context, userID, codeID uuid.UUID, now time.Time, audit adminaudit.Event, event OutboxEvent) error
	// ReplaceRecoveryCodes replaces every recovery code of a confirmed
	// authenticator. ErrNotFound without one.
	ReplaceRecoveryCodes(ctx context.Context, userID uuid.UUID, hashes []string, now time.Time, audit adminaudit.Event) error
	// DeleteAdminMFA deletes the authenticator and recovery codes, revokes
	// every session of the user when revokeSessions, and writes the audit
	// record and events. ErrNotFound when there was nothing to delete.
	DeleteAdminMFA(ctx context.Context, userID uuid.UUID, now time.Time, audit adminaudit.Event, revokeSessions bool, events ...OutboxEvent) error
	// RecordAdminAudit writes one administrators' audit record on its own.
	RecordAdminAudit(ctx context.Context, e adminaudit.Event) error

	// MarkLoginChallengeCodeVerified records that the emailed code of a
	// usable challenge was right and gives it a new expiry: it now waits for
	// the second factor. ErrNotFound when no usable, unverified challenge matched.
	MarkLoginChallengeCodeVerified(ctx context.Context, idHash []byte, now, expiresAt time.Time) error
	// FailLoginChallengeMFA counts one wrong second-factor code and returns
	// the new count. ErrNotFound when the challenge is gone or used.
	FailLoginChallengeMFA(ctx context.Context, idHash []byte, now time.Time) (int, error)
}

// MFAChallenge is the answer of the emailed-code step when a second factor is needed.
type MFAChallenge struct {
	Status      string    `json:"status"`
	ChallengeID string    `json:"challenge_id"`
	Methods     []string  `json:"methods"`
	ExpiresAt   time.Time `json:"expires_at"`
	CodeLength  int       `json:"code_length"`
}

// MFAStatus is GET /v1/admin/auth/mfa.
type MFAStatus struct {
	Enrolled               bool       `json:"enrolled"`
	Method                 *string    `json:"method"`
	CreatedAt              *time.Time `json:"created_at"`
	RecoveryCodesRemaining int        `json:"recovery_codes_remaining"`
	Required               bool       `json:"required"`
	PendingSetup           bool       `json:"pending_setup"`
	WebAuthnAvailable      bool       `json:"webauthn_available"`
}

// TOTPSetup is the answer of POST /v1/admin/auth/mfa/totp/setup. The
// secret is shown here once more for manual entry; it is never logged.
type TOTPSetup struct {
	SecretBase32 string    `json:"secret_base32"`
	OTPAuthURI   string    `json:"otpauth_uri"`
	Issuer       string    `json:"issuer"`
	AccountName  string    `json:"account_name"`
	Algorithm    string    `json:"algorithm"`
	Digits       int       `json:"digits"`
	Period       int       `json:"period"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// RecoveryCodesResult is the answer of confirm and regenerate: the codes
// are shown once.
type RecoveryCodesResult struct {
	RecoveryCodes []string  `json:"recovery_codes"`
	MFA           MFAStatus `json:"mfa"`
}

// WithAdminMFA configures administrator MFA: box seals the TOTP secrets (nil
// makes every MFA feature answer ErrMFAUnavailable) and required is
// ADMIN_MFA_REQUIRED.
func WithAdminMFA(box *secretbox.Box, required bool) Option {
	return func(s *Service) { s.mfaBox, s.mfaRequired = box, required }
}

// MFARequired reports ADMIN_MFA_REQUIRED.
func (s *Service) MFARequired() bool { return s.mfaRequired }

func mfaAAD(userID uuid.UUID) []byte { return []byte("admin-mfa-totp:" + userID.String()) }

// SealTOTPSecret seals a raw secret for userID (used by the development seed).
func SealTOTPSecret(box *secretbox.Box, userID uuid.UUID, secret []byte) ([]byte, error) {
	return box.Seal(secret, mfaAAD(userID))
}

func (s *Service) openSecret(rec AdminMFA) ([]byte, error) {
	if s.mfaBox == nil {
		return nil, ErrMFAUnavailable
	}
	secret, err := s.mfaBox.Open(rec.SecretSealed, mfaAAD(rec.UserID))
	if err != nil {
		// A changed MFA_ENCRYPTION_KEY: the authenticator cannot be used.
		return nil, fmt.Errorf("open totp secret: %w", ErrMFAUnavailable)
	}
	return secret, nil
}

// enrolled loads a CONFIRMED authenticator, or ErrMFANotEnrolled.
func (s *Service) enrolled(ctx context.Context, userID uuid.UUID) (AdminMFA, error) {
	rec, err := s.store.AdminMFA(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return AdminMFA{}, ErrMFANotEnrolled
	case err != nil:
		return AdminMFA{}, fmt.Errorf("load admin mfa: %w", err)
	case rec.ConfirmedAt == nil:
		return AdminMFA{}, ErrMFANotEnrolled
	}
	return rec, nil
}

// adminEnrolled reports whether the user has a confirmed authenticator.
func (s *Service) adminEnrolled(ctx context.Context, userID uuid.UUID) (bool, error) {
	_, err := s.enrolled(ctx, userID)
	if errors.Is(err, ErrMFANotEnrolled) {
		return false, nil
	}
	return err == nil, err
}

// newRecoveryCodes returns RecoveryCodeCount display codes and their hashes.
func (s *Service) newRecoveryCodes() ([]string, []string, error) {
	codes := make([]string, 0, RecoveryCodeCount)
	hashes := make([]string, 0, RecoveryCodeCount)
	for range RecoveryCodeCount {
		var b strings.Builder
		for range recoveryCodeLength {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(recoveryAlphabet))))
			if err != nil {
				return nil, nil, fmt.Errorf("generate recovery code: %w", err)
			}
			b.WriteByte(recoveryAlphabet[n.Int64()])
		}
		raw := b.String()
		h, err := s.hasher.Hash(raw)
		if err != nil {
			return nil, nil, err
		}
		codes = append(codes, raw[:recoveryCodeDisplayDashAt]+"-"+raw[recoveryCodeDisplayDashAt:])
		hashes = append(hashes, h)
	}
	return codes, hashes, nil
}

// normalizeRecoveryCode lower-cases and drops spaces and dashes; "" when it
// cannot be a recovery code.
func normalizeRecoveryCode(raw string) string {
	out := strings.ToLower(strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(strings.TrimSpace(raw)))
	if len(out) != recoveryCodeLength {
		return ""
	}
	for i := 0; i < len(out); i++ {
		if !strings.ContainsRune(recoveryAlphabet, rune(out[i])) {
			return ""
		}
	}
	return out
}

func (s *Service) mfaAudit(actor User, action, reason string, before, after map[string]any, target uuid.UUID, now time.Time) adminaudit.Event {
	return adminaudit.New(actor.ID, actor.Name, action, adminaudit.TargetUser, target.String(), "", reason, before, after, now)
}

// MFAStatusOf is the MFA status of an account.
func (s *Service) MFAStatusOf(ctx context.Context, userID uuid.UUID) (MFAStatus, error) {
	st := MFAStatus{Required: s.mfaRequired}
	rec, err := s.store.AdminMFA(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return st, nil
	}
	if err != nil {
		return MFAStatus{}, fmt.Errorf("load admin mfa: %w", err)
	}
	if rec.ConfirmedAt == nil {
		st.PendingSetup = s.clock().Before(rec.CreatedAt.Add(MFASetupTTL))
		return st, nil
	}
	codes, err := s.store.UnusedRecoveryCodes(ctx, userID)
	if err != nil {
		return MFAStatus{}, fmt.Errorf("load recovery codes: %w", err)
	}
	method := MFAMethodTOTP
	at := rec.ConfirmedAt.UTC()
	st.Enrolled, st.Method, st.CreatedAt, st.RecoveryCodesRemaining = true, &method, &at, len(codes)
	return st, nil
}

// adminSession returns the signed-in administrator, or ErrMFAForbidden.
func (s *Service) adminSession(ctx context.Context, p *auth.Principal) (User, error) {
	if p == nil || p.SessionScope != auth.ScopeAdmin || len(p.PlatformRoles) == 0 {
		return User{}, ErrMFAForbidden
	}
	u, err := s.store.UserByID(ctx, p.UserID)
	if err != nil {
		return User{}, fmt.Errorf("load user: %w", err)
	}
	return u, nil
}

// SetupTOTP starts (or restarts) the enrolment of the signed-in
// administrator: a new secret is sealed and stored as pending.
func (s *Service) SetupTOTP(ctx context.Context, p *auth.Principal) (TOTPSetup, error) {
	user, err := s.adminSession(ctx, p)
	if err != nil {
		return TOTPSetup{}, err
	}
	if s.mfaBox == nil {
		return TOTPSetup{}, ErrMFAUnavailable
	}
	if ok, err := s.adminEnrolled(ctx, user.ID); err != nil {
		return TOTPSetup{}, err
	} else if ok {
		return TOTPSetup{}, ErrMFAAlreadyEnrolled
	}
	secret, err := totp.NewSecret()
	if err != nil {
		return TOTPSetup{}, err
	}
	sealed, err := SealTOTPSecret(s.mfaBox, user.ID, secret)
	if err != nil {
		return TOTPSetup{}, err
	}
	now := s.clock()
	if err := s.store.PutPendingTOTP(ctx, user.ID, sealed, now); err != nil {
		if errors.Is(err, ErrMFAAlreadyEnrolled) {
			return TOTPSetup{}, err
		}
		return TOTPSetup{}, fmt.Errorf("store pending totp: %w", err)
	}
	s.audit(ctx, slog.LevelInfo, auditEventAdminMFA, "setup_started", slog.String("user_id", user.ID.String()))
	return TOTPSetup{
		SecretBase32: totp.EncodeSecret(secret), OTPAuthURI: totp.URI(MFAIssuer, user.Email, secret),
		Issuer: MFAIssuer, AccountName: user.Email, Algorithm: totp.Algorithm, Digits: totp.Digits,
		Period: int(totp.Period / time.Second), ExpiresAt: now.Add(MFASetupTTL),
	}, nil
}

func totpInputProblem(code string) bool { return len(code) != totp.Digits || !allDigits(code) }

// ConfirmTOTP activates the pending authenticator with a code from it and
// returns the recovery codes (shown once).
func (s *Service) ConfirmTOTP(ctx context.Context, p *auth.Principal, code string) (RecoveryCodesResult, error) {
	user, err := s.adminSession(ctx, p)
	if err != nil {
		return RecoveryCodesResult{}, err
	}
	if totpInputProblem(code) {
		return RecoveryCodesResult{}, &ValidationError{Fields: map[string]string{"code": msgTOTPRequired}}
	}
	if s.mfaBox == nil {
		return RecoveryCodesResult{}, ErrMFAUnavailable
	}
	now := s.clock()
	rec, err := s.store.AdminMFA(ctx, user.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return RecoveryCodesResult{}, ErrMFASetupRequired
	case err != nil:
		return RecoveryCodesResult{}, fmt.Errorf("load admin mfa: %w", err)
	case rec.ConfirmedAt != nil:
		return RecoveryCodesResult{}, ErrMFAAlreadyEnrolled
	case !now.Before(rec.CreatedAt.Add(MFASetupTTL)):
		return RecoveryCodesResult{}, ErrMFASetupRequired
	}
	secret, err := s.openSecret(rec)
	if err != nil {
		return RecoveryCodesResult{}, err
	}
	step, ok := totp.Match(secret, code, now)
	if !ok {
		s.audit(ctx, slog.LevelWarn, auditEventAdminMFA, outcomeFailure, slog.String("user_id", user.ID.String()), slog.String("action", "confirm"))
		return RecoveryCodesResult{}, &ValidationError{Fields: map[string]string{"code": msgCodeWrong}}
	}
	codes, hashes, err := s.newRecoveryCodes()
	if err != nil {
		return RecoveryCodesResult{}, err
	}
	audit := s.mfaAudit(user, adminaudit.ActionAdminMFAEnrolled, "", nil,
		map[string]any{"method": MFAMethodTOTP, "recovery_codes": RecoveryCodeCount}, user.ID, now)
	err = s.store.ConfirmTOTP(ctx, user.ID, step, hashes, now.Add(-MFASetupTTL), now, audit)
	if errors.Is(err, ErrNotFound) {
		return RecoveryCodesResult{}, ErrMFASetupRequired
	}
	if err != nil {
		return RecoveryCodesResult{}, fmt.Errorf("confirm totp: %w", err)
	}
	s.audit(ctx, slog.LevelInfo, auditEventAdminMFA, "enrolled", slog.String("user_id", user.ID.String()))
	st, err := s.MFAStatusOf(ctx, user.ID)
	if err != nil {
		return RecoveryCodesResult{}, err
	}
	return RecoveryCodesResult{RecoveryCodes: codes, MFA: st}, nil
}

// checkCurrentTOTP verifies a code of the user's confirmed authenticator and
// records its step (replay protection).
func (s *Service) checkCurrentTOTP(ctx context.Context, user User, code, action string) error {
	if totpInputProblem(code) {
		return &ValidationError{Fields: map[string]string{"code": msgTOTPRequired}}
	}
	rec, err := s.enrolled(ctx, user.ID)
	if err != nil {
		return err
	}
	secret, err := s.openSecret(rec)
	if err != nil {
		return err
	}
	now := s.clock()
	step, ok := totp.Match(secret, code, now)
	if !ok {
		s.audit(ctx, slog.LevelWarn, auditEventAdminMFA, outcomeFailure, slog.String("user_id", user.ID.String()), slog.String("action", action))
		return &ValidationError{Fields: map[string]string{"code": msgCodeWrong}}
	}
	err = s.store.UseTOTPStep(ctx, user.ID, step, now)
	switch {
	case errors.Is(err, ErrMFACodeReused):
		s.audit(ctx, slog.LevelWarn, auditEventAdminMFA, outcomeFailure, slog.String("user_id", user.ID.String()),
			slog.String("action", action), slog.String("reason", "replay"))
		return &ValidationError{Fields: map[string]string{"code": msgTOTPReused}}
	case errors.Is(err, ErrNotFound):
		return ErrMFANotEnrolled
	case err != nil:
		return fmt.Errorf("record totp step: %w", err)
	}
	return nil
}

// RegenerateRecoveryCodes replaces the recovery codes (current TOTP code required).
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, p *auth.Principal, code string) (RecoveryCodesResult, error) {
	user, err := s.adminSession(ctx, p)
	if err != nil {
		return RecoveryCodesResult{}, err
	}
	if err := s.checkCurrentTOTP(ctx, user, code, "regenerate"); err != nil {
		return RecoveryCodesResult{}, err
	}
	codes, hashes, err := s.newRecoveryCodes()
	if err != nil {
		return RecoveryCodesResult{}, err
	}
	now := s.clock()
	audit := s.mfaAudit(user, adminaudit.ActionAdminMFARecoveryRegenerated, "", nil, map[string]any{"recovery_codes": RecoveryCodeCount}, user.ID, now)
	if err := s.store.ReplaceRecoveryCodes(ctx, user.ID, hashes, now, audit); err != nil {
		if errors.Is(err, ErrNotFound) {
			return RecoveryCodesResult{}, ErrMFANotEnrolled
		}
		return RecoveryCodesResult{}, fmt.Errorf("replace recovery codes: %w", err)
	}
	s.audit(ctx, slog.LevelInfo, auditEventAdminMFA, "recovery_codes_regenerated", slog.String("user_id", user.ID.String()))
	st, err := s.MFAStatusOf(ctx, user.ID)
	if err != nil {
		return RecoveryCodesResult{}, err
	}
	return RecoveryCodesResult{RecoveryCodes: codes, MFA: st}, nil
}

// RemoveTOTP removes the caller's own authenticator (current TOTP code
// required). Refused while ADMIN_MFA_REQUIRED is on.
func (s *Service) RemoveTOTP(ctx context.Context, p *auth.Principal, code string) error {
	user, err := s.adminSession(ctx, p)
	if err != nil {
		return err
	}
	if s.mfaRequired {
		return ErrMFARemovalNotAllowed
	}
	if err := s.checkCurrentTOTP(ctx, user, code, "remove"); err != nil {
		return err
	}
	now := s.clock()
	audit := s.mfaAudit(user, adminaudit.ActionAdminMFARemoved, "", map[string]any{"method": MFAMethodTOTP}, nil, user.ID, now)
	if err := s.store.DeleteAdminMFA(ctx, user.ID, now, audit, false); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrMFANotEnrolled
		}
		return fmt.Errorf("delete admin mfa: %w", err)
	}
	s.audit(ctx, slog.LevelInfo, auditEventAdminMFA, "removed", slog.String("user_id", user.ID.String()))
	return nil
}

// ResetAdminMFA is a SUPER_ADMIN removing another administrator's
// authenticator: every session of the target is revoked, the reset is
// audited with the reason and the target is emailed.
func (s *Service) ResetAdminMFA(ctx context.Context, p *auth.Principal, targetID uuid.UUID, rawReason string) (MFAStatus, error) {
	if p == nil || !p.HasPlatformRole(auth.PlatformSuperAdmin) {
		return MFAStatus{}, ErrMFAForbidden
	}
	if p.UserID == targetID {
		return MFAStatus{}, ErrMFAForbidden
	}
	reason := strings.TrimSpace(rawReason)
	if n := utf8.RuneCountInString(reason); n < minMFAResetReason || n > maxMFAResetReason {
		return MFAStatus{}, &ValidationError{Fields: map[string]string{"reason": "is required (3 to 1000 characters)"}}
	}
	actor, err := s.store.UserByID(ctx, p.UserID)
	if err != nil {
		return MFAStatus{}, fmt.Errorf("load actor: %w", err)
	}
	target, err := s.store.UserByID(ctx, targetID)
	if err != nil {
		return MFAStatus{}, err // ErrNotFound -> 404
	}
	now := s.clock()
	audit := adminaudit.New(actor.ID, actor.Name, adminaudit.ActionAdminMFAReset, adminaudit.TargetUser, target.ID.String(), target.Name,
		reason, map[string]any{"method": MFAMethodTOTP}, nil, now)
	err = s.store.DeleteAdminMFA(ctx, target.ID, now, audit, true, OutboxEvent{Type: EventAdminMFAReset, UserID: target.ID, OccurredAt: now})
	if errors.Is(err, ErrNotFound) {
		return MFAStatus{}, ErrMFANotEnrolled
	}
	if err != nil {
		return MFAStatus{}, fmt.Errorf("reset admin mfa: %w", err)
	}
	s.audit(ctx, slog.LevelWarn, auditEventAdminMFA, "reset", slog.String("user_id", target.ID.String()), slog.String("actor_user_id", actor.ID.String()))
	return s.MFAStatusOf(ctx, target.ID)
}

// AdminMFAStatusFor is the MFA status of another account (SUPER_ADMIN).
func (s *Service) AdminMFAStatusFor(ctx context.Context, p *auth.Principal, targetID uuid.UUID) (MFAStatus, error) {
	if p == nil || !p.HasPlatformRole(auth.PlatformSuperAdmin) {
		return MFAStatus{}, ErrMFAForbidden
	}
	if _, err := s.store.UserByID(ctx, targetID); err != nil {
		return MFAStatus{}, err
	}
	return s.MFAStatusOf(ctx, targetID)
}

// mfaChallengeFor answers the emailed-code step of an enrolled administrator.
func (s *Service) mfaChallengeFor(ctx context.Context, c LoginChallenge, challengeID string, now time.Time) (MFAChallenge, error) {
	expires := now.Add(SignInCodeTTL)
	if err := s.store.MarkLoginChallengeCodeVerified(ctx, c.IDHash, now, expires); err != nil {
		if errors.Is(err, ErrNotFound) {
			return MFAChallenge{}, ErrCodeExpired
		}
		return MFAChallenge{}, fmt.Errorf("mark sign-in challenge verified: %w", err)
	}
	return MFAChallenge{
		Status: statusMFARequired, ChallengeID: challengeID, Methods: []string{MFAMethodTOTP, MFAMethodRecoveryCode},
		ExpiresAt: expires, CodeLength: totp.Digits,
	}, nil
}

// VerifyAdminMFA is the second-factor step of the administrator sign-in:
// POST /v1/admin/auth/login/mfa with exactly one of code and recoveryCode.
func (s *Service) VerifyAdminMFA(ctx context.Context, challengeID, code, recoveryCode, clientIP string) (AuthResult, error) {
	code, recoveryCode = strings.TrimSpace(code), strings.TrimSpace(recoveryCode)
	switch {
	case (code == "") == (recoveryCode == ""):
		return AuthResult{}, &ValidationError{Fields: map[string]string{"code": msgMFAEnterOne}}
	case code != "" && totpInputProblem(code):
		return AuthResult{}, &ValidationError{Fields: map[string]string{"code": msgTOTPRequired}}
	}
	normalized := ""
	if recoveryCode != "" {
		if normalized = normalizeRecoveryCode(recoveryCode); normalized == "" {
			// Not something we could have issued: not counted as an attempt.
			return AuthResult{}, &ValidationError{Fields: map[string]string{"recovery_code": msgRecoveryCodeWrong}}
		}
	}
	now := s.clock()
	challengeID = strings.TrimSpace(challengeID)
	if !plausibleToken(challengeID) {
		return AuthResult{}, ErrCodeExpired
	}
	c, err := s.store.LoginChallenge(ctx, hashToken(challengeID))
	switch {
	case errors.Is(err, ErrNotFound):
		return AuthResult{}, ErrCodeExpired
	case err != nil:
		return AuthResult{}, fmt.Errorf("load sign-in challenge: %w", err)
	case c.Scope != auth.ScopeAdmin || c.CodeVerifiedAt == nil || !c.usable(now):
		return AuthResult{}, ErrCodeExpired
	}
	user, err := s.store.UserByID(ctx, c.UserID)
	if errors.Is(err, ErrNotFound) {
		return AuthResult{}, ErrCodeExpired
	}
	if err != nil {
		return AuthResult{}, fmt.Errorf("load user: %w", err)
	}
	if user.Status != StatusActive || !user.Verified() || len(user.PlatformRoles) == 0 {
		return AuthResult{}, ErrCodeExpired
	}
	rec, err := s.enrolled(ctx, user.ID)
	if errors.Is(err, ErrMFANotEnrolled) {
		// Reset since the emailed code was accepted: sign in again.
		return AuthResult{}, ErrCodeExpired
	}
	if err != nil {
		return AuthResult{}, err
	}

	method := MFAMethodTOTP
	if code != "" {
		secret, err := s.openSecret(rec)
		if err != nil {
			return AuthResult{}, err
		}
		step, ok := totp.Match(secret, code, now)
		if !ok {
			return AuthResult{}, s.failMFA(ctx, c, user, method, "wrong_code", clientIP, now, "code", msgCodeWrong)
		}
		err = s.store.UseTOTPStep(ctx, user.ID, step, now)
		switch {
		case errors.Is(err, ErrMFACodeReused):
			return AuthResult{}, s.failMFA(ctx, c, user, method, "replay", clientIP, now, "code", msgTOTPReused)
		case errors.Is(err, ErrNotFound):
			return AuthResult{}, ErrCodeExpired
		case err != nil:
			return AuthResult{}, fmt.Errorf("record totp step: %w", err)
		}
	} else {
		method = MFAMethodRecoveryCode
		codes, err := s.store.UnusedRecoveryCodes(ctx, user.ID)
		if err != nil {
			return AuthResult{}, fmt.Errorf("load recovery codes: %w", err)
		}
		var matched *RecoveryCode
		for i := range codes {
			ok, err := VerifyPassword(codes[i].Hash, normalized)
			if err != nil {
				return AuthResult{}, fmt.Errorf("verify recovery code: %w", err)
			}
			if ok && matched == nil {
				matched = &codes[i]
			}
		}
		if matched == nil {
			return AuthResult{}, s.failMFA(ctx, c, user, method, "wrong_code", clientIP, now, "recovery_code", msgRecoveryCodeWrong)
		}
		remaining := len(codes) - 1
		audit := s.mfaAudit(user, adminaudit.ActionAdminMFARecoveryCodeUsed, "", nil,
			map[string]any{"recovery_codes_remaining": remaining}, user.ID, now)
		err = s.store.UseRecoveryCode(ctx, user.ID, matched.ID, now, audit,
			OutboxEvent{Type: EventAdminMFARecoveryCodeUsed, UserID: user.ID, OccurredAt: now})
		if errors.Is(err, ErrNotFound) {
			return AuthResult{}, s.failMFA(ctx, c, user, method, "used_code", clientIP, now, "recovery_code", msgRecoveryCodeWrong)
		}
		if err != nil {
			return AuthResult{}, fmt.Errorf("use recovery code: %w", err)
		}
	}

	if err := s.store.ConsumeLoginChallenge(ctx, c.IDHash, now); err != nil {
		if errors.Is(err, ErrNotFound) {
			return AuthResult{}, ErrCodeExpired
		}
		return AuthResult{}, fmt.Errorf("consume sign-in challenge: %w", err)
	}
	res, err := s.startSession(ctx, user, DeliveryCookie, auth.ScopeAdmin, now)
	if err != nil {
		return AuthResult{}, err
	}
	s.audit(ctx, slog.LevelInfo, "admin_sign_in", "success", slog.String("user_id", user.ID.String()), slog.String("ip", clientIP),
		slog.String("mfa_method", method), slog.String("email_sha256", emailFingerprint(user.Email)))
	return res, nil
}

// failMFA counts a wrong second-factor answer. At SignInCodeMaxAttempts the
// challenge is dead and one durable audit record says so (failures are
// aggregated there; each one is in the log audit).
func (s *Service) failMFA(ctx context.Context, c LoginChallenge, user User, method, reason, clientIP string, now time.Time, field, msg string) error {
	attempts, err := s.store.FailLoginChallengeMFA(ctx, c.IDHash, now)
	if errors.Is(err, ErrNotFound) {
		return ErrCodeExpired
	}
	if err != nil {
		return fmt.Errorf("record wrong mfa code: %w", err)
	}
	s.audit(ctx, slog.LevelWarn, auditEventAdminMFASignIn, outcomeFailure, slog.String("user_id", user.ID.String()),
		slog.String("method", method), slog.String("reason", reason), slog.Int("attempts", attempts), slog.String("ip", clientIP))
	if attempts >= SignInCodeMaxAttempts {
		e := s.mfaAudit(user, adminaudit.ActionAdminMFAChallengeLocked, "", nil,
			map[string]any{"failed_attempts": attempts, "last_method": method}, user.ID, now)
		if err := s.store.RecordAdminAudit(ctx, e); err != nil {
			s.logger.ErrorContext(ctx, "could not record mfa lockout", slog.String("error", err.Error()))
		}
		return ErrCodeExpired
	}
	return &ValidationError{Fields: map[string]string{field: msg}}
}
