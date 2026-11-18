package applications

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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/logging"
)

// Error codes.
const (
	CodeFormNotFound            = "FORM_NOT_FOUND"
	CodeFormNotEditable         = "FORM_NOT_EDITABLE"
	CodeFormDraftExists         = "FORM_DRAFT_EXISTS"
	CodeFormState               = "FORM_STATE"
	CodeApplicationNotFound     = "APPLICATION_NOT_FOUND"
	CodeApplicationFormNotFound = "APPLICATION_FORM_NOT_FOUND"
	CodeListingNotFound         = "LISTING_NOT_FOUND"
	CodeListingNotEligible      = "LISTING_NOT_ELIGIBLE"
	CodeFileNotFound            = "FILE_NOT_FOUND"
	CodeOrganisationNotFound    = "ORGANISATION_NOT_FOUND"
	CodeApplicationsClosed      = "APPLICATIONS_CLOSED"
	CodeInvalidTransition       = "INVALID_APPLICATION_TRANSITION"
	CodeNotEditable             = "APPLICATION_NOT_EDITABLE"
	CodeConflict                = "APPLICATION_CONFLICT"
	CodeTooManyFiles            = "TOO_MANY_FILES"
	CodeUnsupportedFileType     = "UNSUPPORTED_FILE_TYPE"
	CodeFileTooLarge            = "FILE_TOO_LARGE"
)

// Handler serves the questionnaire and application endpoints.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func private(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
}

// detailBody is the error envelope with error.details.
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

