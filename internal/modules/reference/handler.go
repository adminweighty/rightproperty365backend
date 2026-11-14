package reference

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rightproperty365.com/backend/internal/platform/httpx"
)

// CodeUnknownSet is the error code for GET /v1/reference/{set} with a set
// that does not exist.
const CodeUnknownSet = "REFERENCE_SET_NOT_FOUND"

// Reference data changes rarely but must show up quickly after an edit: a
// short max-age with a longer stale-while-revalidate window, plus an ETag so
// revalidation is a cheap 304.
const (
	cacheMaxAge = time.Minute
	cacheStale  = 5 * time.Minute
)

// Handler serves GET /v1/reference and GET /v1/reference/{set}.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

type allResponse struct {
	// Market echoes the filter, or null when none was given.
	Market *string           `json:"market"`
	Sets   map[string][]Item `json:"sets"`
}

type setResponse struct {
	Set    string  `json:"set"`
	Market *string `json:"market"`
	Items  []Item  `json:"items"`
}

func marketParam(r *http.Request) (string, *string) {
	market := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("market")))
	if market == "" {
		return "", nil
	}
	return market, &market
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrUnknownSet):
		httpx.WriteError(w, r, http.StatusNotFound, CodeUnknownSet, "This reference set does not exist.", nil)
	case errors.Is(err, ErrUnknownMarket):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.",
			map[string]string{"market": "is not a known market code"})
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

// All handles GET /v1/reference.
func (h *Handler) All(w http.ResponseWriter, r *http.Request) {
	market, echo := marketParam(r)
	sets, err := h.svc.All(r.Context(), market)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, allResponse{Market: echo, Sets: sets}, cacheMaxAge, cacheStale)
}

// One handles GET /v1/reference/{set}.
func (h *Handler) One(w http.ResponseWriter, r *http.Request) {
	set := chi.URLParam(r, "set")
	market, echo := marketParam(r)
	items, err := h.svc.Set(r.Context(), set, market)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, setResponse{Set: set, Market: echo, Items: items}, cacheMaxAge, cacheStale)
}
