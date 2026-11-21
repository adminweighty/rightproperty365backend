package savedsearches

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
	CodeNotFound     = "SAVED_SEARCH_NOT_FOUND"
	CodeLimit        = "SAVED_SEARCH_LIMIT"
	CodeInvalidToken = "INVALID_TOKEN"
)

// Handler serves the saved search endpoints.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

type view struct {
	ID          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	Query       string     `json:"query"`
	Frequency   string     `json:"frequency"`
	LastAlertAt *time.Time `json:"last_alert_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func viewOf(ss SavedSearch) view {
	return view{ID: ss.ID, Name: ss.Name, Query: ss.Query, Frequency: ss.Frequency, LastAlertAt: ss.LastAlertAt, CreatedAt: ss.CreatedAt, UpdatedAt: ss.UpdatedAt}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotFound, "This saved search does not exist.", nil)
	case errors.Is(err, ErrLimit):
		httpx.WriteError(w, r, http.StatusConflict, CodeLimit, "You already have 25 saved searches. Delete one first.", nil)
	case errors.Is(err, ErrInvalidToken):
		httpx.WriteError(w, r, http.StatusBadRequest, CodeInvalidToken, "This unsubscribe link is not valid.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func (h *Handler) user(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
		return uuid.Nil, false
	}
	return p.UserID, true
}

func idOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotFound, "This saved search does not exist.", nil)
		return uuid.Nil, false
	}
	return id, true
}

// List serves GET /v1/me/saved-searches.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	user, ok := h.user(w, r)
	if !ok {
		return
	}
	list, err := h.svc.List(r.Context(), user)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]view, 0, len(list))
	for _, ss := range list {
		items = append(items, viewOf(ss))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// Create serves POST /v1/me/saved-searches.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	user, ok := h.user(w, r)
	if !ok {
		return
	}
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	ss, err := h.svc.Create(r.Context(), user, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"saved_search": viewOf(ss)})
}

// Update serves PATCH /v1/me/saved-searches/{id}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	user, ok := h.user(w, r)
	if !ok {
		return
	}
	id, ok := idOf(w, r)
	if !ok {
		return
	}
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	ss, err := h.svc.Update(r.Context(), user, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"saved_search": viewOf(ss)})
}

// Delete serves DELETE /v1/me/saved-searches/{id}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	user, ok := h.user(w, r)
	if !ok {
		return
	}
	id, ok := idOf(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), user, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Unsubscribe serves POST /v1/saved-searches/unsubscribe (public).
func (h *Handler) Unsubscribe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	ss, err := h.svc.Unsubscribe(r.Context(), in.Token)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"name": ss.Name})
}
