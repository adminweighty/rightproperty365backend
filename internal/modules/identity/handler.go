package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Error codes specific to this module.
const (
	CodeInvalidCredentials = "INVALID_CREDENTIALS"
	CodeEmailNotVerified   = "EMAIL_NOT_VERIFIED"
	CodeInvalidToken       = "INVALID_TOKEN"
)

// Fixed response statuses for the enumeration-safe endpoints.
const (
	statusVerificationSent = "VERIFICATION_SENT"
	statusResetSent        = "RESET_SENT"
)

// Handler serves /v1/auth/*, /v1/admin/auth/login, /v1/me and /v1/admin/me.
type Handler struct {
	svc          *Service
	logger       *slog.Logger
	secureCookie bool
	// onLogout runs before a session is revoked: it removes the push
	// devices the session registered and the given device token (FR-045).
	onLogout func(ctx context.Context, userID, sessionID uuid.UUID, deviceToken string) error
}

// SetLogoutHook sets what runs before a signed-in session is revoked.
func (h *Handler) SetLogoutHook(hook func(ctx context.Context, userID, sessionID uuid.UUID, deviceToken string) error) {
	h.onLogout = hook
}

// logoutDeviceToken reads the optional body {"device_token": "..."} of a
// logout. An empty or unreadable body is no token: signing out never fails
// because of its body.
func logoutDeviceToken(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil || strings.TrimSpace(string(raw)) == "" {
		return ""
	}
	var in struct {
		DeviceToken string `json:"device_token"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	return strings.TrimSpace(in.DeviceToken)
}

// NewHandler builds the handler. secureCookie must be true everywhere except
// local development over plain HTTP.
func NewHandler(svc *Service, logger *slog.Logger, secureCookie bool) *Handler {
	return &Handler{svc: svc, logger: logger, secureCookie: secureCookie}
}

type statusResponse struct {
	Status string `json:"status"`
}

type sessionView struct {
	Scope     string    `json:"scope"`
	ExpiresAt time.Time `json:"expires_at"`
}

type authResponse struct {
	User UserView `json:"user"`
	// Session is absent only when email verification deliberately did not
	// sign the caller in (platform-role accounts).
	Session *sessionView `json:"session,omitempty"`
	// AccessToken is present only for token_delivery "bearer".
	AccessToken string `json:"access_token,omitempty"`
}

type meResponse struct {
	User    UserView    `json:"user"`
	Session sessionView `json:"session"`
}

// fail maps service errors to responses.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrInvalidCredentials):
		httpx.WriteError(w, r, http.StatusUnauthorized, CodeInvalidCredentials, "The email or password is incorrect.", nil)
	case errors.Is(err, ErrEmailNotVerified):
		httpx.WriteError(w, r, http.StatusForbidden, CodeEmailNotVerified, "Please verify your email address before signing in.", nil)
	case errors.Is(err, ErrInvalidToken):
		httpx.WriteError(w, r, http.StatusBadRequest, CodeInvalidToken, "This link is invalid, has expired or has already been used.", nil)
	case errors.Is(err, ErrCodeExpired):
		httpx.WriteError(w, r, http.StatusBadRequest, CodeCodeExpired, msgCodeExpired, nil)
	default:
		var later *ResendLaterError
		if errors.As(err, &later) {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(later.After.Seconds())))))
			httpx.WriteError(w, r, http.StatusTooManyRequests, httpx.CodeRateLimited, later.Message(), nil)
			return
		}
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(ttl / time.Second),
		HttpOnly: true,
		Secure:   h.secureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

// writeAuth answers a successful login/verification: the token goes either
// into the HttpOnly cookie or into the body, never both.
func (h *Handler) writeAuth(w http.ResponseWriter, res AuthResult) {
	body := authResponse{User: res.User.View()}
	if !res.NoSession {
		body.Session = &sessionView{Scope: res.Scope, ExpiresAt: res.ExpiresAt.UTC()}
		if res.Delivery == DeliveryBearer {
			body.AccessToken = res.Token
		} else {
			h.setSessionCookie(w, res.Token, res.ExpiresAt, res.TTL)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

// Captcha handles GET /v1/auth/captcha: a new question for the registration
// form. Never cached: every call is a different, single-use question.
func (h *Handler) Captcha(w http.ResponseWriter, r *http.Request) {
	challenge, err := h.svc.NewCaptcha(r.Context())
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, challenge)
}

// Register handles POST /v1/auth/register. Valid input always yields 202.
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var in RegisterInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.Register(r.Context(), in); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, statusResponse{Status: statusVerificationSent})
}

// VerifyEmail handles POST /v1/auth/verify-email.
func (h *Handler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token         string `json:"token"`
		TokenDelivery string `json:"token_delivery"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.VerifyEmail(r.Context(), in.Token, in.TokenDelivery)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeAuth(w, res)
}

// ResendVerification handles POST /v1/auth/resend-verification.
func (h *Handler) ResendVerification(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.ResendVerification(r.Context(), in.Email); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, statusResponse{Status: statusVerificationSent})
}

