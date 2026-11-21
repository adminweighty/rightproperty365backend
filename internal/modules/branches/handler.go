package branches

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Error codes.
const (
	CodeBranchNotFound = "BRANCH_NOT_FOUND"
	CodeNameTaken      = "BRANCH_NAME_TAKEN"
	CodeTooMany        = "BRANCH_LIMIT"
)

// Handler serves the branch endpoints of an organisation.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

type view struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func viewOf(b Branch) view {
	return view{ID: b.ID, Name: b.Name, Active: b.Active, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Only the organisation's owners and managers can manage its branches.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeBranchNotFound, "This branch does not exist.", nil)
	case errors.Is(err, ErrNameTaken):
		httpx.WriteError(w, r, http.StatusConflict, CodeNameTaken, "Another branch already has that name.", nil)
	case errors.Is(err, ErrTooMany):
		httpx.WriteError(w, r, http.StatusConflict, CodeTooMany, "The organisation already has 100 branches.", nil)
	case errors.Is(err, ErrNotMember):
		httpx.WriteError(w, r, http.StatusNotFound, "MEMBER_NOT_FOUND", "This person is not a member of the organisation.", nil)
	case errors.Is(err, ErrNoListing):
		httpx.WriteError(w, r, http.StatusNotFound, "LISTING_NOT_FOUND", "This listing is not the organisation's.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func ids(w http.ResponseWriter, r *http.Request, names ...string) ([]uuid.UUID, bool) {
	out := make([]uuid.UUID, 0, len(names))
	for _, n := range names {
		id, err := uuid.Parse(chi.URLParam(r, n))
		if err != nil {
			httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "Not found.", nil)
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}

// List serves GET /v1/agent/organisations/{id}/branches (with the pins).
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	v, ok := ids(w, r, "id")
	if !ok {
		return
	}
	p := auth.FromContext(r.Context())
	list, err := h.svc.List(r.Context(), p, v[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	pins, err := h.svc.Pins(r.Context(), p, v[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]view, 0, len(list))
	for _, b := range list {
		items = append(items, viewOf(b))
	}
	members := map[string]string{}
	for u, b := range pins {
		members[u.String()] = b.String()
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "member_branches": members})
}

// Create serves POST /v1/agent/organisations/{id}/branches.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	v, ok := ids(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	b, err := h.svc.Create(r.Context(), auth.FromContext(r.Context()), v[0], in.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"branch": viewOf(b)})
}

// Update serves PATCH /v1/agent/organisations/{id}/branches/{branchId}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	v, ok := ids(w, r, "id", "branchId")
	if !ok {
		return
	}
	var in struct {
		Name   *string `json:"name"`
		Active *bool   `json:"active"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	b, err := h.svc.Update(r.Context(), auth.FromContext(r.Context()), v[0], v[1], in.Name, in.Active)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"branch": viewOf(b)})
}

type assignment struct {
	BranchID *string `json:"branch_id"`
}

func (a assignment) branch() (*uuid.UUID, error) {
	if a.BranchID == nil || *a.BranchID == "" {
		return nil, nil
	}
	id, err := uuid.Parse(*a.BranchID)
	if err != nil {
		return nil, &ValidationError{Fields: map[string]string{"branch_id": "must be a UUID or null"}}
	}
	return &id, nil
}

// PinMember serves PUT /v1/agent/organisations/{id}/members/{userId}/branch.
func (h *Handler) PinMember(w http.ResponseWriter, r *http.Request) {
	v, ok := ids(w, r, "id", "userId")
	if !ok {
		return
	}
	var in assignment
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	branch, err := in.branch()
	if err == nil {
		err = h.svc.PinMember(r.Context(), auth.FromContext(r.Context()), v[0], v[1], branch)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// TieListing serves PUT /v1/agent/organisations/{id}/listings/{listingId}/branch.
func (h *Handler) TieListing(w http.ResponseWriter, r *http.Request) {
	v, ok := ids(w, r, "id", "listingId")
	if !ok {
		return
	}
	var in assignment
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	branch, err := in.branch()
	if err == nil {
		err = h.svc.TieListing(r.Context(), auth.FromContext(r.Context()), v[0], v[1], branch)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
