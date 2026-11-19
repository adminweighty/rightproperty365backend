package inspections

import (
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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/logging"
)

// Error codes.
const (
	CodeBookNotFound             = "BOOK_NOT_FOUND"
	CodeNotFound                 = "INSPECTION_NOT_FOUND"
	CodeItemNotFound             = "INSPECTION_ITEM_NOT_FOUND"
	CodePhotoNotFound            = "INSPECTION_PHOTO_NOT_FOUND"
	CodeClosed                   = "INSPECTION_CLOSED"
	CodeInvalidTransition        = "INSPECTION_INVALID_TRANSITION"
	CodeIncomplete               = "INSPECTION_INCOMPLETE"
	CodeChanged                  = "INSPECTION_CHANGED"
	CodePhotoLimit               = "INSPECTION_PHOTO_LIMIT"
	CodeTenancyNotFound          = "TENANCY_NOT_FOUND"
	CodeFileTooLarge             = "FILE_TOO_LARGE"
	CodeUnsupportedFile          = "UNSUPPORTED_FILE_TYPE"
	DefaultPageSize, MaxPageSize = 25, 100
)

// Handler serves /v1/agent/organisations/{id}/inspections (the book is the
// route's {id}) and /v1/me/tenancies/{id}/inspections.
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
	private(w)
	var verr *ValidationError
	var tr *TransitionError
	var closed *ClosedError
	var incomplete *IncompleteError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &closed):
		writeDetails(w, r, http.StatusConflict, CodeClosed, "This inspection is "+strings.ToLower(StatusLabel(closed.Status))+
			" and can no longer change.", map[string]any{"status": closed.Status})
	case errors.As(err, &tr):
		writeDetails(w, r, http.StatusConflict, CodeInvalidTransition, "An inspection that is "+strings.ToLower(StatusLabel(tr.Status))+
			" cannot move there.", map[string]any{"status": tr.Status, "allowed": tr.Allowed})
	case errors.As(err, &incomplete):
		ids := make([]string, 0, len(incomplete.ItemIDs))
		for _, id := range incomplete.ItemIDs {
			ids = append(ids, id.String())
		}
		writeDetails(w, r, http.StatusConflict, CodeIncomplete, "Every item needs a status before the inspection can be completed.",
			map[string]any{"missing": incomplete.Missing, "item_ids": ids})
	case errors.Is(err, ErrUnauthenticated):
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	case errors.Is(err, ErrBookNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeBookNotFound, "These books do not exist or are not yours.", nil)
	case errors.Is(err, ErrTenancyNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeTenancyNotFound, "This tenancy does not exist or is not yours.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotFound, "This inspection does not exist or is not yours.", nil)
	case errors.Is(err, ErrItemNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeItemNotFound, "This checklist item does not exist.", nil)
	case errors.Is(err, ErrPhotoNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodePhotoNotFound, "This photo does not exist.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Your role does not allow this.", nil)
	case errors.Is(err, ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, CodeChanged, "The inspection changed meanwhile. Reload it and try again.", nil)
	case errors.Is(err, ErrPhotoLimit):
		httpx.WriteError(w, r, http.StatusConflict, CodePhotoLimit,
			fmt.Sprintf("An item holds at most %d photos and an inspection %d.", MaxPhotosPerItem, MaxPhotos), nil)
	case errors.Is(err, ErrPhotoTooLarge), errors.As(err, &tooLarge):
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, CodeFileTooLarge, "A photo must be at most 10 MB.", nil)
	case errors.Is(err, ErrPhotoUnsupported):
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedFile, "A photo must be a JPEG, PNG or WebP image.", nil)
	case errors.Is(err, ErrPhotoDimensions):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", map[string]string{
			"file": "must be at most 8000x8000 pixels (40 megapixels)"})
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

// begin resolves the caller and the book (404 first, before any validation
// or decoding) and parses the other path IDs.
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
	ids, ok := h.ids(w, r, names)
	return p, book, ids, ok
}

func (h *Handler) ids(w http.ResponseWriter, r *http.Request, names []string) ([]uuid.UUID, bool) {
	out := make([]uuid.UUID, 0, len(names))
	for _, n := range names {
		id, err := uuid.Parse(chi.URLParam(r, n))
		if err != nil {
			switch n {
			case "photoId":
				h.fail(w, r, ErrPhotoNotFound)
			case "itemId":
				h.fail(w, r, ErrItemNotFound)
			default:
				h.fail(w, r, ErrNotFound)
			}
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}

// mine resolves the caller and the tenancy of a /v1/me/tenancies/{id} route.
func (h *Handler) mine(w http.ResponseWriter, r *http.Request, names ...string) (*auth.Principal, uuid.UUID, []uuid.UUID, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		h.fail(w, r, ErrUnauthenticated)
		return nil, uuid.Nil, nil, false
	}
	tenancy, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, ErrTenancyNotFound)
		return nil, uuid.Nil, nil, false
	}
	ids, ok := h.ids(w, r, names)
	return p, tenancy, ids, ok
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