var answers = []struct {
	err           error
	status        int
	code, message string
}{
	{ErrUnauthenticated, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this."},
	{ErrForbidden, http.StatusForbidden, httpx.CodeForbidden, "Your role does not allow this."},
	{ErrFormNotFound, http.StatusNotFound, CodeFormNotFound, "This form does not exist."},
	{ErrFormNotEditable, http.StatusConflict, CodeFormNotEditable, "This form version cannot be changed. Edit its newest version instead."},
	{ErrFormLocked, http.StatusConflict, CodeFormNotEditable, "This form version has applications and can never change."},
	{ErrDraftExists, http.StatusConflict, CodeFormDraftExists, "This form already has a draft version. Edit that draft instead."},
	{ErrFormState, http.StatusConflict, CodeFormState, "The form is not in a state that allows this (only a draft can be published, a retired form cannot be retired again)."},
	{ErrListingNotFound, http.StatusNotFound, CodeListingNotFound, "This listing does not exist."},
	{ErrListingNotEligible, http.StatusBadRequest, CodeListingNotEligible, "Short stays do not take applications: only sale, rent and commercial listings do."},
	{ErrFormUnavailable, http.StatusNotFound, CodeApplicationFormNotFound, "This listing does not take applications."},
	{ErrNotFound, http.StatusNotFound, CodeApplicationNotFound, "This application does not exist."},
	{ErrNotEditable, http.StatusConflict, CodeNotEditable, "This application can no longer be changed."},
	{ErrConflict, http.StatusConflict, CodeConflict, "The application was changed at the same time. Reload it and try again."},
	{ErrFileNotFound, http.StatusNotFound, CodeFileNotFound, "This file does not exist."},
	{ErrFileTooLarge, http.StatusRequestEntityTooLarge, CodeFileTooLarge, fmt.Sprintf("A file may be at most %d MB.", MaxFileBytes>>20)},
	{ErrOrganisationUnknown, http.StatusNotFound, CodeOrganisationNotFound, "This organisation does not exist or you cannot see its applications."},
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	var closed *ClosedError
	var trans *TransitionError
	var unsupported *UnsupportedFileError
	var tooMany *TooManyFilesError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
		return
	case errors.As(err, &closed):
		writeDetails(w, r, http.StatusConflict, CodeApplicationsClosed, ReasonMessage(closed.Reason),
			map[string]any{"reason": closed.Reason, "closes_at": closed.ClosesAt})
		return
	case errors.As(err, &trans):
		allowed := trans.Allowed
		if allowed == nil {
			allowed = []string{}
		}
		writeDetails(w, r, http.StatusConflict, CodeInvalidTransition,
			fmt.Sprintf("An application that is %s cannot be moved to %s.", StatusLabel(trans.From), StatusLabel(trans.To)),
			map[string]any{"from": trans.From, "to": trans.To, "allowed": allowed})
		return
	case errors.As(err, &unsupported):
		httpx.WriteError(w, r, http.StatusBadRequest, CodeUnsupportedFileType,
			"This type of file is not accepted here. Accepted: "+strings.ToUpper(strings.Join(unsupported.Accepted, ", "))+".", nil)
		return
	case errors.As(err, &tooMany):
		httpx.WriteError(w, r, http.StatusConflict, CodeTooManyFiles, fmt.Sprintf("This question takes at most %d files. Remove one first.", tooMany.Max), nil)
		return
	case errors.As(err, &tooLarge):
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, CodeFileTooLarge, fmt.Sprintf("A file may be at most %d MB.", MaxFileBytes>>20), nil)
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

func pathID(r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	return id, err == nil
}

// paging reads ?page= and ?page_size=.
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

func statusParam(r *http.Request, fields map[string]string) string {
	st := r.URL.Query().Get("status")
	if st != "" && !slices.Contains(Statuses(), st) {
		fields["status"] = "must be one of " + strings.Join(Statuses(), ", ")
	}
	return st
}

// ---------------------------------------------------------------------------
// Forms: /v1/agent/organisations/{id}/forms
// ---------------------------------------------------------------------------

// formBody is FormInput as sent: expires_at may be null (clear it).
type formBody struct {
	Name          *string         `json:"name"`
	MarketCode    *string         `json:"market_code"`
	Purpose       *string         `json:"purpose"`
	PrivacyNotice *string         `json:"privacy_notice"`
	RetentionDays *int            `json:"retention_days"`
	ExpiresAt     json.RawMessage `json:"expires_at"`
	Sections      *[]Section      `json:"sections"`
}

func (b formBody) input() (FormInput, error) {
	in := FormInput{Name: b.Name, MarketCode: b.MarketCode, Purpose: b.Purpose, PrivacyNotice: b.PrivacyNotice, RetentionDays: b.RetentionDays, Sections: b.Sections}
	switch raw := strings.TrimSpace(string(b.ExpiresAt)); raw {
	case "":
	case "null":
		in.ClearExpiry = true
	default:
		var t time.Time
		if err := json.Unmarshal(b.ExpiresAt, &t); err != nil {
			return FormInput{}, &ValidationError{Fields: map[string]string{"expires_at": "must be a date and time (RFC 3339) or null"}}
		}
		in.ExpiresAt = &t
	}
	return in, nil
}

func (h *Handler) formPath(w http.ResponseWriter, r *http.Request, withID bool) (*auth.Principal, uuid.UUID, uuid.UUID, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		h.fail(w, r, ErrUnauthenticated)
		return nil, uuid.Nil, uuid.Nil, false
	}
	book, ok := pathID(r, "id")
	if !ok {
		h.fail(w, r, ErrFormNotFound)
		return nil, uuid.Nil, uuid.Nil, false
	}
	var id uuid.UUID
	if withID {
		if id, ok = pathID(r, "formId"); !ok {
			h.fail(w, r, ErrFormNotFound)
			return nil, uuid.Nil, uuid.Nil, false
		}
	}
	return p, book, id, true
}

