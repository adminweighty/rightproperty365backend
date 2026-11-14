package onboarding

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// MaxUploadBodyBytes is the request body limit of the upload route: the file
// plus room for the multipart framing and the "type" field.
const MaxUploadBodyBytes = MaxDocumentBytes + 64<<10

// Cache lifetimes of GET /v1/onboarding/requirements (as the reference data).
const (
	requirementsMaxAge = 5 * time.Minute
	requirementsStale  = time.Hour
)

// Handler serves the onboarding routes.
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
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrForbiddenRole), errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You do not have permission to do this.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotFound, "This onboarding application does not exist.", nil)
	case errors.Is(err, ErrDocumentNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeDocumentNotFound, "This document does not exist.", nil)
	case errors.Is(err, ErrInvalidState):
		httpx.WriteError(w, r, http.StatusConflict, CodeInvalidState, "The application is not in a state that allows this.", nil)
	case errors.Is(err, ErrAgencyNotVerified):
		httpx.WriteError(w, r, http.StatusConflict, CodeAgencyNotVerified, "The agency this person wants to join is no longer a verified agency.", nil)
	case errors.Is(err, ErrTooManyDocuments):
		httpx.WriteError(w, r, http.StatusConflict, CodeTooManyDocuments, fmt.Sprintf("An application can hold at most %d documents.", MaxDocumentsPerApp), nil)
	case errors.Is(err, ErrFileTooLarge), errors.As(err, &tooLarge):
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, CodeFileTooLarge, "The file is larger than 10 MB.", nil)
	case errors.Is(err, ErrLogoTooLarge):
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, CodeFileTooLarge, "A logo must be at most 2 MB.", nil)
	case errors.Is(err, ErrUnsupportedLogo):
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedFile, "A logo must be a PNG or JPEG image.", nil)
	case errors.Is(err, ErrUnsupportedFile):
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedFile, "Only PDF, JPEG and PNG files are accepted.", nil)
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

type requirementsResponse struct {
	Roles []RoleRequirements `json:"roles"`
}

// Requirements handles GET /v1/onboarding/requirements. Public and cacheable.
func (h *Handler) Requirements(w http.ResponseWriter, r *http.Request) {
	roles, err := h.svc.Requirements(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, requirementsResponse{Roles: roles}, requirementsMaxAge, requirementsStale)
}

type mineResponse struct {
	Applications []ApplicationView `json:"applications"`
}

type applicationResponse struct {
	Application ApplicationView `json:"application"`
}

// Mine handles GET /v1/me/onboarding.
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	apps, err := h.svc.Mine(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, mineResponse{Applications: apps})
}

// private marks a response as personal: never cached by anything shared.
func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

// Save handles PUT /v1/me/onboarding/{role}.
func (h *Handler) Save(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var patch map[string]json.RawMessage
	if !httpx.DecodeJSON(w, r, &patch) {
		return
	}
	if patch == nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "The request body must be a JSON object.", nil)
		return
	}
	app, err := h.svc.Save(r.Context(), p, chi.URLParam(r, "role"), patch)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, applicationResponse{Application: app})
}

// Submit handles POST /v1/me/onboarding/{role}/submit.
func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	app, err := h.svc.Submit(r.Context(), p, chi.URLParam(r, "role"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, applicationResponse{Application: app})
}

type documentResponse struct {
	Document DocumentView `json:"document"`
}

// readUpload reads the multipart form: a "type" field and one "file" part,
// in either order. The file is read into memory, at most MaxDocumentBytes+1
// bytes of it, so an oversized file is detected without being stored.
func readUpload(r *http.Request) (typeCode, fileName string, content []byte, err error) {
	mediaType, params, perr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if perr != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return "", "", nil, &ValidationError{Fields: map[string]string{"file": "the request must be multipart/form-data with the fields type and file"}}
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	seenFile := false
	for parts := 0; ; parts++ {
		part, perr := reader.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(perr, &tooLarge) {
				return "", "", nil, ErrFileTooLarge
			}
			return "", "", nil, &ValidationError{Fields: map[string]string{"file": "the multipart body could not be read"}}
		}
		if parts >= 4 {
			return "", "", nil, &ValidationError{Fields: map[string]string{"file": "send exactly one type field and one file"}}
		}
		switch part.FormName() {
		case "type":
			raw, rerr := io.ReadAll(io.LimitReader(part, 200))
			if rerr != nil {
				return "", "", nil, rerr
			}
			typeCode = strings.TrimSpace(string(raw))
		case "file":
			if seenFile {
				return "", "", nil, &ValidationError{Fields: map[string]string{"file": "send exactly one file"}}
			}
			seenFile = true
			fileName = part.FileName()
			content, err = io.ReadAll(io.LimitReader(part, MaxDocumentBytes+1))
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					return "", "", nil, ErrFileTooLarge
				}
				return "", "", nil, &ValidationError{Fields: map[string]string{"file": "the file could not be read"}}
			}
		}
		_ = part.Close()
	}
	return typeCode, fileName, content, nil
}

