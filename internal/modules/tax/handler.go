package tax

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/i18n"
)

// Handler serves /v1/admin/tax-rules.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

// View is a rule as administrators see it.
type View struct {
	ID                      uuid.UUID  `json:"id"`
	MarketCode              string     `json:"market_code"`
	TaxName                 string     `json:"tax_name"`
	Rate                    string     `json:"rate"`
	AppliesTo               string     `json:"applies_to"`
	Inclusive               bool       `json:"inclusive"`
	EffectiveFrom           time.Time  `json:"effective_from"`
	EffectiveTo             *time.Time `json:"effective_to"`
	RegistrationNumberLabel *string    `json:"registration_number_label"`
	Status                  string     `json:"status"`
	IsSample                bool       `json:"is_sample"`
	Notes                   string     `json:"notes"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
}

func view(r Rule) View {
	return View{ID: r.ID, MarketCode: r.MarketCode, TaxName: r.TaxName, Rate: r.Rate, AppliesTo: r.AppliesTo, Inclusive: r.Inclusive,
		EffectiveFrom: r.EffectiveFrom, EffectiveTo: r.EffectiveTo, RegistrationNumberLabel: r.RegistrationNumberLabel, Status: r.Status,
		IsSample: r.IsSample, Notes: r.Notes, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteValidation(w, r, verr.Fields)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeRuleNotFound, i18n.T(ctx, "tax.error.not_found", nil), nil)
	case errors.Is(err, ErrOverlap):
		httpx.WriteError(w, r, http.StatusConflict, CodeRuleOverlap, i18n.T(ctx, "tax.error.overlap", nil), nil)
	case errors.Is(err, ErrRetired):
		httpx.WriteError(w, r, http.StatusConflict, CodeRuleRetired, i18n.T(ctx, "tax.error.retired", nil), nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, i18n.T(r.Context(), "error.unauthenticated", nil), nil)
	}
	return p
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, http.StatusNotFound, CodeRuleNotFound, i18n.T(r.Context(), "tax.error.not_found", nil), nil)
		return uuid.Nil, false
	}
	return id, true
}

// Routes mounts /v1/admin/tax-rules (the caller applies the role check;
// writes go through limit).
func (h *Handler) Routes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/", h.List)
	r.Get("/lookup", h.Lookup)
	r.Get("/{id}", h.Get)
	r.Group(func(r chi.Router) {
		r.Use(limit)
		r.Post("/", h.Create)
		r.Patch("/{id}", h.Update)
		r.Post("/{id}/retire", h.Retire)
	})
}

func (h *Handler) write(w http.ResponseWriter, status int, r Rule) {
	httpx.WriteJSON(w, status, map[string]any{"tax_rule": view(r)})
}

// List handles GET /v1/admin/tax-rules.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	q := r.URL.Query()
	list, err := h.svc.List(r.Context(), Filter{Market: q.Get("market"), AppliesTo: q.Get("applies_to"), Status: q.Get("status")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := struct {
		Rules []View `json:"tax_rules"`
	}{Rules: make([]View, 0, len(list))}
	for _, rule := range list {
		out.Rules = append(out.Rules, view(rule))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Lookup handles GET /v1/admin/tax-rules/lookup?market=&applies_to=&at=.
func (h *Handler) Lookup(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	q := r.URL.Query()
	fields := map[string]string{}
	market := normaliseMarket(q.Get("market"))
	if market == "" {
		fields["market"] = i18n.T(r.Context(), "field.required", nil)
	}
	appliesTo := strings.ToUpper(strings.TrimSpace(q.Get("applies_to")))
	if appliesTo == "" {
		fields["applies_to"] = i18n.T(r.Context(), "field.required", nil)
	}
	at := h.svc.now().UTC()
	if raw := strings.TrimSpace(q.Get("at")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fields["at"] = "must be an RFC 3339 time"
		}
		at = t
	}
	if len(fields) > 0 {
		httpx.WriteValidation(w, r, fields)
		return
	}
	rule, ok, err := h.svc.TaxFor(r.Context(), market, appliesTo, at)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := struct {
		Rule *View `json:"tax_rule"`
	}{}
	if ok {
		v := view(rule)
		out.Rule = &v
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Get handles GET /v1/admin/tax-rules/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rule, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, rule)
}

// Create handles POST /v1/admin/tax-rules.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	rule, err := h.svc.Create(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusCreated, rule)
}

// Update handles PATCH /v1/admin/tax-rules/{id}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	rule, err := h.svc.Update(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, rule)
}

// Retire handles POST /v1/admin/tax-rules/{id}/retire.
func (h *Handler) Retire(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rule, err := h.svc.Retire(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, rule)
}
