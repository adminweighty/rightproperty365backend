package leases

import (
	"errors"
	"fmt"
	"io"
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
)

// Handler serves the lease endpoints.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

type answer struct {
	err           error
	status        int
	code, message string
}

var answers = []answer{
	{ErrForbidden, http.StatusForbidden, httpx.CodeForbidden, "Only people who manage listings (agents, agency owners and managers, approved landlords and sellers) can prepare leases."},
	{ErrTemplateNotFound, http.StatusNotFound, CodeTemplateNotFound, "This lease template does not exist."},
	{ErrTemplateNotEditable, http.StatusConflict, CodeTemplateNotEditable, "A retired template cannot be edited. Edit its newest version instead."},
	{ErrTemplateDraftExists, http.StatusConflict, CodeTemplateDraftExists, "This template already has a draft version. Edit that draft instead."},
	{ErrTemplateState, http.StatusConflict, CodeTemplateState, "The template is not in a state that allows this (only a draft can be approved, only an approved template retired)."},
	{ErrTemplateUnavailable, http.StatusUnprocessableEntity, CodeTemplateNotAvailable, "This template cannot be used: only an approved template of the property's country can be used for a new lease."},
	{ErrLeaseNotFound, http.StatusNotFound, CodeLeaseNotFound, "This lease does not exist."},
	{ErrLeaseNotEditable, http.StatusConflict, CodeLeaseNotEditable, "Only a draft lease can be changed or deleted. Cancel it and prepare a new one."},
	{ErrLeaseState, http.StatusConflict, CodeLeaseState, "The lease changed or is not in a state that allows this. Reload it and try again."},
	{ErrListingNotFound, http.StatusNotFound, CodeListingNotFound, "This listing does not exist or is not one you manage."},
	{ErrMarketMismatch, http.StatusUnprocessableEntity, CodeMarketMismatch, "The template is for another country than the property. Choose a template of the property's country."},
	{ErrDocumentUnavailable, http.StatusNotFound, CodeDocumentUnavailable, "The document is not available."},
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	if errors.As(err, &verr) {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
		return
	}
	for _, a := range answers {
		if errors.Is(err, a.err) {
			httpx.WriteError(w, r, a.status, a.code, a.message, nil)
			return
		}
	}
	httpx.WriteInternalError(w, r, h.logger, err)
}

func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

func pathID(r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	return id, err == nil
}

// paging reads ?page= and ?page_size=.
func paging(r *http.Request, fields map[string]string) (int, int) {
	page, size := 1, DefaultPageSize
	if raw := r.URL.Query().Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100000 {
			fields["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxPageSize {
			fields["page_size"] = "must be a whole number from 1 to " + strconv.Itoa(MaxPageSize)
		} else {
			size = n
		}
	}
	return page, size
}

func leaseStatuses() []string {
	return []string{StatusDraft, StatusSentForSigning, StatusSigned, StatusDeclined, StatusCancelled}
}

func statusFilter(r *http.Request, fields map[string]string) string {
	status := r.URL.Query().Get("status")
	if status != "" && !slices.Contains(leaseStatuses(), status) {
		fields["status"] = "must be one of " + strings.Join(leaseStatuses(), ", ")
	}
	return status
}

// decodeOptional decodes a JSON body when one was sent (a body is optional).
func decodeOptional(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 {
		return true
	}
	return httpx.DecodeJSON(w, r, dst)
}

func (h *Handler) stream(w http.ResponseWriter, r *http.Request, f File) {
	defer f.Content.Close()
	head := w.Header()
	head.Set("Content-Type", "application/pdf")
	if f.Size > 0 {
		head.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	}
	head.Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"; filename*=UTF-8''%s`, f.FileName, url.PathEscape(f.FileName)))
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Cache-Control", "private, no-store")
	head.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f.Content); err != nil {
		h.logger.WarnContext(r.Context(), "lease document download interrupted", slog.String("error", err.Error()))
	}
}

// ---------------------------------------------------------------------------
// Administrators: the template library and lease oversight
// ---------------------------------------------------------------------------

func templateStatuses() []string { return []string{TemplateDraft, TemplateApproved, TemplateRetired} }

// AdminTemplates serves GET /v1/admin/lease-templates?market=&status=.
func (h *Handler) AdminTemplates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := TemplateFilter{MarketCode: strings.ToUpper(q.Get("market")), Status: q.Get("status")}
	if f.Status != "" && !slices.Contains(templateStatuses(), f.Status) {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.",
			map[string]string{"status": "must be one of DRAFT, APPROVED, RETIRED"})
		return
	}
	items, err := h.svc.AdminTemplates(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "variables": Variables()})
}

// AdminCreateTemplate serves POST /v1/admin/lease-templates.
func (h *Handler) AdminCreateTemplate(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var in TemplateInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	t, err := h.svc.CreateTemplate(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"template": t})
}

