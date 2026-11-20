package availability

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// CodeNotShortStay is the error code of a calendar request for a listing
// that is not in the short-stay category.
const CodeNotShortStay = "NOT_SHORT_STAY"

var (
	slugRe   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	codeRe   = regexp.MustCompile(`^[a-z0-9-]{1,60}$`)
	statusRe = regexp.MustCompile(`^[A-Z_]{1,30}$`)
)

// Handler serves the lister's calendar endpoints under /v1/me/listings and
// the public GET /v1/listings/{slug}/availability.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You do not have permission to do this.", nil)
	case errors.Is(err, ErrNotFound), errors.Is(err, listings.ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, listings.CodeListingNotFound, "This listing does not exist.", nil)
	case errors.Is(err, listings.ErrUnavailable):
		httpx.WriteError(w, r, http.StatusGone, listings.CodeListingUnavailable, "This listing is no longer available.", nil)
	case errors.Is(err, ErrNotShortStay):
		httpx.WriteError(w, r, http.StatusConflict, CodeNotShortStay, "Only short stays have an availability calendar.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

// principal returns the signed-in caller (the routes require one).
func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

// listingID reads the {id} path parameter. A value that is not a UUID names
// no listing.
func (h *Handler) listingID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, ErrNotFound)
		return uuid.Nil, false
	}
	return id, true
}

// MyListings handles GET /v1/me/listings.
func (h *Handler) MyListings(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	v := r.URL.Query()
	fields := map[string]string{}
	page, size := listings.ParsePaging(v, fields)
	category, status := v.Get("category"), v.Get("status")
	if category != "" && !codeRe.MatchString(category) {
		fields["category"] = "is not a known value; see GET /v1/reference/property_categories"
	}
	if status != "" && !statusRe.MatchString(status) {
		fields["status"] = "must be a listing status such as PUBLISHED"
	}
	// The state of the publication period (API 0.45.0).
	expiry := v.Get("expiry")
	if expiry != "" && expiry != listings.ExpiryExpiringSoon && expiry != listings.ExpiryExpired {
		fields["expiry"] = "must be EXPIRING_SOON or EXPIRED"
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	out, err := h.svc.MyListings(r.Context(), p, category, status, expiry, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Calendar handles GET /v1/me/listings/{id}/availability.
func (h *Handler) Calendar(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := h.listingID(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Calendar(r.Context(), p, id, r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

type changeRequest struct {
	Changes []ChangeInput `json:"changes"`
}

// Change handles PUT /v1/me/listings/{id}/availability.
func (h *Handler) Change(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := h.listingID(w, r)
	if !ok {
		return
	}
	var in changeRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Change(r.Context(), p, id, in.Changes)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Identifiers only: reasons and notes are the lister's private data.
	h.logger.InfoContext(r.Context(), "availability changed", slog.String("listing_id", id.String()),
		slog.String("actor_user_id", p.UserID.String()), slog.Int("changes", len(in.Changes)))
	httpx.WriteJSON(w, http.StatusOK, out)
}

type stayRulesRequest struct {
	MinNights *int `json:"min_nights"`
	MaxGuests *int `json:"max_guests"`
}

type listingResponse struct {
	Listing ListingSummary `json:"listing"`
}

// StayRules handles PATCH /v1/me/listings/{id}/stay-rules.
func (h *Handler) StayRules(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := h.listingID(w, r)
	if !ok {
		return
	}
	var in stayRulesRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.SetStayRules(r.Context(), p, id, in.MinNights, in.MaxGuests)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, listingResponse{Listing: out})
}

// Public handles GET /v1/listings/{slug}/availability. It never reads the
// session and is cacheable like the listing itself.
func (h *Handler) Public(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if !slugRe.MatchString(slug) || len(slug) > 200 {
		h.fail(w, r, listings.ErrNotFound)
		return
	}
	out, err := h.svc.Public(r.Context(), slug, r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, out, listings.CacheMaxAge, listings.CacheStale)
}
