package tenancies

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/logging"
)

// Error codes.
const (
	CodeBookNotFound             = "BOOK_NOT_FOUND"
	CodePropertyNotFound         = "PROPERTY_NOT_FOUND"
	CodeUnitNotFound             = "UNIT_NOT_FOUND"
	CodeTenancyNotFound          = "TENANCY_NOT_FOUND"
	CodePropertyInUse            = "PROPERTY_IN_USE"
	CodeUnitInUse                = "UNIT_IN_USE"
	CodeUnitOccupied             = "UNIT_OCCUPIED"
	CodeInvalidState             = "TENANCY_INVALID_STATE"
	DefaultPageSize, MaxPageSize = 25, 100
)

// Handler serves /v1/agent/organisations/{id}/properties, .../tenancies
// (with their dues, receipts and statement), .../dues and .../arrears (the
// book is the route's {id}), and /v1/me/tenancies.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

type detailBody struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
	TraceID string `json:"trace_id"`
}

func writeDetails(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	var b detailBody
	b.Error.Code, b.Error.Message, b.Error.Details, b.TraceID = code, message, details, logging.RequestID(r.Context())
	httpx.WriteJSON(w, status, b)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	var occupied *OccupiedError
	var state *StateError
	if h.failRent(w, r, err) {
		return
	}
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &occupied):
		var other any
		if occupied.TenancyID != nil {
			other = *occupied.TenancyID
		}
		writeDetails(w, r, http.StatusConflict, CodeUnitOccupied, "This unit already has an active tenancy. End it first.",
			map[string]any{"tenancy_id": other})
	case errors.Is(err, ErrUnitOccupied):
		writeDetails(w, r, http.StatusConflict, CodeUnitOccupied, "This unit already has an active tenancy. End it first.",
			map[string]any{"tenancy_id": nil})
	case errors.As(err, &state):
		writeDetails(w, r, http.StatusConflict, CodeInvalidState, "This cannot be done while the tenancy is "+strings.ToLower(StatusLabel(state.Status))+".",
			map[string]any{"status": state.Status})
	case errors.Is(err, ErrConflict):
		writeDetails(w, r, http.StatusConflict, CodeInvalidState, "The tenancy changed meanwhile. Reload it and try again.", map[string]any{"status": nil})
	case errors.Is(err, ErrUnauthenticated):
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	case errors.Is(err, ErrBookNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeBookNotFound, "These books do not exist or are not yours.", nil)
	case errors.Is(err, ErrPropertyNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodePropertyNotFound, "This property does not exist in these books.", nil)
	case errors.Is(err, ErrUnitNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeUnitNotFound, "This unit does not exist in this property.", nil)
	case errors.Is(err, ErrTenancyNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeTenancyNotFound, "This tenancy does not exist or is not yours.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Your role does not allow this.", nil)
	case errors.Is(err, ErrPropertyInUse):
		httpx.WriteError(w, r, http.StatusConflict, CodePropertyInUse, "A draft or active tenancy uses this property. End or delete it first.", nil)
	case errors.Is(err, ErrUnitInUse):
		httpx.WriteError(w, r, http.StatusConflict, CodeUnitInUse, "A draft or active tenancy uses this unit. End or delete it first.", nil)
	case errors.Is(err, ErrUnitLabelTaken):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.",
			map[string]string{"label": "is already a unit of this property"})
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func private(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
}

func (h *Handler) ok(w http.ResponseWriter, status int, v any) {
	private(w)
	httpx.WriteJSON(w, status, v)
}

func (h *Handler) noContent(w http.ResponseWriter) {
	private(w)
	w.WriteHeader(http.StatusNoContent)
}