// readPhoto reads the multipart form: exactly one "file" part, at most
// MaxPhotoBytes+1 bytes of it.
func readPhoto(r *http.Request) ([]byte, error) {
	problem := func(message string) error { return &ValidationError{Fields: map[string]string{"file": message}} }
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, problem("the request must be multipart/form-data with the field file")
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	var content []byte
	seen := false
	for parts := 0; ; parts++ {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, ErrPhotoTooLarge
		}
		if err != nil {
			return nil, problem("the multipart body could not be read")
		}
		if parts >= 3 || (part.FormName() == "file" && seen) {
			return nil, problem("send exactly one file")
		}
		if part.FormName() == "file" {
			seen = true
			content, err = io.ReadAll(io.LimitReader(part, MaxPhotoBytes+1))
			if errors.As(err, &tooLarge) {
				return nil, ErrPhotoTooLarge
			}
			if err != nil {
				return nil, problem("the file could not be read")
			}
		}
		_ = part.Close()
	}
	if !seen {
		return nil, problem("is required")
	}
	if len(content) > MaxPhotoBytes {
		return nil, ErrPhotoTooLarge
	}
	return content, nil
}

func (h *Handler) image(w http.ResponseWriter, r *http.Request, rc io.ReadCloser) {
	defer rc.Close()
	private(w)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		h.logger.WarnContext(r.Context(), "inspection photo download interrupted", slog.String("error", err.Error()))
	}
}

func (h *Handler) pdf(w http.ResponseWriter, content []byte, name string) {
	private(w)
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// ---------------------------------------------------------------------------
// The managers
// ---------------------------------------------------------------------------

// Options serves GET .../inspections/options.
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

// List serves GET .../inspections.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := map[string]string{}
	page, size := paging(q, f)
	flt := Filter{Status: strings.TrimSpace(q.Get("status")), Type: strings.TrimSpace(q.Get("type")),
		PropertyID: optionalID(q, "property_id", f), TenancyID: optionalID(q, "tenancy_id", f),
		From: strings.TrimSpace(q.Get("from")), To: strings.TrimSpace(q.Get("to"))}
	if flt.Status != "" && StatusLabel(flt.Status) == flt.Status {
		f["status"] = "must be one of " + strings.Join(Statuses(), ", ")
	}
	if flt.Type != "" && TypeLabel(flt.Type) == flt.Type {
		f["type"] = "must be one of " + strings.Join(Types(), ", ")
	}
	for name, v := range map[string]string{"from": flt.From, "to": flt.To} {
		if v != "" && !ValidDate(v) {
			f[name] = "must be a date (YYYY-MM-DD)"
		}
	}
	if flt.From != "" && flt.To != "" && f["from"] == "" && f["to"] == "" && flt.To < flt.From {
		f["to"] = "must not be before from"
	}
	if len(f) > 0 {
		h.fail(w, r, &ValidationError{Fields: f})
		return
	}
	out, err := h.svc.List(r.Context(), p, book, flt, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

func (h *Handler) one(w http.ResponseWriter, r *http.Request, status int, out InspectionView, err error) {
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, status, map[string]any{"inspection": out})
}

// Create serves POST .../inspections.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Create(r.Context(), p, book, in)
	h.one(w, r, http.StatusCreated, out, err)
}

// Get serves GET .../inspections/{inspectionId}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "inspectionId")
	if !ok {
		return
	}
	out, err := h.svc.Get(r.Context(), p, book, ids[0])
	h.one(w, r, http.StatusOK, out, err)
}

// Update serves PATCH .../inspections/{inspectionId}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "inspectionId")
	if !ok {
		return
	}
	var in PatchInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Update(r.Context(), p, book, ids[0], in)
	h.one(w, r, http.StatusOK, out, err)
}

// Checklist serves PUT .../inspections/{inspectionId}/checklist.
func (h *Handler) Checklist(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "inspectionId")
	if !ok {
		return
	}
	var in ChecklistInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.PutChecklist(r.Context(), p, book, ids[0], in)
	h.one(w, r, http.StatusOK, out, err)
}

// Findings serves PUT .../inspections/{inspectionId}/findings.
func (h *Handler) Findings(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "inspectionId")
	if !ok {
		return
	}
	var in FindingsInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.PutFindings(r.Context(), p, book, ids[0], in)
	h.one(w, r, http.StatusOK, out, err)
}

// Start serves POST .../inspections/{inspectionId}/start.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "inspectionId")
	if !ok {
		return
	}
	out, err := h.svc.Start(r.Context(), p, book, ids[0])
	h.one(w, r, http.StatusOK, out, err)
}

// Complete serves POST .../inspections/{inspectionId}/complete (the body
// may be empty).
func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "inspectionId")
	if !ok {
		return
	}
	var in CompleteInput
	if r.ContentLength != 0 || r.Header.Get("Content-Type") != "" {
		if !httpx.DecodeJSON(w, r, &in) {
			return
		}
	}
	out, err := h.svc.Complete(r.Context(), p, book, ids[0], in)
	h.one(w, r, http.StatusOK, out, err)
}