// Upload handles POST /v1/me/onboarding/{role}/documents (multipart).
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	typeCode, fileName, content, err := readUpload(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	doc, err := h.svc.Upload(r.Context(), p, chi.URLParam(r, "role"), typeCode, fileName, content)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusCreated, documentResponse{Document: doc})
}

func documentID(r *http.Request, param string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		return uuid.Nil, ErrDocumentNotFound
	}
	return id, nil
}

// DeleteDocument handles DELETE /v1/me/onboarding/{role}/documents/{id}.
func (h *Handler) DeleteDocument(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, err := documentID(r, "id")
	if err == nil {
		err = h.svc.DeleteDocument(r.Context(), p, chi.URLParam(r, "role"), id)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serve streams a private file. It is never cacheable, never sniffed, and
// always a download.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, f File) {
	defer f.Content.Close()
	ascii := strings.Map(func(c rune) rune {
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return '_'
		}
		return c
	}, f.Document.FileName)
	head := w.Header()
	head.Set("Content-Type", f.Document.ContentType)
	head.Set("Content-Length", strconv.FormatInt(f.Document.Size, 10))
	head.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(f.Document.FileName)))
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Cache-Control", "private, no-store")
	head.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f.Content); err != nil {
		h.logger.WarnContext(r.Context(), "onboarding document download interrupted", slog.String("document_id", f.Document.ID.String()))
	}
}

// File handles GET /v1/me/onboarding/{role}/documents/{id}/file.
func (h *Handler) File(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, err := documentID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	f, err := h.svc.OpenDocument(r.Context(), p, chi.URLParam(r, "role"), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.serve(w, r, f)
}

// ---------------------------------------------------------------------------
// Administrator routes (behind auth.RequirePlatformRole(AdminRoles()...))
// ---------------------------------------------------------------------------

func applicationID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

// AdminList handles GET /v1/admin/onboarding.
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	v := r.URL.Query()
	fields := map[string]string{}
	number := func(name string, def, max int) int {
		raw := v.Get(name)
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > max {
			fields[name] = fmt.Sprintf("must be a whole number between 1 and %d", max)
			return def
		}
		return n
	}
	page, size := number("page", 1, 100000), number("page_size", 20, 100)
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	out, err := h.svc.List(r.Context(), p, v.Get("status"), v.Get("role"), v.Get("q"), page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

type adminApplicationResponse struct {
	Application AdminApplication `json:"application"`
}

// AdminGet handles GET /v1/admin/onboarding/{id}.
func (h *Handler) AdminGet(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, err := applicationID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	app, err := h.svc.AdminGet(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, adminApplicationResponse{Application: app})
}

// AdminFile handles GET /v1/admin/onboarding/{id}/documents/{docId}/file.
func (h *Handler) AdminFile(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, err := applicationID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	docID, err := documentID(r, "docId")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	f, err := h.svc.AdminOpenDocument(r.Context(), p, id, docID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.serve(w, r, f)
}

type approveRequest struct {
	Note string `json:"note"`
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

// AdminApprove handles POST /v1/admin/onboarding/{id}/approve.
func (h *Handler) AdminApprove(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, err := applicationID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req approveRequest
	if r.ContentLength != 0 && !httpx.DecodeJSON(w, r, &req) {
		return
	}
	out, err := h.svc.Approve(r.Context(), p, id, req.Note)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) decide(w http.ResponseWriter, r *http.Request, do func(p *auth.Principal, id uuid.UUID, reason string) (AdminApplication, error)) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, err := applicationID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req reasonRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	app, err := do(p, id, req.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, adminApplicationResponse{Application: app})
}

// AdminReject handles POST /v1/admin/onboarding/{id}/reject.
func (h *Handler) AdminReject(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, func(p *auth.Principal, id uuid.UUID, reason string) (AdminApplication, error) {
		return h.svc.Reject(r.Context(), p, id, reason)
	})
}

// AdminRequestChanges handles POST /v1/admin/onboarding/{id}/request-changes.
func (h *Handler) AdminRequestChanges(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, func(p *auth.Principal, id uuid.UUID, reason string) (AdminApplication, error) {
		return h.svc.RequestChanges(r.Context(), p, id, reason)
	})
}
