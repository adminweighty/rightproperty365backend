package identity

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// writeMFAError answers the administrator MFA errors; false when err is not one.
func writeMFAError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case errors.Is(err, ErrMFAUnavailable):
		httpx.WriteError(w, r, http.StatusServiceUnavailable, CodeMFAUnavailable,
			"Two-step sign-in with an authenticator app is not available right now. Contact the platform owner.", nil)
	case errors.Is(err, ErrMFAAlreadyEnrolled):
		httpx.WriteError(w, r, http.StatusConflict, CodeMFAAlreadyEnrolled, "An authenticator app is already set up for this account.", nil)
	case errors.Is(err, ErrMFANotEnrolled):
		httpx.WriteError(w, r, http.StatusConflict, CodeMFANotEnrolled, "No authenticator app is set up for this account.", nil)
	case errors.Is(err, ErrMFASetupRequired):
		httpx.WriteError(w, r, http.StatusConflict, CodeMFASetupRequired, "Start the authenticator app setup again.", nil)
	case errors.Is(err, ErrMFARemovalNotAllowed):
		httpx.WriteError(w, r, http.StatusForbidden, CodeMFARemovalNotAllowed,
			"Two-step sign-in is required for administrators. Ask a super administrator to reset it.", nil)
	case errors.Is(err, ErrMFAForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You do not have permission to do this.", nil)
	default:
		return false
	}
	return true
}

// mfaCodeInput is the body of the routes that take a current TOTP code.
type mfaCodeInput struct {
	Code json.RawMessage `json:"code"`
}

// AdminLoginMFA handles POST /v1/admin/auth/login/mfa: the second factor of
// an administrator with an authenticator, on the challenge the emailed-code
// step answered MFA_REQUIRED for. Exactly one of code and recovery_code.
func (h *Handler) AdminLoginMFA(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChallengeID  string          `json:"challenge_id"`
		Code         json.RawMessage `json:"code"`
		RecoveryCode string          `json:"recovery_code"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.VerifyAdminMFA(r.Context(), in.ChallengeID, ParseCodeInput(in.Code), in.RecoveryCode, httpx.ClientIP(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeAdminSession(w, res)
}

func writeNoStore(w http.ResponseWriter, v any) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, v)
}

// MFAStatus handles GET /v1/admin/auth/mfa.
func (h *Handler) MFAStatus(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	st, err := h.svc.MFAStatusOf(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoStore(w, st)
}

// SetupTOTP handles POST /v1/admin/auth/mfa/totp/setup (no body needed).
func (h *Handler) SetupTOTP(w http.ResponseWriter, r *http.Request) {
	setup, err := h.svc.SetupTOTP(r.Context(), auth.FromContext(r.Context()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoStore(w, setup)
}

// ConfirmTOTP handles POST /v1/admin/auth/mfa/totp/confirm {code}.
func (h *Handler) ConfirmTOTP(w http.ResponseWriter, r *http.Request) {
	var in mfaCodeInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.ConfirmTOTP(r.Context(), auth.FromContext(r.Context()), ParseCodeInput(in.Code))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoStore(w, res)
}

// refuseEnrolmentOnly answers 403 MFA_ENROLMENT_REQUIRED to an enrolment-only
// session on the routes that need an authenticator already.
func refuseEnrolmentOnly(w http.ResponseWriter, r *http.Request) bool {
	if p := auth.FromContext(r.Context()); p != nil && p.MFAEnrolmentOnly {
		httpx.WriteError(w, r, http.StatusForbidden, CodeMFAEnrolmentRequired,
			"Set up two-step sign-in with an authenticator app to continue.", nil)
		return true
	}
	return false
}

// RegenerateRecoveryCodes handles POST /v1/admin/auth/mfa/recovery-codes/regenerate {code}.
func (h *Handler) RegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	if refuseEnrolmentOnly(w, r) {
		return
	}
	var in mfaCodeInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.RegenerateRecoveryCodes(r.Context(), auth.FromContext(r.Context()), ParseCodeInput(in.Code))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoStore(w, res)
}

// RemoveTOTP handles DELETE /v1/admin/auth/mfa/totp {code}.
func (h *Handler) RemoveTOTP(w http.ResponseWriter, r *http.Request) {
	if refuseEnrolmentOnly(w, r) {
		return
	}
	var in mfaCodeInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.RemoveTOTP(r.Context(), auth.FromContext(r.Context()), ParseCodeInput(in.Code)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) targetUser(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "That account does not exist.", nil)
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) failTarget(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "That account does not exist.", nil)
		return
	}
	h.fail(w, r, err)
}

// ResetUserMFA handles POST /v1/admin/users/{id}/mfa/reset {reason} (SUPER_ADMIN).
func (h *Handler) ResetUserMFA(w http.ResponseWriter, r *http.Request) {
	id, ok := h.targetUser(w, r)
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	st, err := h.svc.ResetAdminMFA(r.Context(), auth.FromContext(r.Context()), id, in.Reason)
	if err != nil {
		h.failTarget(w, r, err)
		return
	}
	writeNoStore(w, st)
}

// UserMFAStatus handles GET /v1/admin/users/{id}/mfa (SUPER_ADMIN).
func (h *Handler) UserMFAStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := h.targetUser(w, r)
	if !ok {
		return
	}
	st, err := h.svc.AdminMFAStatusFor(r.Context(), auth.FromContext(r.Context()), id)
	if err != nil {
		h.failTarget(w, r, err)
		return
	}
	writeNoStore(w, st)
}
