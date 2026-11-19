package maintenance

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

	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/logging"
)

// Error codes.
const (
	CodeBookNotFound             = "BOOK_NOT_FOUND"
	CodeNotFound                 = "MAINTENANCE_NOT_FOUND"
	CodePhotoNotFound            = "MAINTENANCE_PHOTO_NOT_FOUND"
	CodeInvalidTransition        = "MAINTENANCE_INVALID_TRANSITION"
	CodeClosed                   = "MAINTENANCE_CLOSED"
	CodePhotoLimit               = "MAINTENANCE_PHOTO_LIMIT"
	CodeChanged                  = "MAINTENANCE_CHANGED"
	CodeTenancyNotFound          = "TENANCY_NOT_FOUND"
	CodeTenancyInvalidState      = "TENANCY_INVALID_STATE"
	CodeFileTooLarge             = "FILE_TOO_LARGE"
	CodeUnsupportedFile          = "UNSUPPORTED_FILE_TYPE"
	DefaultPageSize, MaxPageSize = 25, 100
)

// Handler serves /v1/agent/organisations/{id}/maintenance (the book is the
// route's {id}) and /v1/me/tenancies/{id}/maintenance.
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
	var state *tenancies.StateError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &tr):
		writeDetails(w, r, http.StatusConflict, CodeInvalidTransition, "A request that is "+strings.ToLower(StatusLabel(tr.Status))+" cannot move there.",
			map[string]any{"status": tr.Status, "allowed": tr.Allowed})
	case errors.As(err, &state):
		writeDetails(w, r, http.StatusConflict, CodeTenancyInvalidState, "Requests can only be raised on an active tenancy.",
			map[string]any{"status": state.Status})
	case errors.Is(err, ErrUnauthenticated):
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	case errors.Is(err, ErrBookNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeBookNotFound, "These books do not exist or are not yours.", nil)
	case errors.Is(err, ErrTenancyNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeTenancyNotFound, "This tenancy does not exist or is not yours.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotFound, "This maintenance request does not exist or is not yours.", nil)
	case errors.Is(err, ErrPhotoNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodePhotoNotFound, "This photo does not exist.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Your role does not allow this.", nil)
	case errors.Is(err, ErrClosed):
		httpx.WriteError(w, r, http.StatusConflict, CodeClosed, "This request is closed or cancelled.", nil)
	case errors.Is(err, ErrPhotoLimit):
		httpx.WriteError(w, r, http.StatusConflict, CodePhotoLimit, fmt.Sprintf("A request holds at most %d photos.", MaxPhotos), nil)
	case errors.Is(err, ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, CodeChanged, "The request changed meanwhile. Reload it and try again.", nil)
	case errors.Is(err, ErrPhotoTooLarge), errors.As(err, &tooLarge):
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, CodeFileTooLarge, "A photo must be at most 10 MB.", nil)
	case errors.Is(err, ErrPhotoUnsupported):
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedFile, "A photo must be a JPEG, PNG or WebP image.", nil)
	case errors.Is(err, ErrPhotoDimensions):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", map[string]string{
			"file": fmt.Sprintf("must be at most %dx%d pixels (%d megapixels)", MaxPhotoSide, MaxPhotoSide, MaxPhotoPixels/1_000_000)})
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
			if n == "photoId" {
				h.fail(w, r, ErrPhotoNotFound)
			} else {
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
		h.logger.WarnContext(r.Context(), "maintenance photo download interrupted", slog.String("error", err.Error()))
	}
}

// ---------------------------------------------------------------------------
// The managers
// ---------------------------------------------------------------------------

// Options serves GET .../maintenance/options.
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

// Providers serves GET .../maintenance/providers.
func (h *Handler) Providers(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	out, err := h.svc.SearchProviders(r.Context(), p, book, q.Get("q"), q.Get("trade"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"items": out})
}

// List serves GET .../maintenance.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := map[string]string{}
	page, size := paging(q, f)
	flt := Filter{Status: strings.TrimSpace(q.Get("status")), Category: strings.TrimSpace(q.Get("category")),
		Priority: strings.TrimSpace(q.Get("priority")), Q: strings.TrimSpace(q.Get("q")),
		PropertyID: optionalID(q, "property_id", f), TenancyID: optionalID(q, "tenancy_id", f)}
	if flt.Status != "" && StatusLabel(flt.Status) == flt.Status {
		f["status"] = "must be one of " + strings.Join(Statuses(), ", ")
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

// Create serves POST .../maintenance.
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
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"request": out})
}

// Get serves GET .../maintenance/{requestId}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "requestId")
	if !ok {
		return
	}
	out, err := h.svc.Get(r.Context(), p, book, ids[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"request": out})
}

// Update serves PATCH .../maintenance/{requestId}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "requestId")
	if !ok {
		return
	}
	var in PatchInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Update(r.Context(), p, book, ids[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"request": out})
}

// Status serves POST .../maintenance/{requestId}/status.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "requestId")
	if !ok {
		return
	}
	var in StatusInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.ChangeStatus(r.Context(), p, book, ids[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"request": out})
}

