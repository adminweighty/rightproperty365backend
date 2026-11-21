package listings

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Placement error codes.
const (
	CodePlacementNotFound     = "PLACEMENT_NOT_FOUND"
	CodePlacementOverlap      = "PLACEMENT_OVERLAP"
	CodePlacementLimit        = "PLACEMENT_LIMIT"
	CodePlacementEnded        = "PLACEMENT_ENDED"
	CodePlacementNotPublished = "LISTING_NOT_PUBLISHED"
)

// PlacementHandler serves the administrators' placement endpoints.
type PlacementHandler struct {
	svc    *Placements
	logger *slog.Logger
}

// NewPlacementHandler builds the handler.
func NewPlacementHandler(svc *Placements, logger *slog.Logger) *PlacementHandler {
	return &PlacementHandler{svc: svc, logger: logger}
}

type placementListingView struct {
	ID         uuid.UUID `json:"id"`
	Slug       string    `json:"slug"`
	Title      string    `json:"title"`
	Reference  string    `json:"reference"`
	MarketCode string    `json:"market_code"`
}

type adminPlacementView struct {
	ID             uuid.UUID            `json:"id"`
	Listing        placementListingView `json:"listing"`
	Kind           string               `json:"kind"`
	Label          string               `json:"label"`
	StartsAt       time.Time            `json:"starts_at"`
	EndsAt         time.Time            `json:"ends_at"`
	Status         string               `json:"status"`
	SubscriptionID *uuid.UUID           `json:"subscription_id"`
	Note           *string              `json:"note"`
	GrantedBy      string               `json:"granted_by"`
	GrantedAt      time.Time            `json:"granted_at"`
	EndedAt        *time.Time           `json:"ended_at"`
	EndReason      *string              `json:"end_reason"`
}

func viewPlacement(p Placement, now time.Time) adminPlacementView {
	return adminPlacementView{ID: p.ID, Listing: placementListingView{ID: p.ListingID, Slug: p.ListingSlug, Title: p.ListingTitle, Reference: p.ListingReference, MarketCode: p.MarketCode},
		Kind: p.Kind, Label: PlacementLabel(p.Kind), StartsAt: p.StartsAt, EndsAt: p.EndsAt, Status: p.Status(now), SubscriptionID: p.SubscriptionID,
		Note: opt(p.Note), GrantedBy: p.GrantedByName, GrantedAt: p.GrantedAt, EndedAt: p.EndedAt, EndReason: opt(p.EndReason)}
}

func (h *PlacementHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeListingNotFound, "This listing does not exist.", nil)
	case errors.Is(err, ErrPlacementNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodePlacementNotFound, "This placement does not exist.", nil)
	case errors.Is(err, ErrPlacementOverlap):
		httpx.WriteError(w, r, http.StatusConflict, CodePlacementOverlap, "The listing already has a placement in that period.", nil)
	case errors.Is(err, ErrPlacementLimit):
		httpx.WriteError(w, r, http.StatusConflict, CodePlacementLimit, "The subscription's featured listings are all in use in that period.", nil)
	case errors.Is(err, ErrPlacementEnded):
		httpx.WriteError(w, r, http.StatusConflict, CodePlacementEnded, "This placement has already ended.", nil)
	case errors.Is(err, ErrPlacementNotPublished):
		httpx.WriteError(w, r, http.StatusConflict, CodePlacementNotPublished, "Only a published listing can be given a placement.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

// List serves GET /v1/admin/placements.
func (h *PlacementHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := PlacementFilter{Status: q.Get("status"), Page: 1, PageSize: 50}
	fields := map[string]string{}
	if raw := q.Get("listing_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			fields["listing_id"] = "must be a UUID"
		}
		f.ListingID = &id
	}
	switch f.Status {
	case "", PlacementActive, PlacementScheduled, PlacementEnded:
	default:
		fields["status"] = "must be ACTIVE, SCHEDULED or ENDED"
	}
	if raw := q.Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			fields["page"] = "must be a whole number from 1"
		}
		f.Page = n
	}
	if len(fields) > 0 {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", fields)
		return
	}
	list, total, err := h.svc.List(r.Context(), auth.FromContext(r.Context()), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := h.svc.clock()
	items := make([]adminPlacementView, 0, len(list))
	for _, p := range list {
		items = append(items, viewPlacement(p, now))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": f.Page, "page_size": f.PageSize})
}

// Grant serves POST /v1/admin/placements.
func (h *PlacementHandler) Grant(w http.ResponseWriter, r *http.Request) {
	var in PlacementInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	id, err := uuid.Parse(in.ListingID)
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.",
			map[string]string{"listing_id": "must be a UUID"})
		return
	}
	p, err := h.svc.Grant(r.Context(), auth.FromContext(r.Context()), id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"placement": viewPlacement(p, h.svc.clock())})
}

// End serves POST /v1/admin/placements/{id}/end.
func (h *PlacementHandler) End(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, ErrPlacementNotFound)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, err := h.svc.End(r.Context(), auth.FromContext(r.Context()), id, in.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"placement": viewPlacement(p, h.svc.clock())})
}