// begin resolves the caller and the book (404 first, before any validation
// or decoding) and parses the other path IDs (a malformed one is the 404 of
// what it names).
func (h *Handler) begin(w http.ResponseWriter, r *http.Request, names ...string) (*auth.Principal, uuid.UUID, []uuid.UUID, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		h.fail(w, r, ErrUnauthenticated)
		return nil, uuid.Nil, nil, false
	}
	book, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, ErrBookNotFound)
		return nil, uuid.Nil, nil, false
	}
	if err := h.svc.Check(p, book); err != nil {
		h.fail(w, r, err)
		return nil, uuid.Nil, nil, false
	}
	out := make([]uuid.UUID, 0, len(names))
	for _, n := range names {
		id, err := uuid.Parse(chi.URLParam(r, n))
		if err != nil {
			switch n {
			case "propertyId":
				h.fail(w, r, ErrPropertyNotFound)
			case "unitId":
				h.fail(w, r, ErrUnitNotFound)
			case "dueId":
				h.fail(w, r, ErrDueNotFound)
			case "receiptId":
				h.fail(w, r, ErrReceiptNotFound)
			default:
				h.fail(w, r, ErrTenancyNotFound)
			}
			return nil, uuid.Nil, nil, false
		}
		out = append(out, id)
	}
	return p, book, out, true
}

func paging(q url.Values, f map[string]string) (int, int) {
	page, size := 1, DefaultPageSize
	if raw := q.Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 100000 {
			f["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := q.Get("page_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > MaxPageSize {
			f["page_size"] = "must be a whole number from 1 to " + strconv.Itoa(MaxPageSize)
		} else {
			size = n
		}
	}
	return page, size
}

func optionalID(q url.Values, name string, f map[string]string) *uuid.UUID {
	raw := q.Get(name)
	if raw == "" {
		return nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		f[name] = "must be a UUID"
		return nil
	}
	return &id
}

// ---------------------------------------------------------------------------
// Properties
// ---------------------------------------------------------------------------

// ListProperties serves GET .../properties.
func (h *Handler) ListProperties(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := map[string]string{}
	page, size := paging(q, f)
	flt := PropertyFilter{Q: strings.TrimSpace(q.Get("q")), MarketCode: strings.ToUpper(strings.TrimSpace(q.Get("market_code")))}
	if len(f) > 0 {
		h.fail(w, r, &ValidationError{Fields: f})
		return
	}
	out, err := h.svc.ListProperties(r.Context(), p, book, flt, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

// CreateProperty serves POST .../properties.
func (h *Handler) CreateProperty(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	var in PropertyInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.CreateProperty(r.Context(), p, book, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"property": out})
}

// GetProperty serves GET .../properties/{propertyId}.
func (h *Handler) GetProperty(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "propertyId")
	if !ok {
		return
	}
	out, err := h.svc.GetProperty(r.Context(), p, book, ids[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"property": out})
}

// UpdateProperty serves PATCH .../properties/{propertyId}.
func (h *Handler) UpdateProperty(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "propertyId")
	if !ok {
		return
	}
	var in PropertyInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.UpdateProperty(r.Context(), p, book, ids[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"property": out})
}

// DeleteProperty serves DELETE .../properties/{propertyId}.
func (h *Handler) DeleteProperty(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "propertyId")
	if !ok {
		return
	}
	if err := h.svc.DeleteProperty(r.Context(), p, book, ids[0]); err != nil {
		h.fail(w, r, err)
		return
	}
	h.noContent(w)
}

// AddUnit serves POST .../properties/{propertyId}/units.
func (h *Handler) AddUnit(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "propertyId")
	if !ok {
		return
	}
	var in UnitInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.AddUnit(r.Context(), p, book, ids[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"property": out})
}

// RenameUnit serves PATCH .../properties/{propertyId}/units/{unitId}.
func (h *Handler) RenameUnit(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "propertyId", "unitId")
	if !ok {
		return
	}
	var in UnitInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.RenameUnit(r.Context(), p, book, ids[0], ids[1], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"property": out})
}

// DeleteUnit serves DELETE .../properties/{propertyId}/units/{unitId}.
func (h *Handler) DeleteUnit(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "propertyId", "unitId")
	if !ok {
		return
	}
	out, err := h.svc.DeleteUnit(r.Context(), p, book, ids[0], ids[1])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"property": out})
}

// ---------------------------------------------------------------------------
// Tenancies
// ---------------------------------------------------------------------------

