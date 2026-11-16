package reference

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// AdminHandler serves /v1/admin/reference/*.
type AdminHandler struct {
	admin  *Admin
	logger *slog.Logger
}

// NewAdminHandler builds the handler.
func NewAdminHandler(admin *Admin, logger *slog.Logger) *AdminHandler {
	return &AdminHandler{admin: admin, logger: logger}
}

// AdminItem is a row as administrators see it: with its active flag.
type AdminItem struct {
	Item
	Active bool `json:"active"`
}

func adminItem(it Item) AdminItem { return AdminItem{Item: it, Active: it.Active} }

func (h *AdminHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrUnknownSet):
		httpx.WriteError(w, r, http.StatusNotFound, CodeUnknownSet, "This reference set does not exist.", nil)
	case errors.Is(err, ErrUnknownMarket):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.",
			map[string]string{"market": "is not a known market code"})
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeReferenceNotFound, "This reference value does not exist.", nil)
	case errors.Is(err, ErrDuplicate):
		httpx.WriteError(w, r, http.StatusConflict, CodeReferenceDuplicate, "A value with this code already exists in the set (it may be inactive: reactivate it instead).", nil)
	case errors.Is(err, ErrNotCreatable):
		httpx.WriteError(w, r, http.StatusUnprocessableEntity, CodeReferenceNotCreatable,
			"Values cannot be added to this set: the platform's behaviour depends on its codes. Relabel, reorder or deactivate them instead.", nil)
	case errors.Is(err, ErrMarketForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, CodeReferenceMarketForbidden, "You can only change the values of your own market.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

// Sets handles GET /v1/admin/reference: every set and whether rows can be added to it.
func (h *AdminHandler) Sets(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	type setView struct {
		Set         string  `json:"set"`
		Creatable   bool    `json:"creatable"`
		MarketBound bool    `json:"market_bound"`
		ParentSet   *string `json:"parent_set"`
	}
	out := struct {
		Sets []setView `json:"sets"`
	}{}
	for _, set := range Sets() {
		v := setView{Set: set}
		for _, c := range CreatableSets() {
			v.Creatable = v.Creatable || c == set
		}
		for _, m := range marketSets {
			v.MarketBound = v.MarketBound || m == set
		}
		if parent, ok := parentSets[set]; ok {
			v.ParentSet = &parent
		}
		out.Sets = append(out.Sets, v)
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// List handles GET /v1/admin/reference/{set}: every row, inactive ones too.
func (h *AdminHandler) List(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	set := chi.URLParam(r, "set")
	items, err := h.admin.List(r.Context(), set, strings.TrimSpace(r.URL.Query().Get("market")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := struct {
		Set   string      `json:"set"`
		Items []AdminItem `json:"items"`
	}{Set: set, Items: make([]AdminItem, 0, len(items))}
	for _, it := range items {
		out.Items = append(out.Items, adminItem(it))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *AdminHandler) write(w http.ResponseWriter, status int, it Item) {
	private(w)
	httpx.WriteJSON(w, status, map[string]any{"item": adminItem(it)})
}

// Create handles POST /v1/admin/reference/{set}.
func (h *AdminHandler) Create(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var in ItemInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	it, err := h.admin.Create(r.Context(), p, chi.URLParam(r, "set"), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusCreated, it)
}

// Update handles PATCH /v1/admin/reference/{set}/{code}.
func (h *AdminHandler) Update(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var in ItemInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	it, err := h.admin.Update(r.Context(), p, chi.URLParam(r, "set"), chi.URLParam(r, "code"), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, it)
}

// Deactivate handles POST /v1/admin/reference/{set}/{code}/deactivate.
func (h *AdminHandler) Deactivate(w http.ResponseWriter, r *http.Request) { h.setActive(w, r, false) }

// Reactivate handles POST /v1/admin/reference/{set}/{code}/reactivate.
func (h *AdminHandler) Reactivate(w http.ResponseWriter, r *http.Request) { h.setActive(w, r, true) }

func (h *AdminHandler) setActive(w http.ResponseWriter, r *http.Request, active bool) {
	p := principal(w, r)
	if p == nil {
		return
	}
	it, err := h.admin.SetActive(r.Context(), p, chi.URLParam(r, "set"), chi.URLParam(r, "code"), active)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, it)
}
