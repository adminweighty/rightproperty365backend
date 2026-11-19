package legal

import (
	"context"
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

// Public responses may be cached this long.
const (
	cacheMaxAge = 5 * time.Minute
	cacheStale  = time.Minute
)

// Handler serves /v1/legal, /v1/me/legal-acceptances and /v1/admin/legal.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

// Service returns the service (for /v1/me's pending acceptances).
func (h *Handler) Service() *Service { return h.svc }

// PersonRef names who created or published a version.
type PersonRef struct {
	ID   *uuid.UUID `json:"id"`
	Name string     `json:"name"`
}

// DocumentView is a version as the public sees it.
type DocumentView struct {
	ID                   uuid.UUID  `json:"id"`
	Kind                 string     `json:"kind"`
	Title                string     `json:"title"`
	MarketCode           string     `json:"market_code"`
	Lang                 string     `json:"lang"`
	Version              string     `json:"version"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	Status               string     `json:"status"`
	Body                 *string    `json:"body_markdown,omitempty"`
	Summary              string     `json:"summary_of_changes"`
	RequiresReacceptance bool       `json:"requires_reacceptance"`
	DraftNotice          bool       `json:"draft_notice"`
	PublishedAt          *time.Time `json:"published_at"`
	RetiredAt            *time.Time `json:"retired_at"`
}

// AdminView adds who and when.
type AdminView struct {
	DocumentView
	CreatedBy   PersonRef  `json:"created_by"`
	PublishedBy *PersonRef `json:"published_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Title is the document kind's name in the request's language.
func Title(r *http.Request, kind string) string { return i18n.T(r.Context(), "legal.kind."+kind, nil) }

func documentView(r *http.Request, v Version, withBody bool) DocumentView {
	d := DocumentView{ID: v.ID, Kind: v.Kind, Title: Title(r, v.Kind), MarketCode: v.MarketCode, Lang: v.Lang, Version: v.Label,
		EffectiveFrom: v.EffectiveFrom.UTC(), Status: v.Status, Summary: v.Summary, RequiresReacceptance: v.RequiresReacceptance,
		DraftNotice: v.DraftNotice, PublishedAt: v.PublishedAt, RetiredAt: v.RetiredAt}
	if withBody {
		body := v.Body
		d.Body = &body
	}
	return d
}

func adminView(r *http.Request, v Version, withBody bool) AdminView {
	a := AdminView{DocumentView: documentView(r, v, withBody), CreatedBy: PersonRef{ID: v.CreatedBy, Name: v.CreatedByName},
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
	if v.Status != StatusDraft {
		a.PublishedBy = &PersonRef{ID: v.PublishedBy, Name: v.PublishedByName}
	}
	return a
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteValidation(w, r, verr.Fields)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeDocumentNotFound, i18n.T(ctx, "legal.error.not_found", nil), nil)
	case errors.Is(err, ErrPreviewForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, CodePreviewForbidden, i18n.T(ctx, "legal.error.preview_forbidden", nil), nil)
	case errors.Is(err, ErrMarketForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, CodeMarketForbidden, i18n.T(ctx, "legal.error.market_forbidden", nil), nil)
	case errors.Is(err, ErrImmutable):
		httpx.WriteError(w, r, http.StatusConflict, CodeVersionImmutable, i18n.T(ctx, "legal.error.immutable", nil), nil)
	case errors.Is(err, ErrDuplicate):
		httpx.WriteError(w, r, http.StatusConflict, CodeVersionExists, i18n.T(ctx, "legal.error.exists", nil), nil)
	case errors.Is(err, ErrNotAcceptable):
		httpx.WriteError(w, r, http.StatusConflict, CodeVersionNotAllowed, i18n.T(ctx, "legal.error.not_acceptable", nil), nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

// languageHeaders marks a response that depends on the language.
func languageHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Language", i18n.Lang(r.Context()))
	w.Header().Add("Vary", "Accept-Language")
}

func (h *Handler) market(w http.ResponseWriter, r *http.Request) (string, bool) {
	m, err := h.svc.Market(r.Context(), r.URL.Query().Get("market"), "market")
	if err != nil {
		h.fail(w, r, err)
		return "", false
	}
	return m, true
}

// List handles GET /v1/legal: the current version of every kind.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	languageHeaders(w, r)
	market, ok := h.market(w, r)
	if !ok {
		return
	}
	lang := i18n.Lang(r.Context())
	all, err := h.svc.CurrentAll(r.Context(), market, lang)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	type item struct {
		DocumentView
		FallbackMarket bool `json:"fallback_market"`
		FallbackLang   bool `json:"fallback_lang"`
	}
	out := struct {
		Market    string `json:"market"`
		Lang      string `json:"lang"`
		Documents []item `json:"documents"`
	}{Market: market, Lang: lang, Documents: []item{}}
	for _, res := range all {
		out.Documents = append(out.Documents, item{DocumentView: documentView(r, res.Version, false),
			FallbackMarket: res.FallbackMarket, FallbackLang: res.FallbackLang})
	}
	httpx.WriteCacheableJSON(w, r, out, cacheMaxAge, cacheStale)
}

// Get handles GET /v1/legal/{kind} (?market, ?lang, ?version, ?preview).
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	languageHeaders(w, r)
	kind, err := ParseKind(chi.URLParam(r, "kind"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	market, ok := h.market(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	lang := i18n.Lang(r.Context())
	preview := q.Get("preview") == "true" || q.Get("preview") == "1"
	var res Resolved
	switch {
	case preview:
		res, err = h.svc.Preview(r.Context(), auth.FromContext(r.Context()), kind, market, lang)
	case strings.TrimSpace(q.Get("version")) != "":
		res, err = h.svc.Specific(r.Context(), kind, market, lang, strings.TrimSpace(q.Get("version")))
	default:
		res, err = h.svc.Current(r.Context(), kind, market, lang)
	}
	if preview {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := struct {
		Document        DocumentView `json:"document"`
		RequestedMarket string       `json:"requested_market"`
		RequestedLang   string       `json:"requested_lang"`
		FallbackMarket  bool         `json:"fallback_market"`
		FallbackLang    bool         `json:"fallback_lang"`
		Preview         bool         `json:"preview"`
	}{Document: documentView(r, res.Version, true), RequestedMarket: market, RequestedLang: lang,
		FallbackMarket: res.FallbackMarket, FallbackLang: res.FallbackLang, Preview: preview}
	if preview {
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	httpx.WriteCacheableJSON(w, r, out, cacheMaxAge, cacheStale)
}

// Versions handles GET /v1/legal/{kind}/versions: the published history.
func (h *Handler) Versions(w http.ResponseWriter, r *http.Request) {
	languageHeaders(w, r)
	kind, err := ParseKind(chi.URLParam(r, "kind"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	market, ok := h.market(w, r)
	if !ok {
		return
	}
	lang := i18n.Lang(r.Context())
	list, current, err := h.svc.History(r.Context(), kind, market, lang)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	type item struct {
		DocumentView
		Current bool `json:"current"`
	}
	out := struct {
		Kind     string `json:"kind"`
		Market   string `json:"market"`
		Lang     string `json:"lang"`
		Versions []item `json:"versions"`
	}{Kind: kind, Market: market, Lang: lang, Versions: []item{}}
	for _, v := range list {
		out.Versions = append(out.Versions, item{DocumentView: documentView(r, v, false), Current: v.ID == current})
	}
	httpx.WriteCacheableJSON(w, r, out, cacheMaxAge, cacheStale)
}

// AcceptanceView is one acceptance.
type AcceptanceView struct {
	Kind       string    `json:"kind"`
	Version    string    `json:"version"`
	MarketCode *string   `json:"market_code"`
	AcceptedAt time.Time `json:"accepted_at"`
	Context    *string   `json:"context"`
}

func acceptanceView(a Acceptance) AcceptanceView {
	return AcceptanceView{Kind: a.Kind, Version: a.Version, MarketCode: a.MarketCode, AcceptedAt: a.AcceptedAt.UTC(), Context: a.Context}
}

// PendingView is one document awaiting the person's acceptance (in /v1/me).
type PendingView struct {
	Kind          string    `json:"kind"`
	Title         string    `json:"title"`
	Version       string    `json:"version"`
	MarketCode    string    `json:"market_code"`
	EffectiveFrom time.Time `json:"effective_from"`
	Summary       string    `json:"summary_of_changes"`
}

// PendingViews returns the pending acceptances of the signed-in person for
// /v1/me (?market=, default GLOBAL; an unknown market is read as GLOBAL so
// /v1/me never fails because of it).
func (h *Handler) PendingViews(r *http.Request, userID uuid.UUID) ([]PendingView, error) {
	market, err := h.svc.Market(r.Context(), r.URL.Query().Get("market"), "market")
	if err != nil {
		market = MarketGlobal
	}
	list, err := h.svc.Pending(r.Context(), userID, market)
	if err != nil {
		return nil, err
	}
	out := make([]PendingView, 0, len(list))
	for _, v := range list {
		out = append(out, PendingView{Kind: v.Kind, Title: Title(r, v.Kind), Version: v.Label, MarketCode: v.MarketCode,
			EffectiveFrom: v.EffectiveFrom.UTC(), Summary: v.Summary})
	}
	return out, nil
}

func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, i18n.T(r.Context(), "error.unauthenticated", nil), nil)
	}
	return p
}

// MyAcceptances handles GET /v1/me/legal-acceptances.
func (h *Handler) MyAcceptances(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	list, err := h.svc.MyAcceptances(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := struct {
		Acceptances []AcceptanceView `json:"acceptances"`
	}{Acceptances: make([]AcceptanceView, 0, len(list))}
	for _, a := range list {
		out.Acceptances = append(out.Acceptances, acceptanceView(a))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Accept handles POST /v1/me/legal-acceptances.
func (h *Handler) Accept(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var in AcceptInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	a, created, err := h.svc.Accept(r.Context(), p.UserID, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, map[string]any{"acceptance": acceptanceView(a)})
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, http.StatusNotFound, CodeVersionNotFound, i18n.T(r.Context(), "legal.error.version_not_found", nil), nil)
		return uuid.Nil, false
	}
	return id, true
}

// AdminRoutes mounts /v1/admin/legal (the caller applies the role check;
// writes go through limit).
func (h *Handler) AdminRoutes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/", h.AdminList)
	r.Get("/{id}", h.AdminGet)
	r.Group(func(r chi.Router) {
		r.Use(limit)
		r.Post("/", h.AdminCreate)
		r.Patch("/{id}", h.AdminUpdate)
		r.Delete("/{id}", h.AdminDelete)
		r.Post("/{id}/publish", h.AdminPublish)
		r.Post("/{id}/retire", h.AdminRetire)
	})
}

func (h *Handler) writeVersion(w http.ResponseWriter, r *http.Request, status int, v Version) {
	httpx.WriteJSON(w, status, map[string]any{"version": adminView(r, v, true)})
}

// AdminList handles GET /v1/admin/legal.
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	q := r.URL.Query()
	list, err := h.svc.List(r.Context(), Filter{Kind: q.Get("kind"), Market: q.Get("market"), Lang: q.Get("lang"), Status: q.Get("status")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := struct {
		Versions []AdminView `json:"versions"`
	}{Versions: make([]AdminView, 0, len(list))}
	for _, v := range list {
		out.Versions = append(out.Versions, adminView(r, v, false))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// AdminGet handles GET /v1/admin/legal/{id}.
func (h *Handler) AdminGet(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeVersion(w, r, http.StatusOK, v)
}

// AdminCreate handles POST /v1/admin/legal.
func (h *Handler) AdminCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var in VersionInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.Create(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeVersion(w, r, http.StatusCreated, v)
}

// AdminUpdate handles PATCH /v1/admin/legal/{id}.
func (h *Handler) AdminUpdate(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in VersionInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.Update(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeVersion(w, r, http.StatusOK, v)
}

// AdminDelete handles DELETE /v1/admin/legal/{id}.
func (h *Handler) AdminDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), p, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminPublish handles POST /v1/admin/legal/{id}/publish.
func (h *Handler) AdminPublish(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, h.svc.Publish)
}

// AdminRetire handles POST /v1/admin/legal/{id}/retire.
func (h *Handler) AdminRetire(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, h.svc.Retire)
}

func (h *Handler) transition(w http.ResponseWriter, r *http.Request,
	do func(ctx context.Context, p *auth.Principal, id uuid.UUID) (Version, error)) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	v, err := do(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeVersion(w, r, http.StatusOK, v)
}
