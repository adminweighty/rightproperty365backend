package saved

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// CodeListingNotFound is the error code of a listing that cannot be saved.
const CodeListingNotFound = "LISTING_NOT_FOUND"

// Paging of GET /v1/me/saved-listings.
const (
	DefaultPageSize = 24
	MaxPageSize     = 100
)

// Handler serves /v1/me/saved-listings.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrUnauthenticated):
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Saving listings is for customer accounts.", nil)
	case errors.Is(err, ErrListingNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeListingNotFound, "This listing does not exist.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func listingID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "listingId"))
	return id, err == nil
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

// Save serves PUT /v1/me/saved-listings/{listingId}.
func (h *Handler) Save(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, ok := listingID(r)
	if !ok {
		if _, err := customer(p); err != nil {
			h.fail(w, r, err)
			return
		}
		h.fail(w, r, ErrListingNotFound)
		return
	}
	res, err := h.svc.Save(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, res)
}

// Remove serves DELETE /v1/me/saved-listings/{listingId}: 204 whether or not
// it was saved.
func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if _, err := customer(p); err != nil {
		h.fail(w, r, err)
		return
	}
	if id, ok := listingID(r); ok {
		if err := h.svc.Remove(r.Context(), p, id); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// List serves GET /v1/me/saved-listings.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if _, err := customer(p); err != nil {
		h.fail(w, r, err)
		return
	}
	fields := map[string]string{}
	q := r.URL.Query()
	page, size := 1, DefaultPageSize
	if raw := q.Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 100000 {
			fields["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := q.Get("page_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > MaxPageSize {
			fields["page_size"] = "must be a whole number from 1 to " + strconv.Itoa(MaxPageSize)
		} else {
			size = n
		}
	}
	if len(fields) > 0 {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", fields)
		return
	}
	out, err := h.svc.List(r.Context(), p, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// IDs serves GET /v1/me/saved-listings/ids.
func (h *Handler) IDs(w http.ResponseWriter, r *http.Request) {
	ids, err := h.svc.IDs(r.Context(), auth.FromContext(r.Context()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ids": ids})
}
