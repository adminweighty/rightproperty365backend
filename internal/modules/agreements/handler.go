package agreements

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Body limits of the routes with large bodies.
const (
	// MaxDocumentBodyBytes: PUT .../document (a 20 MB PDF in multipart).
	MaxDocumentBodyBytes = MaxPDFBytes + 1<<20
	// MaxSubmitBodyBytes: POST /v1/signing/{token}/submit (two images of up
	// to 500 KB in base64, and the field values).
	MaxSubmitBodyBytes = 2 << 20
	// MaxSignatureBodyBytes: PUT /v1/me/signature.
	MaxSignatureBodyBytes = 2 << 20
)

// Handler serves the agreement endpoints.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

var answers = []struct {
	err           error
	status        int
	code, message string
}{
	{ErrForbidden, http.StatusForbidden, httpx.CodeForbidden, "Only people who manage listings (agents, agency owners and managers, approved landlords and sellers) can send documents for signature."},
	{ErrNotFound, http.StatusNotFound, CodeAgreementNotFound, "This agreement does not exist."},
	{ErrState, http.StatusConflict, CodeAgreementState, "The agreement is not in a state that allows this. Reload it and try again."},
	{ErrRecipientNotFound, http.StatusNotFound, CodeRecipientNotFound, "This person is not a recipient of the agreement."},
	{ErrDocumentMissing, http.StatusConflict, CodeDocumentMissing, "Upload the PDF to be signed first."},
	{ErrDocumentInvalid, http.StatusUnsupportedMediaType, CodeDocumentInvalid, "The file must be a PDF that opens without a password."},
	{ErrFileTooLarge, http.StatusRequestEntityTooLarge, CodeFileTooLarge, "The PDF can be at most 20 MB."},
	{ErrLinkNotFound, http.StatusNotFound, CodeLinkNotFound, "This signing link does not exist."},
	{ErrLinkExpired, http.StatusGone, CodeLinkExpired, "This signing link has expired."},
	{ErrLinkNoLongerValid, http.StatusGone, CodeLinkNoLongerValid, "This signing link no longer works: a newer link was sent, or the agreement was cancelled."},
	{ErrNotYourTurn, http.StatusConflict, CodeNotYourTurn, "This document is not waiting for you now."},
	{ErrAlreadyDone, http.StatusConflict, CodeAlreadyDone, "You have already signed, approved or declined this document."},
	{ErrCodeExpired, http.StatusBadRequest, CodeCodeExpired, "That code has expired, was used too many times, or none was sent. Ask for a new code."},
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	if errors.As(err, &verr) {
		code := httpx.CodeValidationFailed
		status := http.StatusBadRequest
		if verr.Code == CodeFieldsIncomplete {
			code, status = CodeFieldsIncomplete, http.StatusUnprocessableEntity
		}
		if verr.Code == CodeDocumentInvalid {
			code, status = CodeDocumentInvalid, http.StatusUnprocessableEntity
		}
		httpx.WriteError(w, r, status, code, "Some fields need your attention.", verr.Fields)
		return
	}
	var later *ResendLaterError
	if errors.As(err, &later) {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(later.After.Seconds())))))
		msg := "Please wait a moment before asking for another code."
		if later.Exhausted {
			msg = "Too many codes were sent for this document. Ask the sender to send it again."
		}
		httpx.WriteError(w, r, http.StatusTooManyRequests, httpx.CodeRateLimited, msg, nil)
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

func (h *Handler) agreement(w http.ResponseWriter, r *http.Request) (*auth.Principal, uuid.UUID, bool) {
	p := principal(w, r)
	if p == nil {
		return nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		if !MaySend(p) {
			h.fail(w, r, ErrForbidden)
		} else {
			h.fail(w, r, ErrNotFound)
		}
		return nil, uuid.Nil, false
	}
	return p, id, true
}

func (h *Handler) write(w http.ResponseWriter, status int, v View) {
	private(w)
	httpx.WriteJSON(w, status, map[string]any{"agreement": v})
}

func paging(r *http.Request, fields map[string]string) (int, int) {
	page, size := 1, DefaultPageSize
	if raw := r.URL.Query().Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 100000 {
			fields["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > MaxPageSize {
			fields["page_size"] = "must be a whole number from 1 to " + strconv.Itoa(MaxPageSize)
		} else {
			size = n
		}
	}
	return page, size
}

func statuses() []string {
	return []string{StatusDraft, StatusOutForSignature, StatusCompleted, StatusDeclined, StatusCancelled, StatusExpired}
}

func statusFilter(r *http.Request, fields map[string]string) string {
	status := r.URL.Query().Get("status")
	if status != "" && !slices.Contains(statuses(), status) {
		fields["status"] = "must be one of " + strings.Join(statuses(), ", ")
	}
	return status
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
		h.logger.WarnContext(r.Context(), "agreement document download interrupted", slog.String("error", err.Error()))
	}
}

// readPDF reads the multipart part "file".
func (h *Handler) readPDF(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	file, header, err := r.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.fail(w, r, ErrFileTooLarge)
		} else {
			h.fail(w, r, &ValidationError{Fields: map[string]string{"file": "send the PDF as the multipart/form-data part \"file\""}})
		}
		return nil, "", false
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, MaxPDFBytes+1))
	if err != nil {
		h.fail(w, r, ErrFileTooLarge)
		return nil, "", false
	}
	if len(content) > MaxPDFBytes {
		h.fail(w, r, ErrFileTooLarge)
		return nil, "", false
	}
	return content, header.Filename, true
}

func decodeOptional(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 {
		return true
	}
	return httpx.DecodeJSON(w, r, dst)
}