// ListForms serves GET .../forms?status=&market=&family_id=.
func (h *Handler) ListForms(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.formPath(w, r, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	fields := map[string]string{}
	status := q.Get("status")
	if status != "" && !slices.Contains([]string{FormDraft, FormPublished, FormRetired}, status) {
		fields["status"] = "must be DRAFT, PUBLISHED or RETIRED"
	}
	var family *uuid.UUID
	if raw := q.Get("family_id"); raw != "" {
		if id, err := uuid.Parse(raw); err != nil {
			fields["family_id"] = "must be a form family ID"
		} else {
			family = &id
		}
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	items, err := h.svc.ListForms(r.Context(), p, book, status, strings.ToUpper(q.Get("market")), family)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// CreateForm serves POST .../forms.
func (h *Handler) CreateForm(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.formPath(w, r, false)
	if !ok {
		return
	}
	var body formBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	in, err := body.input()
	if err == nil {
		var f FormView
		if f, err = h.svc.CreateForm(r.Context(), p, book, in); err == nil {
			private(w)
			httpx.WriteJSON(w, http.StatusCreated, map[string]any{"form": f})
			return
		}
	}
	h.fail(w, r, err)
}

// GetForm serves GET .../forms/{formId}.
func (h *Handler) GetForm(w http.ResponseWriter, r *http.Request) {
	p, book, id, ok := h.formPath(w, r, true)
	if !ok {
		return
	}
	f, versions, err := h.svc.GetForm(r.Context(), p, book, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"form": f, "versions": versions})
}

// EditForm serves PATCH .../forms/{formId}: 200 with the edited draft, or
// 201 with the new draft version of a published form.
func (h *Handler) EditForm(w http.ResponseWriter, r *http.Request) {
	p, book, id, ok := h.formPath(w, r, true)
	if !ok {
		return
	}
	var body formBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	in, err := body.input()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	f, created, err := h.svc.EditForm(r.Context(), p, book, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	private(w)
	httpx.WriteJSON(w, status, map[string]any{"form": f, "new_version": created})
}

// PublishForm serves POST .../forms/{formId}/publish.
func (h *Handler) PublishForm(w http.ResponseWriter, r *http.Request) {
	p, book, id, ok := h.formPath(w, r, true)
	if !ok {
		return
	}
	f, err := h.svc.PublishForm(r.Context(), p, book, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"form": f})
}

// RetireForm serves POST .../forms/{formId}/retire.
func (h *Handler) RetireForm(w http.ResponseWriter, r *http.Request) {
	p, book, id, ok := h.formPath(w, r, true)
	if !ok {
		return
	}
	f, err := h.svc.RetireForm(r.Context(), p, book, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"form": f})
}

// ---------------------------------------------------------------------------
// Intake and the public form
// ---------------------------------------------------------------------------

// GetIntake serves GET /v1/me/listings/{id}/applications-intake.
func (h *Handler) GetIntake(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		h.fail(w, r, ErrListingNotFound)
		return
	}
	v, err := h.svc.GetIntake(r.Context(), auth.FromContext(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, v)
}

// PutIntake serves PUT /v1/me/listings/{id}/applications-intake.
func (h *Handler) PutIntake(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		h.fail(w, r, ErrListingNotFound)
		return
	}
	var in IntakeInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.PutIntake(r.Context(), auth.FromContext(r.Context()), id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, v)
}

// PublicForm serves GET /v1/listings/{slug}/application-form (public).
func (h *Handler) PublicForm(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" || len(slug) > 200 {
		h.fail(w, r, ErrListingNotFound)
		return
	}
	f, intake, err := h.svc.PublicForm(r.Context(), slug)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"form": f, "intake": intake})
}

// ---------------------------------------------------------------------------
// The applicant: /v1/me/applications
// ---------------------------------------------------------------------------

func (h *Handler) mineID(w http.ResponseWriter, r *http.Request) (*auth.Principal, uuid.UUID, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		h.fail(w, r, ErrUnauthenticated)
		return nil, uuid.Nil, false
	}
	id, ok := pathID(r, "applicationId")
	if !ok {
		h.fail(w, r, ErrNotFound)
		return nil, uuid.Nil, false
	}
	return p, id, true
}

func (h *Handler) writeApplication(w http.ResponseWriter, status int, v any) {
	private(w)
	httpx.WriteJSON(w, status, map[string]any{"application": v})
}