// Options serves GET .../tenancies/options.
func (h *Handler) Options(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Options(r.Context(), p, book)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

// ListTenancies serves GET .../tenancies.
func (h *Handler) ListTenancies(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := map[string]string{}
	page, size := paging(q, f)
	flt := TenancyFilter{Status: q.Get("status"), PropertyID: optionalID(q, "property_id", f), UnitID: optionalID(q, "unit_id", f),
		ContactID: optionalID(q, "contact_id", f)}
	if flt.Status != "" && !slices.Contains(Statuses(), flt.Status) {
		f["status"] = "must be " + strings.Join(Statuses(), ", ")
	}
	if len(f) > 0 {
		h.fail(w, r, &ValidationError{Fields: f})
		return
	}
	out, err := h.svc.ListTenancies(r.Context(), p, book, flt, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

// CreateTenancy serves POST .../tenancies.
func (h *Handler) CreateTenancy(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	var in TenancyInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.CreateTenancy(r.Context(), p, book, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"tenancy": out})
}

// GetTenancy serves GET .../tenancies/{tenancyId}.
func (h *Handler) GetTenancy(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "tenancyId")
	if !ok {
		return
	}
	out, err := h.svc.GetTenancy(r.Context(), p, book, ids[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"tenancy": out})
}

// UpdateTenancy serves PATCH .../tenancies/{tenancyId}.
func (h *Handler) UpdateTenancy(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "tenancyId")
	if !ok {
		return
	}
	var in TenancyInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.UpdateTenancy(r.Context(), p, book, ids[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"tenancy": out})
}

// DeleteTenancy serves DELETE .../tenancies/{tenancyId}.
func (h *Handler) DeleteTenancy(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "tenancyId")
	if !ok {
		return
	}
	if err := h.svc.DeleteTenancy(r.Context(), p, book, ids[0]); err != nil {
		h.fail(w, r, err)
		return
	}
	h.noContent(w)
}

// Activate serves POST .../tenancies/{tenancyId}/activate (any body is ignored).
func (h *Handler) Activate(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "tenancyId")
	if !ok {
		return
	}
	out, err := h.svc.Activate(r.Context(), p, book, ids[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"tenancy": out})
}

// End serves POST .../tenancies/{tenancyId}/end.
func (h *Handler) End(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "tenancyId")
	if !ok {
		return
	}
	var in EndInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.End(r.Context(), p, book, ids[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"tenancy": out})
}

// PropertyRoutes mounts .../properties under a router whose path holds {id}.
func (h *Handler) PropertyRoutes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/", h.ListProperties)
	r.With(limit).Post("/", h.CreateProperty)
	r.Get("/{propertyId}", h.GetProperty)
	r.With(limit).Patch("/{propertyId}", h.UpdateProperty)
	r.With(limit).Delete("/{propertyId}", h.DeleteProperty)
	r.With(limit).Post("/{propertyId}/units", h.AddUnit)
	r.With(limit).Patch("/{propertyId}/units/{unitId}", h.RenameUnit)
	r.With(limit).Delete("/{propertyId}/units/{unitId}", h.DeleteUnit)
}

// TenancyRoutes mounts .../tenancies under a router whose path holds {id}.
func (h *Handler) TenancyRoutes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/", h.ListTenancies)
	r.With(limit).Post("/", h.CreateTenancy)
	r.Get("/options", h.Options)
	r.Get("/{tenancyId}", h.GetTenancy)
	r.With(limit).Patch("/{tenancyId}", h.UpdateTenancy)
	r.With(limit).Delete("/{tenancyId}", h.DeleteTenancy)
	r.With(limit).Post("/{tenancyId}/activate", h.Activate)
	r.With(limit).Post("/{tenancyId}/end", h.End)
	h.rentRoutes(r, limit)
}

// ---------------------------------------------------------------------------
// The tenant's own tenancies
// ---------------------------------------------------------------------------

// MyTenancies serves GET /v1/me/tenancies.
func (h *Handler) MyTenancies(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.MyTenancies(r.Context(), auth.FromContext(r.Context()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"items": out})
}

// MyTenancy serves GET /v1/me/tenancies/{id}.
func (h *Handler) MyTenancy(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		h.fail(w, r, ErrUnauthenticated)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, ErrTenancyNotFound)
		return
	}
	out, err := h.svc.MyTenancy(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"tenancy": out})
}