// Comment serves POST .../maintenance/{requestId}/comments.
func (h *Handler) Comment(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "requestId")
	if !ok {
		return
	}
	var in CommentInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Comment(r.Context(), p, book, ids[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"item": out})
}

// Assign serves POST .../maintenance/{requestId}/assign.
func (h *Handler) Assign(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "requestId")
	if !ok {
		return
	}
	var in AssignInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Assign(r.Context(), p, book, ids[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

// AddCost serves POST .../maintenance/{requestId}/costs.
func (h *Handler) AddCost(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "requestId")
	if !ok {
		return
	}
	var in CostInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.AddCost(r.Context(), p, book, ids[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, out)
}

// AddPhoto serves POST .../maintenance/{requestId}/photos.
func (h *Handler) AddPhoto(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "requestId")
	if !ok {
		return
	}
	content, err := readPhoto(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.svc.AddPhoto(r.Context(), p, book, ids[0], content)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"photo": out})
}

// Photo serves GET .../maintenance/{requestId}/photos/{photoId}.
func (h *Handler) Photo(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "requestId", "photoId")
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

// DeletePhoto serves DELETE .../maintenance/{requestId}/photos/{photoId}.
func (h *Handler) DeletePhoto(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "requestId", "photoId")
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

// Routes mounts .../maintenance under a router whose path holds {id}.
func (h *Handler) Routes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/", h.List)
	r.With(limit).Post("/", h.Create)
	r.Get("/options", h.Options)
	r.Get("/providers", h.Providers)
	r.Get("/{requestId}", h.Get)
	r.With(limit).Patch("/{requestId}", h.Update)
	r.With(limit).Post("/{requestId}/status", h.Status)
	r.With(limit).Post("/{requestId}/comments", h.Comment)
	r.With(limit).Post("/{requestId}/assign", h.Assign)
	r.With(limit).Post("/{requestId}/costs", h.AddCost)
	r.With(limit).Post("/{requestId}/photos", h.AddPhoto)
	r.Get("/{requestId}/photos/{photoId}", h.Photo)
	r.With(limit).Delete("/{requestId}/photos/{photoId}", h.DeletePhoto)
}

// ---------------------------------------------------------------------------
// The tenant
// ---------------------------------------------------------------------------

// MyOptions serves GET /v1/me/tenancies/{id}/maintenance/options.
func (h *Handler) MyOptions(w http.ResponseWriter, r *http.Request) {
	p, tenancy, _, ok := h.mine(w, r)
	if !ok {
		return
	}
	out, err := h.svc.MyOptions(r.Context(), p, tenancy)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

// MyList serves GET /v1/me/tenancies/{id}/maintenance.
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

// MyCreate serves POST /v1/me/tenancies/{id}/maintenance.
func (h *Handler) MyCreate(w http.ResponseWriter, r *http.Request) {
	p, tenancy, _, ok := h.mine(w, r)
	if !ok {
		return
	}
	var in MyInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.MyCreate(r.Context(), p, tenancy, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"request": out})
}

// MyGet serves GET /v1/me/tenancies/{id}/maintenance/{requestId}.
func (h *Handler) MyGet(w http.ResponseWriter, r *http.Request) {
	p, tenancy, ids, ok := h.mine(w, r, "requestId")
	if !ok {
		return
	}
	out, err := h.svc.MyGet(r.Context(), p, tenancy, ids[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"request": out})
}

// MyComment serves POST /v1/me/tenancies/{id}/maintenance/{requestId}/comments.
func (h *Handler) MyComment(w http.ResponseWriter, r *http.Request) {
	p, tenancy, ids, ok := h.mine(w, r, "requestId")
	if !ok {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.MyComment(r.Context(), p, tenancy, ids[0], CommentInput{Text: in.Text})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"item": out})
}

// MyAddPhoto serves POST /v1/me/tenancies/{id}/maintenance/{requestId}/photos.
func (h *Handler) MyAddPhoto(w http.ResponseWriter, r *http.Request) {
	p, tenancy, ids, ok := h.mine(w, r, "requestId")
	if !ok {
		return
	}
	content, err := readPhoto(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.svc.MyAddPhoto(r.Context(), p, tenancy, ids[0], content)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"photo": out})
}

// MyPhoto serves GET /v1/me/tenancies/{id}/maintenance/{requestId}/photos/{photoId}.
func (h *Handler) MyPhoto(w http.ResponseWriter, r *http.Request) {
	p, tenancy, ids, ok := h.mine(w, r, "requestId", "photoId")
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

// MyRoutes mounts the tenant's routes under /v1/me/tenancies/{id}/maintenance.
func (h *Handler) MyRoutes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/", h.MyList)
	r.With(limit).Post("/", h.MyCreate)
	r.Get("/options", h.MyOptions)
	r.Get("/{requestId}", h.MyGet)
	r.With(limit).Post("/{requestId}/comments", h.MyComment)
	r.With(limit).Post("/{requestId}/photos", h.MyAddPhoto)
	r.Get("/{requestId}/photos/{photoId}", h.MyPhoto)
}