// AdminTemplate serves GET /v1/admin/lease-templates/{id}.
func (h *Handler) AdminTemplate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		h.fail(w, r, ErrTemplateNotFound)
		return
	}
	t, events, err := h.svc.AdminTemplate(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"template": t, "events": events, "variables": Variables()})
}

// AdminEditTemplate serves PATCH /v1/admin/lease-templates/{id}: 200 with
// the edited draft, or 201 with the new draft version of an approved one.
func (h *Handler) AdminEditTemplate(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r, "id")
	if !ok {
		h.fail(w, r, ErrTemplateNotFound)
		return
	}
	var in TemplateInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	t, created, err := h.svc.EditTemplate(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	private(w)
	httpx.WriteJSON(w, status, map[string]any{"template": t, "new_version": created})
}

// AdminApproveTemplate serves POST /v1/admin/lease-templates/{id}/approve.
func (h *Handler) AdminApproveTemplate(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r, "id")
	if !ok {
		h.fail(w, r, ErrTemplateNotFound)
		return
	}
	var in struct {
		LegalReview *LegalReview `json:"legal_review"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if in.LegalReview == nil {
		h.fail(w, r, &ValidationError{Fields: map[string]string{"legal_review": "is required: who reviewed the template, for which organisation, and when"}})
		return
	}
	t, err := h.svc.ApproveTemplate(r.Context(), p, id, *in.LegalReview)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"template": t})
}

// AdminRetireTemplate serves POST /v1/admin/lease-templates/{id}/retire. No body.
func (h *Handler) AdminRetireTemplate(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := pathID(r, "id")
	if !ok {
		h.fail(w, r, ErrTemplateNotFound)
		return
	}
	t, err := h.svc.RetireTemplate(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"template": t})
}

// AdminLeases serves GET /v1/admin/leases?status=&page=&page_size=.
func (h *Handler) AdminLeases(w http.ResponseWriter, r *http.Request) {
	fields := map[string]string{}
	status := statusFilter(r, fields)
	page, size := paging(r, fields)
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	out, err := h.svc.AdminList(r.Context(), status, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Advertisers: templates on offer, drafting, sending
// ---------------------------------------------------------------------------

// OfferedTemplates serves GET /v1/me/lease-templates?market=.
func (h *Handler) OfferedTemplates(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	items, err := h.svc.OfferedTemplates(r.Context(), p, r.URL.Query().Get("market"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "variables": Variables()})
}

// List serves GET /v1/me/leases?status=&page=&page_size=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	fields := map[string]string{}
	status := statusFilter(r, fields)
	page, size := paging(r, fields)
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	out, err := h.svc.List(r.Context(), p, status, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) writeLease(w http.ResponseWriter, status int, v LeaseView) {
	private(w)
	httpx.WriteJSON(w, status, map[string]any{"lease": v})
}

// Create serves POST /v1/me/leases.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var in LeaseInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.Create(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeLease(w, http.StatusCreated, v)
}

func (h *Handler) lease(w http.ResponseWriter, r *http.Request) (*auth.Principal, uuid.UUID, bool) {
	p := principal(w, r)
	if p == nil {
		return nil, uuid.Nil, false
	}
	id, ok := pathID(r, "id")
	if !ok {
		if !MayCreateLeases(p) {
			h.fail(w, r, ErrForbidden)
		} else {
			h.fail(w, r, ErrLeaseNotFound)
		}
		return nil, uuid.Nil, false
	}
	return p, id, true
}

// Get serves GET /v1/me/leases/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.lease(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Get(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeLease(w, http.StatusOK, v)
}

// Patch serves PATCH /v1/me/leases/{id}.
func (h *Handler) Patch(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.lease(w, r)
	if !ok {
		return
	}
	var in LeaseInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.Patch(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeLease(w, http.StatusOK, v)
}

// Delete serves DELETE /v1/me/leases/{id}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.lease(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), p, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PDF serves GET /v1/me/leases/{id}/pdf.
func (h *Handler) PDF(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.lease(w, r)
	if !ok {
		return
	}
	f, err := h.svc.PDF(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.stream(w, r, f)
}

// Send serves POST /v1/me/leases/{id}/send {message?, expires_at?, reminder_frequency?}.
func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.lease(w, r)
	if !ok {
		return
	}
	var in SendInput
	if !decodeOptional(w, r, &in) {
		return
	}
	v, err := h.svc.Send(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeLease(w, http.StatusOK, v)
}

// Cancel serves POST /v1/me/leases/{id}/cancel {"reason"?}.
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.lease(w, r)
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !decodeOptional(w, r, &in) {
		return
	}
	v, err := h.svc.Cancel(r.Context(), p, id, in.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeLease(w, http.StatusOK, v)
}

// Events serves GET /v1/me/leases/{id}/events.
func (h *Handler) Events(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.lease(w, r)
	if !ok {
		return
	}
	items, err := h.svc.Events(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
