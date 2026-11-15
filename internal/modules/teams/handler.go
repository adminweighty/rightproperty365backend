package teams

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Handler serves the team endpoints.
type Handler struct {
	svc        *Service
	onboarding *onboarding.Service
	logger     *slog.Logger
}

// NewHandler builds the handler. The join requests are onboarding
// applications, decided through the onboarding service.
func NewHandler(svc *Service, onboard *onboarding.Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, onboarding: onboard, logger: logger}
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	var overr *onboarding.ValidationError
	type answer struct {
		status        int
		code, message string
	}
	answers := []struct {
		err error
		answer
	}{
		{ErrOrganisationNotFound, answer{http.StatusNotFound, CodeOrganisationNotFound, "This organisation does not exist."}},
		{ErrForbidden, answer{http.StatusForbidden, httpx.CodeForbidden, "Only the organisation's owner and managers can manage its team."}},
		{ErrMemberNotFound, answer{http.StatusNotFound, CodeMemberNotFound, "This person is not a member of the organisation."}},
		{ErrRoleNotAllowed, answer{http.StatusForbidden, CodeRoleNotAllowed, "Your role cannot give that role or act on members who have it. Only an owner can make someone a manager, and nobody can make someone an owner."}},
		{ErrOwnerProtected, answer{http.StatusForbidden, CodeOwnerProtected, "The organisation's owner cannot be changed, suspended or removed."}},
		{ErrSelfChange, answer{http.StatusForbidden, CodeSelfChangeForbidden, "You cannot change, suspend or remove your own membership."}},
		{ErrLastOwner, answer{http.StatusConflict, CodeLastOwner, "The organisation's last owner can never be removed or changed."}},
		{ErrAlreadyMember, answer{http.StatusConflict, CodeAlreadyAMember, "This person already belongs to the organisation. To give a suspended member access again, reactivate them."}},
		{ErrInvitationNotFound, answer{http.StatusNotFound, CodeInvitationNotFound, "This invitation does not exist."}},
		{ErrInvitationExpired, answer{http.StatusGone, CodeInvitationExpired, "This invitation has expired. Ask for a new one."}},
		{ErrInvitationUsed, answer{http.StatusGone, CodeInvitationNoLongerValid, "This invitation was already accepted, cancelled or replaced by a newer one."}},
		{ErrEmailMismatch, answer{http.StatusForbidden, CodeInvitationEmailMismatch, "This invitation was sent to a different email address. Sign in with the account of the invited address."}},
		{ErrSeekerAccount, answer{http.StatusConflict, CodeSeekerAccount, "This is a property seeker account, and a property seeker account cannot also have a professional role. Sign up separately with a professional account to join the organisation."}},
		{ErrAdminSession, answer{http.StatusForbidden, httpx.CodeForbidden, "An administrator session cannot join an organisation."}},
		{onboarding.ErrJoinRequestNotFound, answer{http.StatusNotFound, CodeJoinRequestNotFound, "This join request does not exist."}},
		{onboarding.ErrInvalidState, answer{http.StatusConflict, CodeJoinRequestDecided, "This join request was already decided (by your organisation or by the verification team)."}},
		{onboarding.ErrForbidden, answer{http.StatusForbidden, CodeRoleNotAllowed, "Your role cannot decide this join request. Only an owner can approve or decline someone who asks to be a manager."}},
		{onboarding.ErrAgencyNotVerified, answer{http.StatusConflict, CodeAgencyNotVerified, "The agency is no longer a verified agency."}},
	}
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
		return
	case errors.As(err, &overr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", overr.Fields)
		return
	}
	for _, a := range answers {
		if errors.Is(err, a.err) {
			httpx.WriteError(w, r, a.status, a.code, a.message, nil)
			return
		}
	}
	httpx.WriteInternalError(w, r, h.logger, err)
}

func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

// team reads the caller and the organisation ID of the path.
func (h *Handler) team(w http.ResponseWriter, r *http.Request) (*auth.Principal, uuid.UUID, bool) {
	p := principal(w, r)
	if p == nil {
		return nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, ErrOrganisationNotFound)
		return nil, uuid.Nil, false
	}
	if _, err := Team(p, id); err != nil {
		h.fail(w, r, err)
		return nil, uuid.Nil, false
	}
	return p, id, true
}

func pathUUID(r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	return id, err == nil
}

