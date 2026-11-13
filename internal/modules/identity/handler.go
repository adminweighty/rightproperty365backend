package identity

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

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

// Handler serves /v1/auth/* and /v1/me.
type Handler struct {
	svc          *Service
	logger       *slog.Logger
	secureCookie bool
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
	ExpiresAt time.Time `json:"expires_at"`
}

type authResponse struct {
	User    UserView    `json:"user"`
	Session sessionView `json:"session"`
	// AccessToken is present only for token_delivery "bearer".
	AccessToken string `json:"access_token,omitempty"`
}

type meResponse struct {
	User UserView `json:"user"`
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
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(SessionAbsoluteTTL / time.Second),
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
	body := authResponse{User: res.User.View(), Session: sessionView{ExpiresAt: res.ExpiresAt.UTC()}}
	if res.Delivery == DeliveryBearer {
		body.AccessToken = res.Token
	} else {
		h.setSessionCookie(w, res.Token, res.ExpiresAt)
	}
	httpx.WriteJSON(w, http.StatusOK, body)
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

// Login handles POST /v1/auth/login.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email         string `json:"email"`
		Password      string `json:"password"`
		TokenDelivery string `json:"token_delivery"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.Login(r.Context(), in.Email, in.Password, in.TokenDelivery)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeAuth(w, res)
}

// Logout handles POST /v1/auth/logout: revokes the current session (cookie or
// bearer), clears the cookie and answers 204 even when nobody is signed in.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if p := auth.FromContext(r.Context()); p != nil {
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

// Me handles GET /v1/me (behind auth.RequireAuth).
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
	httpx.WriteJSON(w, http.StatusOK, meResponse{User: user.View()})
}