// Mine serves GET /v1/me/applications?status=&page=&page_size=.
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	fields := map[string]string{}
	status := statusParam(r, fields)
	page, size := paging(r, fields)
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	out, err := h.svc.Mine(r.Context(), auth.FromContext(r.Context()), status, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Start serves POST /v1/me/applications {listing_id}.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ListingID string `json:"listing_id"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	id, err := uuid.Parse(body.ListingID)
	if err != nil {
		h.fail(w, r, &ValidationError{Fields: map[string]string{"listing_id": "must be a listing ID"}})
		return
	}
	v, created, err := h.svc.Start(r.Context(), auth.FromContext(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	private(w)
	httpx.WriteJSON(w, status, map[string]any{"application": v, "created": created})
}

// MineOne serves GET /v1/me/applications/{applicationId}.
func (h *Handler) MineOne(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.mineID(w, r)
	if !ok {
		return
	}
	v, err := h.svc.MineOne(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeApplication(w, http.StatusOK, v)
}

// Autosave serves PATCH /v1/me/applications/{applicationId} {answers}.
func (h *Handler) Autosave(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.mineID(w, r)
	if !ok {
		return
	}
	var body struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if body.Answers == nil {
		h.fail(w, r, &ValidationError{Fields: map[string]string{"answers": "is required: an object of field keys"}})
		return
	}
	v, err := h.svc.Autosave(r.Context(), p, id, body.Answers)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeApplication(w, http.StatusOK, v)
}

// readFile reads the one "file" part of a multipart upload.
func readFile(r *http.Request) (string, []byte, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return "", nil, &ValidationError{Fields: map[string]string{"file": "the request must be multipart/form-data with one file part named file"}}
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	var name string
	var content []byte
	seen := false
	for parts := 0; ; parts++ {
		part, perr := reader.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(perr, &tooLarge) {
				return "", nil, ErrFileTooLarge
			}
			return "", nil, &ValidationError{Fields: map[string]string{"file": "the multipart body could not be read"}}
		}
		if parts >= 3 {
			return "", nil, &ValidationError{Fields: map[string]string{"file": "send exactly one file"}}
		}
		if part.FormName() == "file" {
			if seen {
				return "", nil, &ValidationError{Fields: map[string]string{"file": "send exactly one file"}}
			}
			seen = true
			name = part.FileName()
			content, err = io.ReadAll(io.LimitReader(part, MaxFileBytes+1))
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					return "", nil, ErrFileTooLarge
				}
				return "", nil, &ValidationError{Fields: map[string]string{"file": "the file could not be read"}}
			}
		}
		_ = part.Close()
	}
	if !seen {
		return "", nil, &ValidationError{Fields: map[string]string{"file": "is required"}}
	}
	return name, content, nil
}

// Upload serves POST /v1/me/applications/{applicationId}/files?field=key (multipart).
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.mineID(w, r)
	if !ok {
		return
	}
	name, content, err := readFile(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	f, v, err := h.svc.Upload(r.Context(), p, id, r.URL.Query().Get("field"), name, content)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"file_id": f.ID, "file": f, "application": v})
}

// MyFile serves GET /v1/me/applications/{applicationId}/files/{fileId}.
func (h *Handler) MyFile(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.mineID(w, r)
	if !ok {
		return
	}
	fileID, ok := pathID(r, "fileId")
	if !ok {
		h.fail(w, r, ErrFileNotFound)
		return
	}
	f, err := h.svc.OpenFile(r.Context(), p, id, fileID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.serve(w, r, f)
}

// DeleteMyFile serves DELETE /v1/me/applications/{applicationId}/files/{fileId}.
func (h *Handler) DeleteMyFile(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.mineID(w, r)
	if !ok {
		return
	}
	fileID, ok := pathID(r, "fileId")
	if !ok {
		h.fail(w, r, ErrFileNotFound)
		return
	}
	v, err := h.svc.DeleteFile(r.Context(), p, id, fileID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeApplication(w, http.StatusOK, v)
}

// Submit serves POST /v1/me/applications/{applicationId}/submit.
func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.mineID(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Submit(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeApplication(w, http.StatusOK, v)
}

// Withdraw serves POST /v1/me/applications/{applicationId}/withdraw.
func (h *Handler) Withdraw(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.mineID(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Withdraw(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeApplication(w, http.StatusOK, v)
}

// serve streams a private file: never cacheable, never sniffed, always a download.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, f FileContent) {
	defer f.Content.Close()
	ascii := strings.Map(func(c rune) rune {
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return '_'
		}
		return c
	}, f.File.FileName)
	head := w.Header()
	head.Set("Content-Type", f.File.ContentType)
	head.Set("Content-Length", strconv.FormatInt(f.File.Size, 10))
	head.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(f.File.FileName)))
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Cache-Control", "private, no-store")
	head.Set("X-Robots-Tag", "noindex")
	head.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f.Content); err != nil {
		h.logger.WarnContext(r.Context(), "application file download interrupted", slog.String("file_id", f.File.ID.String()))
	}
}

// ---------------------------------------------------------------------------
// The receiving side: /v1/agent/organisations/{id}/applications
// ---------------------------------------------------------------------------

func (h *Handler) receivedPath(w http.ResponseWriter, r *http.Request, withID bool) (*auth.Principal, uuid.UUID, uuid.UUID, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		h.fail(w, r, ErrUnauthenticated)
		return nil, uuid.Nil, uuid.Nil, false
	}
	book, ok := pathID(r, "id")
	if !ok {
		if withID {
			h.fail(w, r, ErrNotFound)
		} else {
			h.fail(w, r, ErrOrganisationUnknown)
		}
		return nil, uuid.Nil, uuid.Nil, false
	}
	var id uuid.UUID
	if withID {
		if id, ok = pathID(r, "applicationId"); !ok {
			h.fail(w, r, ErrNotFound)
			return nil, uuid.Nil, uuid.Nil, false
		}
	}
	return p, book, id, true
}

// Received serves GET .../applications?listing_id=&status=&q=&page=&page_size=.
func (h *Handler) Received(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.receivedPath(w, r, false)
	if !ok {
		return
	}
	fields := map[string]string{}
	f := AppFilter{Status: statusParam(r, fields), Q: r.URL.Query().Get("q")}
	if f.Status == StatusDraft {
		fields["status"] = "drafts are private to the applicant"
	}
	if raw := r.URL.Query().Get("listing_id"); raw != "" {
		if id, err := uuid.Parse(raw); err != nil {
			fields["listing_id"] = "must be a listing ID"
		} else {
			f.ListingID = &id
		}
	}
	if len([]rune(f.Q)) > 100 {
		fields["q"] = "must be at most 100 characters"
	}
	page, size := paging(r, fields)
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	out, err := h.svc.Received(r.Context(), p, book, f, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ReceivedOne serves GET .../applications/{applicationId} (audited).
func (h *Handler) ReceivedOne(w http.ResponseWriter, r *http.Request) {
	p, book, id, ok := h.receivedPath(w, r, true)
	if !ok {
		return
	}
	v, err := h.svc.ReceivedOne(r.Context(), p, book, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeApplication(w, http.StatusOK, v)
}

// ReceivedFile serves GET .../applications/{applicationId}/files/{fileId} (audited).
func (h *Handler) ReceivedFile(w http.ResponseWriter, r *http.Request) {
	p, book, id, ok := h.receivedPath(w, r, true)
	if !ok {
		return
	}
	fileID, ok := pathID(r, "fileId")
	if !ok {
		h.fail(w, r, ErrFileNotFound)
		return
	}
	f, err := h.svc.ReceivedFile(r.Context(), p, book, id, fileID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.serve(w, r, f)
}

// SetStatus serves POST .../applications/{applicationId}/status {status, note?}.
func (h *Handler) SetStatus(w http.ResponseWriter, r *http.Request) {
	p, book, id, ok := h.receivedPath(w, r, true)
	if !ok {
		return
	}
	var in StatusInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.SetStatus(r.Context(), p, book, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeApplication(w, http.StatusOK, v)
}

// RequestInfo serves POST .../applications/{applicationId}/request-info {message}.
func (h *Handler) RequestInfo(w http.ResponseWriter, r *http.Request) {
	p, book, id, ok := h.receivedPath(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	v, err := h.svc.RequestInfo(r.Context(), p, book, id, body.Message)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeApplication(w, http.StatusOK, v)
}

// AddNote serves POST .../applications/{applicationId}/notes {text}.
func (h *Handler) AddNote(w http.ResponseWriter, r *http.Request) {
	p, book, id, ok := h.receivedPath(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	n, err := h.svc.AddNote(r.Context(), p, book, id, body.Text)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"note": n})
}