type membersResponse struct {
	Organisation struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
		Kind string    `json:"kind"`
	} `json:"organisation"`
	Items []MemberView `json:"items"`
}

// Members serves GET /v1/agent/organisations/{id}/members.
func (h *Handler) Members(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.team(w, r)
	if !ok {
		return
	}
	team, members, err := h.svc.Members(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var out membersResponse
	out.Organisation.ID, out.Organisation.Name, out.Organisation.Kind = id, team.OrganisationName, team.OrganisationKind
	out.Items = members
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// PatchMember serves PATCH /v1/agent/organisations/{id}/members/{userId}.
func (h *Handler) PatchMember(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.team(w, r)
	if !ok {
		return
	}
	userID, ok := pathUUID(r, "userId")
	if !ok {
		h.fail(w, r, ErrMemberNotFound)
		return
	}
	var in MemberPatch
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	member, err := h.svc.ChangeMember(r.Context(), p, id, userID, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"member": member})
}

// RemoveMember serves DELETE /v1/agent/organisations/{id}/members/{userId}.
func (h *Handler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.team(w, r)
	if !ok {
		return
	}
	userID, ok := pathUUID(r, "userId")
	if !ok {
		h.fail(w, r, ErrMemberNotFound)
		return
	}
	if _, err := h.svc.RemoveMember(r.Context(), p, id, userID); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Invitations serves GET /v1/agent/organisations/{id}/invitations.
func (h *Handler) Invitations(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.team(w, r)
	if !ok {
		return
	}
	items, err := h.svc.Invitations(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// Invite serves POST /v1/agent/organisations/{id}/invitations.
func (h *Handler) Invite(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.team(w, r)
	if !ok {
		return
	}
	var in InviteInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	inv, err := h.svc.Invite(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"invitation": inv})
}

// CancelInvitation serves DELETE /v1/agent/organisations/{id}/invitations/{invitationId}.
func (h *Handler) CancelInvitation(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.team(w, r)
	if !ok {
		return
	}
	invID, ok := pathUUID(r, "invitationId")
	if !ok {
		h.fail(w, r, ErrInvitationNotFound)
		return
	}
	if err := h.svc.CancelInvitation(r.Context(), p, id, invID); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PublicInvitation serves GET /v1/invitations/{token}: no session needed.
func (h *Handler) PublicInvitation(w http.ResponseWriter, r *http.Request) {
	inv, err := h.svc.Invitation(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"invitation": inv})
}

// Accept serves POST /v1/invitations/{token}/accept (signed in). No body.
func (h *Handler) Accept(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	out, err := h.svc.Accept(r.Context(), p, chi.URLParam(r, "token"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// JoinRequests serves GET /v1/agent/organisations/{id}/join-requests.
func (h *Handler) JoinRequests(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.team(w, r)
	if !ok {
		return
	}
	items, err := h.onboarding.JoinRequests(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ApproveJoinRequest serves POST .../join-requests/{requestId}/approve. No body.
func (h *Handler) ApproveJoinRequest(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.team(w, r)
	if !ok {
		return
	}
	reqID, ok := pathUUID(r, "requestId")
	if !ok {
		h.fail(w, r, onboarding.ErrJoinRequestNotFound)
		return
	}
	jr, err := h.onboarding.AgencyApprove(r.Context(), p, id, reqID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"join_request": jr})
}

// DeclineJoinRequest serves POST .../join-requests/{requestId}/decline {"reason"}.
func (h *Handler) DeclineJoinRequest(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.team(w, r)
	if !ok {
		return
	}
	reqID, ok := pathUUID(r, "requestId")
	if !ok {
		h.fail(w, r, onboarding.ErrJoinRequestNotFound)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	jr, err := h.onboarding.AgencyDecline(r.Context(), p, id, reqID, in.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"join_request": jr})
}

// Audit serves GET /v1/agent/organisations/{id}/audit?page=&page_size=.
func (h *Handler) Audit(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.team(w, r)
	if !ok {
		return
	}
	fields := map[string]string{}
	page, size := 1, DefaultPageSize
	if raw := r.URL.Query().Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100000 {
			fields["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxPageSize {
			fields["page_size"] = "must be a whole number from 1 to " + strconv.Itoa(MaxPageSize)
		} else {
			size = n
		}
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	out, err := h.svc.Audit(r.Context(), p, id, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}
