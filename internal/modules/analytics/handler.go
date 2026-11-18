package analytics

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Handler serves the analytics endpoints.
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
	case errors.Is(err, ErrUnauthenticated):
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You are not allowed to see these figures.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.NotFound(w, r)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

// ingestBody is the body of POST /v1/analytics/events. Events are decoded
// one by one so that a bad event only rejects itself.
type ingestBody struct {
	Schema *int              `json:"schema"`
	Events []json.RawMessage `json:"events"`
}

// Ingest serves POST /v1/analytics/events (public; signed-in optional).
// The body is JSON, sent as application/json or as text/plain (what
// navigator.sendBeacon sends for a string, without a preflight).
func (h *Handler) Ingest(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && mediaType != "text/plain") {
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, httpx.CodeUnsupportedMediaType, "Content-Type must be application/json or text/plain.", nil)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge, "The request body is too large.", nil)
			return
		}
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeInvalidJSON, "The request body could not be read.", nil)
		return
	}
	var body ingestBody
	if err := json.Unmarshal(raw, &body); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeInvalidJSON, "The request body is not valid JSON for this endpoint.", nil)
		return
	}
	schema := 0
	if body.Schema != nil {
		schema = *body.Schema
	}
	res, err := h.svc.Ingest(r.Context(), Client{UserAgent: r.Header.Get("User-Agent"), IP: httpx.ClientIP(r)}, schema, body.Events)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusAccepted, res)
}

func period(r *http.Request) (PeriodQuery, error) {
	q := r.URL.Query()
	return ParsePeriod(strings.TrimSpace(q.Get("period")), strings.TrimSpace(q.Get("from")), strings.TrimSpace(q.Get("to")))
}

// ListingReport serves GET /v1/analytics/listings/{id}.
func (h *Handler) ListingReport(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		h.fail(w, r, ErrUnauthenticated)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, ErrNotFound)
		return
	}
	q, err := period(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.svc.ListingReport(r.Context(), p, id, q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// OrganisationReport serves GET /v1/analytics/organisations/{orgId}.
func (h *Handler) OrganisationReport(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		h.fail(w, r, ErrUnauthenticated)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "orgId"))
	if err != nil {
		h.fail(w, r, ErrNotFound)
		return
	}
	q, err := period(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.svc.OrganisationReport(r.Context(), p, id, q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// AdminSummary serves GET /v1/admin/analytics/summary.
func (h *Handler) AdminSummary(w http.ResponseWriter, r *http.Request) {
	q, err := period(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	market := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("market")))
	out, err := h.svc.AdminSummary(r.Context(), auth.FromContext(r.Context()), market, q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}