// writeChallenge answers the password step: 200 CODE_REQUIRED, no session.
func (h *Handler) writeChallenge(w http.ResponseWriter, c CodeChallenge) {
	c.ExpiresAt = c.ExpiresAt.UTC()
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, c)
}

// Login handles POST /v1/auth/login: the password step. When the password is
// right the answer is 200 CODE_REQUIRED and a code is emailed; the session is
// started by LoginVerify.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email         string `json:"email"`
		Password      string `json:"password"`
		TokenDelivery string `json:"token_delivery"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	c, err := h.svc.Login(r.Context(), in.Email, in.Password, in.TokenDelivery)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeChallenge(w, c)
}

// AdminLogin handles POST /v1/admin/auth/login. The body is {email, password}
// only: strict JSON decoding rejects token_delivery (400 INVALID_JSON). When
// the password is right the answer is 200 CODE_REQUIRED; the session is
// started by AdminLoginVerify and never carries access_token.
func (h *Handler) AdminLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	c, err := h.svc.AdminLogin(r.Context(), in.Email, in.Password, httpx.ClientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeChallenge(w, c)
}

// verifyInput is the body of POST .../login/verify. The code may be sent as
// a string (preferred) or a number.
type verifyInput struct {
	ChallengeID string          `json:"challenge_id"`
	Code        json.RawMessage `json:"code"`
}

// LoginVerify handles POST /v1/auth/login/verify: on the right code, exactly
// what the login used to answer (cookie set, or access_token for bearer).
func (h *Handler) LoginVerify(w http.ResponseWriter, r *http.Request) {
	var in verifyInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.VerifyCode(r.Context(), auth.ScopeCustomer, in.ChallengeID, ParseCodeInput(in.Code), httpx.ClientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeAuth(w, res)
}

// AdminLoginVerify handles POST /v1/admin/auth/login/verify: on the right
// code, an admin-scope cookie session.
func (h *Handler) AdminLoginVerify(w http.ResponseWriter, r *http.Request) {
	var in verifyInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.VerifyCode(r.Context(), auth.ScopeAdmin, in.ChallengeID, ParseCodeInput(in.Code), httpx.ClientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.setSessionCookie(w, res.Token, res.ExpiresAt, res.TTL)
	httpx.WriteJSON(w, http.StatusOK, authResponse{
		User:    res.User.View(),
		Session: &sessionView{Scope: res.Scope, ExpiresAt: res.ExpiresAt.UTC()},
	})
}

func (h *Handler) resend(scope string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ChallengeID string `json:"challenge_id"`
		}
		if !httpx.DecodeJSON(w, r, &in) {
			return
		}
		c, err := h.svc.ResendCode(r.Context(), scope, in.ChallengeID)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		h.writeChallenge(w, c)
	}
}

// LoginResend handles POST /v1/auth/login/resend.
func (h *Handler) LoginResend(w http.ResponseWriter, r *http.Request) {
	h.resend(auth.ScopeCustomer)(w, r)
}

// AdminLoginResend handles POST /v1/admin/auth/login/resend.
func (h *Handler) AdminLoginResend(w http.ResponseWriter, r *http.Request) {
	h.resend(auth.ScopeAdmin)(w, r)
}

// Logout handles POST /v1/auth/logout: revokes the current session (cookie or
// bearer), clears the cookie and answers 204 even when nobody is signed in.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if p := auth.FromContext(r.Context()); p != nil {
		if h.onLogout != nil {
			// The devices go first: a session that is gone could not remove
			// them on a retry.
			if err := h.onLogout(r.Context(), p.UserID, p.SessionID, logoutDeviceToken(r)); err != nil {
				httpx.WriteInternalError(w, r, h.logger, err)
				return
			}
		}
		if err := h.svc.Logout(r.Context(), p.SessionID); err != nil {
			httpx.WriteInternalError(w, r, h.logger, err)
			return
		}
	}
	h.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// RequestPasswordReset handles POST /v1/auth/password-reset/request.
func (h *Handler) RequestPasswordReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.RequestPasswordReset(r.Context(), in.Email); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, statusResponse{Status: statusResetSent})
}

// ConfirmPasswordReset handles POST /v1/auth/password-reset/confirm.
func (h *Handler) ConfirmPasswordReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.ConfirmPasswordReset(r.Context(), in.Token, in.Password); err != nil {
		h.fail(w, r, err)
		return
	}
	// Every session of the user is now revoked, including this browser's.
	h.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// Me handles GET /v1/me (behind auth.RequireAuth) and GET /v1/admin/me
// (additionally behind auth.RequirePlatformRole).
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
		return
	}
	user, err := h.svc.User(r.Context(), p.UserID)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, meResponse{
		User:    user.View(),
		Session: sessionView{Scope: p.SessionScope, ExpiresAt: p.SessionExpiresAt.UTC()},
	})
}
