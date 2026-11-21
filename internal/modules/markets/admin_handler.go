package markets

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Error codes.
const (
	CodeMarketNotFound = "MARKET_NOT_FOUND"
	CodeMarketExists   = "MARKET_EXISTS"
	CodeMarketNotReady = "MARKET_NOT_READY"
)

// AdminHandler serves /v1/admin/markets.
type AdminHandler struct {
	admin  *Admin
	logger *slog.Logger
}

// NewAdminHandler builds the handler.
func NewAdminHandler(admin *Admin, logger *slog.Logger) *AdminHandler {
	return &AdminHandler{admin: admin, logger: logger}
}

type adminView struct {
	Code               string    `json:"code"`
	Name               string    `json:"name"`
	CurrencyCode       string    `json:"currency_code"`
	ExtraCurrencies    []string  `json:"extra_currencies"`
	Timezone           string    `json:"timezone"`
	Status             string    `json:"status"`
	DefaultPackageCode *string   `json:"default_package_code"`
	Notice             *string   `json:"notice"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func viewAdmin(m Market) adminView {
	v := adminView{Code: m.Code, Name: m.Name, CurrencyCode: m.CurrencyCode, ExtraCurrencies: m.ExtraCurrencies, Timezone: m.Timezone,
		Status: m.AdminStatus, Notice: m.Notice, UpdatedAt: m.UpdatedAt}
	if v.ExtraCurrencies == nil {
		v.ExtraCurrencies = []string{}
	}
	if m.DefaultPackageCode != "" {
		code := m.DefaultPackageCode
		v.DefaultPackageCode = &code
	}
	return v
}

func (h *AdminHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	var nerr *NotReadyError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &nerr):
		fields := map[string]string{}
		for i, p := range nerr.Problems {
			fields["readiness["+strconv.Itoa(i)+"]"] = p
		}
		httpx.WriteError(w, r, http.StatusConflict, CodeMarketNotReady, "This country is not ready to go live.", fields)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeMarketNotFound, "This country does not exist.", nil)
	case errors.Is(err, ErrExists):
		httpx.WriteError(w, r, http.StatusConflict, CodeMarketExists, "This country already exists.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Only a platform administrator can add a country.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

// List serves GET /v1/admin/markets.
func (h *AdminHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, err := h.admin.List(r.Context(), auth.FromContext(r.Context()), q.Get("status"), q.Get("q"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]adminView, 0, len(list))
	for _, m := range list {
		items = append(items, viewAdmin(m))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// Get serves GET /v1/admin/markets/{code}.
func (h *AdminHandler) Get(w http.ResponseWriter, r *http.Request) {
	m, ready, err := h.admin.Get(r.Context(), auth.FromContext(r.Context()), chi.URLParam(r, "code"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"market": viewAdmin(m), "readiness": ready})
}

// Create serves POST /v1/admin/markets.
func (h *AdminHandler) Create(w http.ResponseWriter, r *http.Request) {
	var in CreateInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	m, err := h.admin.Create(r.Context(), auth.FromContext(r.Context()), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"market": viewAdmin(m)})
}

// Update serves PATCH /v1/admin/markets/{code}.
func (h *AdminHandler) Update(w http.ResponseWriter, r *http.Request) {
	var in UpdateInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	m, ready, err := h.admin.Update(r.Context(), auth.FromContext(r.Context()), chi.URLParam(r, "code"), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"market": viewAdmin(m), "readiness": ready})
}