// ---------------------------------------------------------------------------
// Senders
// ---------------------------------------------------------------------------

// List serves GET /v1/me/agreements?view=&status=&page=&page_size=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	fields := map[string]string{}
	status := statusFilter(r, fields)
	page, size := paging(r, fields)
	v := r.URL.Query().Get("view")
	if v != "" && !slices.Contains([]string{ViewWaitingForYou, ViewWaitingForOthers, ViewCompleted, ViewDrafts}, v) {
		fields["view"] = "must be WAITING_FOR_YOU, WAITING_FOR_OTHERS, COMPLETED or DRAFTS"
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	out, err := h.svc.List(r.Context(), p, status, v, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Create serves POST /v1/me/agreements.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.Create(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusCreated, v)
}

// Get serves GET /v1/me/agreements/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.agreement(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Get(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, v)
}

// Patch serves PATCH /v1/me/agreements/{id}.
func (h *Handler) Patch(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.agreement(w, r)
	if !ok {
		return
	}
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.Patch(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, v)
}

// Delete serves DELETE /v1/me/agreements/{id}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.agreement(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), p, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Document serves PUT /v1/me/agreements/{id}/document (multipart "file").
func (h *Handler) Document(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.agreement(w, r)
	if !ok {
		return
	}
	content, name, ok := h.readPDF(w, r)
	if !ok {
		return
	}
	v, err := h.svc.AddDocument(r.Context(), p, id, name, content)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, v)
}

// Fields serves PUT /v1/me/agreements/{id}/fields {"fields": [...]}.
func (h *Handler) Fields(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.agreement(w, r)
	if !ok {
		return
	}
	var in struct {
		Fields []FieldInput `json:"fields"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.PutFields(r.Context(), p, id, in.Fields)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, v)
}

// PDF serves GET /v1/me/agreements/{id}/pdf.
func (h *Handler) PDF(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.agreement(w, r)
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

// Send serves POST /v1/me/agreements/{id}/send. No body.
func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.agreement(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Send(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, v)
}

// Cancel serves POST /v1/me/agreements/{id}/cancel {"reason"?}.
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.agreement(w, r)
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
	h.write(w, http.StatusOK, v)
}

func recipientID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "recipientId"))
	return id, err == nil
}

// Replace serves POST /v1/me/agreements/{id}/recipients/{recipientId}/replace.
func (h *Handler) Replace(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.agreement(w, r)
	if !ok {
		return
	}
	rid, ok := recipientID(r)
	if !ok {
		h.fail(w, r, ErrRecipientNotFound)
		return
	}
	var in ReplaceInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.ReplaceRecipient(r.Context(), p, id, rid, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, v)
}

// Remind serves POST /v1/me/agreements/{id}/recipients/{recipientId}/remind.
func (h *Handler) Remind(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.agreement(w, r)
	if !ok {
		return
	}
	rid, ok := recipientID(r)
	if !ok {
		h.fail(w, r, ErrRecipientNotFound)
		return
	}
	v, err := h.svc.Remind(r.Context(), p, id, rid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.write(w, http.StatusOK, v)
}

// Events serves GET /v1/me/agreements/{id}/events.
func (h *Handler) Events(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.agreement(w, r)
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

// AdminList serves GET /v1/admin/agreements?status=&page=&page_size=.
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
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
// Signed-in recipients
// ---------------------------------------------------------------------------

// SigningRequests serves GET /v1/me/signing-requests.
func (h *Handler) SigningRequests(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	items, err := h.svc.SigningRequests(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// OwnLink serves POST /v1/me/signing-requests/{recipientId}/link. No body.
func (h *Handler) OwnLink(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	rid, ok := recipientID(r)
	if !ok {
		h.fail(w, r, ErrRecipientNotFound)
		return
	}
	out, err := h.svc.OwnLink(r.Context(), p, rid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// MySignature serves GET /v1/me/signature.
func (h *Handler) MySignature(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	out, err := h.svc.MySignature(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// SaveMySignature serves PUT /v1/me/signature.
func (h *Handler) SaveMySignature(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var in SaveSignatureInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.SaveMySignature(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Signing by link, and verification (no session)
// ---------------------------------------------------------------------------

// Signing serves GET /v1/signing/{token}.
func (h *Handler) Signing(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Signing(r.Context(), chi.URLParam(r, "token"), httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"signing": v})
}

// SigningPDF serves GET /v1/signing/{token}/pdf.
func (h *Handler) SigningPDF(w http.ResponseWriter, r *http.Request) {
	f, err := h.svc.SigningPDF(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.stream(w, r, f)
}

// SendCode serves POST /v1/signing/{token}/code. No body.
func (h *Handler) SendCode(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.SendCode(r.Context(), chi.URLParam(r, "token"), httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Submit serves POST /v1/signing/{token}/submit.
func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	var in SubmitInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Submit(r.Context(), chi.URLParam(r, "token"), identity.ParseCodeInput(in.Code), in, httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Decline serves POST /v1/signing/{token}/decline {"reason"}.
func (h *Handler) Decline(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.Decline(r.Context(), chi.URLParam(r, "token"), in.Reason, httpx.ClientIP(r), r.UserAgent())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"signing": v})
}

// VerifyReference serves GET /v1/verify/{reference}.
func (h *Handler) VerifyReference(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.VerifyReference(r.Context(), chi.URLParam(r, "reference"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, v)
}

// VerifyFile serves POST /v1/verify (multipart "file").
func (h *Handler) VerifyFile(w http.ResponseWriter, r *http.Request) {
	content, _, ok := h.readPDF(w, r)
	if !ok {
		return
	}
	v, err := h.svc.VerifyFile(r.Context(), content)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, v)
}