// Cancel serves POST .../inspections/{inspectionId}/cancel.
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "inspectionId")
	if !ok {
		return
	}
	var in CancelInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Cancel(r.Context(), p, book, ids[0], in)
	h.one(w, r, http.StatusOK, out, err)
}

// Report serves GET .../inspections/{inspectionId}/report.pdf.
func (h *Handler) Report(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "inspectionId")
	if !ok {
		return
	}
	content, name, err := h.svc.Report(r.Context(), p, book, ids[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.pdf(w, content, name)
}

// AddPhoto serves POST .../inspections/{inspectionId}/items/{itemId}/photos.
func (h *Handler) AddPhoto(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "inspectionId", "itemId")
	if !ok {
		return
	}
	content, err := readPhoto(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.svc.AddPhoto(r.Context(), p, book, ids[0], ids[1], content)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"photo": out})
}

// Photo serves GET .../inspections/{inspectionId}/photos/{photoId}.
func (h *Handler) Photo(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "inspectionId", "photoId")
	if !ok {
		return
	}
	rc, err := h.svc.OpenPhoto(r.Context(), p, book, ids[0], ids[1])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.image(w, r, rc)
}

// DeletePhoto serves DELETE .../inspections/{inspectionId}/photos/{photoId}.
func (h *Handler) DeletePhoto(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "inspectionId", "photoId")
	if !ok {
		return
	}
	if err := h.svc.DeletePhoto(r.Context(), p, book, ids[0], ids[1]); err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	w.WriteHeader(http.StatusNoContent)
}

// Routes mounts .../inspections under a router whose path holds {id}.
func (h *Handler) Routes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/", h.List)
	r.With(limit).Post("/", h.Create)
	r.Get("/options", h.Options)
	r.Get("/{inspectionId}", h.Get)
	r.With(limit).Patch("/{inspectionId}", h.Update)
	r.With(limit).Put("/{inspectionId}/checklist", h.Checklist)
	r.With(limit).Put("/{inspectionId}/findings", h.Findings)
	r.With(limit).Post("/{inspectionId}/start", h.Start)
	r.With(limit).Post("/{inspectionId}/complete", h.Complete)
	r.With(limit).Post("/{inspectionId}/cancel", h.Cancel)
	r.Get("/{inspectionId}/report.pdf", h.Report)
	r.With(limit).Post("/{inspectionId}/items/{itemId}/photos", h.AddPhoto)
	r.Get("/{inspectionId}/photos/{photoId}", h.Photo)
	r.With(limit).Delete("/{inspectionId}/photos/{photoId}", h.DeletePhoto)
}

// ---------------------------------------------------------------------------
// The tenant
// ---------------------------------------------------------------------------

// MyList serves GET /v1/me/tenancies/{id}/inspections.
func (h *Handler) MyList(w http.ResponseWriter, r *http.Request) {
	p, tenancy, _, ok := h.mine(w, r)
	if !ok {
		return
	}
	out, err := h.svc.MyList(r.Context(), p, tenancy)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"items": out})
}

// MyGet serves GET /v1/me/tenancies/{id}/inspections/{inspectionId}.
func (h *Handler) MyGet(w http.ResponseWriter, r *http.Request) {
	p, tenancy, ids, ok := h.mine(w, r, "inspectionId")
	if !ok {
		return
	}
	out, err := h.svc.MyGet(r.Context(), p, tenancy, ids[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"inspection": out})
}

// MyReport serves GET /v1/me/tenancies/{id}/inspections/{inspectionId}/report.pdf.
func (h *Handler) MyReport(w http.ResponseWriter, r *http.Request) {
	p, tenancy, ids, ok := h.mine(w, r, "inspectionId")
	if !ok {
		return
	}
	content, name, err := h.svc.MyReport(r.Context(), p, tenancy, ids[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.pdf(w, content, name)
}

// MyPhoto serves GET /v1/me/tenancies/{id}/inspections/{inspectionId}/photos/{photoId}.
func (h *Handler) MyPhoto(w http.ResponseWriter, r *http.Request) {
	p, tenancy, ids, ok := h.mine(w, r, "inspectionId", "photoId")
	if !ok {
		return
	}
	rc, err := h.svc.MyOpenPhoto(r.Context(), p, tenancy, ids[0], ids[1])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.image(w, r, rc)
}

// MyRoutes mounts the tenant's routes under /v1/me/tenancies/{id}/inspections.
func (h *Handler) MyRoutes(r chi.Router) {
	r.Get("/", h.MyList)
	r.Get("/{inspectionId}", h.MyGet)
	r.Get("/{inspectionId}/report.pdf", h.MyReport)
	r.Get("/{inspectionId}/photos/{photoId}", h.MyPhoto)
}
