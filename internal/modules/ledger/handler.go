package ledger

import (
	"encoding/csv"
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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/logging"
)

// Error codes.
const (
	CodeBookNotFound             = "BOOK_NOT_FOUND"
	CodeEntryNotFound            = "LEDGER_ENTRY_NOT_FOUND"
	CodeAttachmentNotFound       = "LEDGER_ATTACHMENT_NOT_FOUND"
	CodeCategoryNotFound         = "LEDGER_CATEGORY_NOT_FOUND"
	CodeAlreadyReversed          = "LEDGER_ENTRY_ALREADY_REVERSED"
	CodeNotReversible            = "LEDGER_ENTRY_NOT_REVERSIBLE"
	CodeOpeningBalanceExists     = "LEDGER_OPENING_BALANCE_EXISTS"
	CodeCategoryExists           = "LEDGER_CATEGORY_EXISTS"
	CodeUnsupportedFileType      = "UNSUPPORTED_FILE_TYPE"
	CodeFileTooLarge             = "FILE_TOO_LARGE"
	CodeRentReceipt              = "LEDGER_ENTRY_IS_RENT_RECEIPT"
	DefaultPageSize, MaxPageSize = 25, 100
)

// Handler serves /v1/agent/organisations/{id}/ledger. The book is the
// route's {id}; an entry is {entryId}.
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
	var verr *ValidationError
	var opening *OpeningExistsError
	var reversed *ReversedError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &opening):
		var existing any
		if opening.Existing != uuid.Nil {
			existing = opening.Existing
		}
		writeDetails(w, r, http.StatusConflict, CodeOpeningBalanceExists,
			"This book already has an opening balance for that currency, method and branch. Reverse it first, or correct it.",
			map[string]any{"existing_entry_id": existing, "currency": opening.Currency, "method": opening.Method, "branch": strPtr(opening.Branch)})
	case errors.As(err, &reversed):
		writeDetails(w, r, http.StatusConflict, CodeAlreadyReversed, "This entry has already been reversed.",
			map[string]any{"reversed_by": reversed.ReversedBy})
	case errors.As(err, &tooLarge), errors.Is(err, ErrFileTooLarge):
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, CodeFileTooLarge, fmt.Sprintf("A file may be at most %d MB.", MaxFileBytes>>20), nil)
	case errors.Is(err, ErrUnauthenticated):
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	case errors.Is(err, ErrBookNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeBookNotFound, "These books do not exist or are not yours.", nil)
	case errors.Is(err, ErrBranchForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, "BRANCH_FORBIDDEN", "This entry belongs to another branch of your organisation.", nil)
	case errors.Is(err, ErrEntryNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeEntryNotFound, "This entry does not exist in these books.", nil)
	case errors.Is(err, ErrAttachmentNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeAttachmentNotFound, "This attachment does not exist.", nil)
	case errors.Is(err, ErrCategoryNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeCategoryNotFound, "This category does not exist in these books.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Your role does not allow this.", nil)
	case errors.Is(err, ErrAlreadyReversed):
		writeDetails(w, r, http.StatusConflict, CodeAlreadyReversed, "This entry has already been reversed.", map[string]any{"reversed_by": nil})
	case errors.Is(err, ErrNotReversible):
		httpx.WriteError(w, r, http.StatusConflict, CodeNotReversible, "A reversal cannot be reversed or corrected. Post a new entry instead.", nil)
	case errors.Is(err, ErrRentReceipt):
		httpx.WriteError(w, r, http.StatusConflict, CodeRentReceipt,
			"This entry is a rent receipt. Reverse it, then record a new receipt on the tenancy.", nil)
	case errors.Is(err, ErrOpeningBalanceExists):
		writeDetails(w, r, http.StatusConflict, CodeOpeningBalanceExists,
			"This book already has an opening balance for that currency, method and branch. Reverse it first, or correct it.", map[string]any{})
	case errors.Is(err, ErrCategoryExists):
		httpx.WriteError(w, r, http.StatusConflict, CodeCategoryExists, "A category with this code already exists in these books.", nil)
	case errors.Is(err, ErrUnsupportedFile):
		httpx.WriteError(w, r, http.StatusBadRequest, CodeUnsupportedFileType,
			"This type of file is not accepted here. Accepted: "+strings.ToUpper(strings.Join(AcceptedFileKinds(), ", "))+".", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func private(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
}

// begin resolves the caller and the book (404 first, before any validation
// or decoding) and parses the other path IDs (a malformed one is the 404 of
// what it names).
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
	out := make([]uuid.UUID, 0, len(names))
	for _, n := range names {
		id, err := uuid.Parse(chi.URLParam(r, n))
		if err != nil {
			switch n {
			case "attachmentId":
				h.fail(w, r, ErrAttachmentNotFound)
			default:
				h.fail(w, r, ErrEntryNotFound)
			}
			return nil, uuid.Nil, nil, false
		}
		out = append(out, id)
	}
	return p, book, out, true
}

func (h *Handler) ok(w http.ResponseWriter, status int, v any) {
	private(w)
	httpx.WriteJSON(w, status, v)
}

// Options serves GET .../ledger/options.
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

func optionalDate(q url.Values, name string, f map[string]string) string {
	raw := q.Get(name)
	if raw != "" && !validDate(raw) {
		f[name] = "must be a date (YYYY-MM-DD)"
	}
	return raw
}

func optionalBranch(q url.Values) *string {
	if !q.Has("branch") {
		return nil
	}
	b := strings.TrimSpace(q.Get("branch"))
	return &b
}

// filter reads the entries filter of GET .../entries (and of the entries export).
func filter(q url.Values, f map[string]string) Filter {
	out := Filter{Currency: strings.ToUpper(strings.TrimSpace(q.Get("currency"))), Type: q.Get("type"), Category: q.Get("category"),
		Method: q.Get("method"), Status: q.Get("status"), Q: q.Get("q"), Branch: optionalBranch(q)}
	if out.Type != "" {
		if _, ok := typeInfo(out.Type); !ok {
			f["type"] = "must be one of " + strings.Join(append(typeCodes(), TypeReversal), ", ")
		}
	}
	if out.Method != "" && !slices.Contains(Methods(), out.Method) {
		f["method"] = "must be CASH, BANK, MOBILE_MONEY or OTHER"
	}
	if out.Status != "" && !slices.Contains(Statuses(), out.Status) {
		f["status"] = "must be " + strings.Join(Statuses(), ", ")
	}
	out.From, out.To = optionalDate(q, "from", f), optionalDate(q, "to", f)
	if out.From != "" && out.To != "" && out.From > out.To && f["from"] == "" && f["to"] == "" {
		f["from"] = "must be on or before to"
	}
	out.ContactID, out.ListingID, out.LeaseID = optionalID(q, "contact_id", f), optionalID(q, "listing_id", f), optionalID(q, "lease_id", f)
	switch q.Get("include_reversed") {
	case "", "true", "1":
	case "false", "0":
		out.ExcludeReversed = true
	default:
		f["include_reversed"] = "must be true or false"
	}
	switch q.Get("order") {
	case "", "desc":
	case "asc":
		out.Ascending = true
	default:
		f["order"] = "must be asc or desc"
	}
	return out
}

// List serves GET .../entries.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := map[string]string{}
	flt := filter(q, f)
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

// Create serves POST .../entries.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	var in EntryInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Create(r.Context(), p, book, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, out)
}

// Get serves GET .../entries/{entryId}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, book, got, ok := h.begin(w, r, "entryId")
	if !ok {
		return
	}
	out, err := h.svc.Get(r.Context(), p, book, got[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

// Reverse serves POST .../entries/{entryId}/reverse.
func (h *Handler) Reverse(w http.ResponseWriter, r *http.Request) {
	p, book, got, ok := h.begin(w, r, "entryId")
	if !ok {
		return
	}
	var in ReverseInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Reverse(r.Context(), p, book, got[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, out)
}

// Correct serves POST .../entries/{entryId}/correct.
func (h *Handler) Correct(w http.ResponseWriter, r *http.Request) {
	p, book, got, ok := h.begin(w, r, "entryId")
	if !ok {
		return
	}
	var in CorrectInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Correct(r.Context(), p, book, got[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, out)
}

// Reconcile serves POST .../entries/{entryId}/reconcile.
func (h *Handler) Reconcile(w http.ResponseWriter, r *http.Request) {
	p, book, got, ok := h.begin(w, r, "entryId")
	if !ok {
		return
	}
	var in ReconcileInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Reconcile(r.Context(), p, book, got[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"entry": out})
}

// AddNote serves POST .../entries/{entryId}/notes.
func (h *Handler) AddNote(w http.ResponseWriter, r *http.Request) {
	p, book, got, ok := h.begin(w, r, "entryId")
	if !ok {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.AddNote(r.Context(), p, book, got[0], in.Text)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"note": out})
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

// AddAttachment serves POST .../entries/{entryId}/attachments (multipart).
func (h *Handler) AddAttachment(w http.ResponseWriter, r *http.Request) {
	p, book, got, ok := h.begin(w, r, "entryId")
	if !ok {
		return
	}
	name, content, err := readFile(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.svc.AddAttachment(r.Context(), p, book, got[0], name, content)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"attachment": out})
}

// Attachment serves GET .../entries/{entryId}/attachments/{attachmentId} (audited).
func (h *Handler) Attachment(w http.ResponseWriter, r *http.Request) {
	p, book, got, ok := h.begin(w, r, "entryId", "attachmentId")
	if !ok {
		return
	}
	f, err := h.svc.OpenAttachment(r.Context(), p, book, got[0], got[1])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer f.Content.Close()
	ascii := strings.Map(func(c rune) rune {
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return '_'
		}
		return c
	}, f.Attachment.FileName)
	head := w.Header()
	head.Set("Content-Type", f.Attachment.ContentType)
	head.Set("Content-Length", strconv.FormatInt(f.Attachment.Size, 10))
	head.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(f.Attachment.FileName)))
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	private(w)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f.Content); err != nil {
		h.logger.WarnContext(r.Context(), "ledger attachment download interrupted", slog.String("attachment_id", f.Attachment.ID.String()))
	}
}

// reportQuery reads the common report query.
func reportQuery(q url.Values, f map[string]string) ReportQuery {
	return ReportQuery{From: optionalDate(q, "from", f), To: optionalDate(q, "to", f), Currency: q.Get("currency"), Branch: optionalBranch(q),
		By: q.Get("by"), ContactID: optionalID(q, "contact_id", f), AsOf: optionalDate(q, "as_of", f), Side: q.Get("side")}
}

func (h *Handler) report(w http.ResponseWriter, r *http.Request, run func(*auth.Principal, uuid.UUID, ReportQuery) (any, error)) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	f := map[string]string{}
	q := reportQuery(r.URL.Query(), f)
	if len(f) > 0 {
		h.fail(w, r, &ValidationError{Fields: f})
		return
	}
	out, err := run(p, book, q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

// Cashbook serves GET .../reports/cashbook.
func (h *Handler) Cashbook(w http.ResponseWriter, r *http.Request) {
	h.report(w, r, func(p *auth.Principal, book uuid.UUID, q ReportQuery) (any, error) {
		return h.svc.Cashbook(r.Context(), p, book, q)
	})
}

// Categories serves GET .../reports/categories.
func (h *Handler) CategoriesReport(w http.ResponseWriter, r *http.Request) {
	h.report(w, r, func(p *auth.Principal, book uuid.UUID, q ReportQuery) (any, error) {
		return h.svc.CategoriesReport(r.Context(), p, book, q)
	})
}

// Statement serves GET .../reports/statement.
func (h *Handler) Statement(w http.ResponseWriter, r *http.Request) {
	h.report(w, r, func(p *auth.Principal, book uuid.UUID, q ReportQuery) (any, error) {
		return h.svc.Statement(r.Context(), p, book, q)
	})
}

// Receivables serves GET .../reports/receivables.
func (h *Handler) Receivables(w http.ResponseWriter, r *http.Request) {
	h.report(w, r, func(p *auth.Principal, book uuid.UUID, q ReportQuery) (any, error) {
		return h.svc.Receivables(r.Context(), p, book, q)
	})
}

// Export serves GET .../reports/export.csv?report=.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	f := map[string]string{}
	report := query.Get("report")
	var q ReportQuery
	var flt Filter
	if report == ReportEntries {
		flt = filter(query, f)
	} else {
		q = reportQuery(query, f)
	}
	if len(f) > 0 {
		// The role gate first: an AGENT learns nothing more than 403.
		if err := h.svc.mayExport(p, book); err != nil {
			h.fail(w, r, err)
			return
		}
		h.fail(w, r, &ValidationError{Fields: f})
		return
	}
	out, err := h.svc.ExportCSV(r.Context(), p, book, report, q, flt)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	head := w.Header()
	head.Set("Content-Type", "text/csv; charset=utf-8")
	head.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, out.FileName))
	head.Set("X-Content-Type-Options", "nosniff")
	private(w)
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.WriteAll(out.Rows)
}

// Categories serves GET .../categories?active=.
func (h *Handler) Categories(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	var active *bool
	switch r.URL.Query().Get("active") {
	case "":
	case "true":
		t := true
		active = &t
	case "false":
		f := false
		active = &f
	default:
		h.fail(w, r, &ValidationError{Fields: map[string]string{"active": "must be true or false"}})
		return
	}
	out, err := h.svc.Categories(r.Context(), p, book, active)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"items": out})
}

// CreateCategory serves POST .../categories.
func (h *Handler) CreateCategory(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	var in CategoryInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.CreateCategory(r.Context(), p, book, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, map[string]any{"category": out})
}

// UpdateCategory serves PATCH .../categories/{code}.
func (h *Handler) UpdateCategory(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	var in CategoryInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.UpdateCategory(r.Context(), p, book, chi.URLParam(r, "code"), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"category": out})
}

// Routes mounts the ledger under a router whose path holds {id}.
func (h *Handler) Routes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/options", h.Options)
	r.Get("/entries", h.List)
	r.With(limit).Post("/entries", h.Create)
	r.Get("/entries/{entryId}", h.Get)
	r.With(limit).Post("/entries/{entryId}/reverse", h.Reverse)
	r.With(limit).Post("/entries/{entryId}/correct", h.Correct)
	r.With(limit).Post("/entries/{entryId}/reconcile", h.Reconcile)
	r.With(limit).Post("/entries/{entryId}/notes", h.AddNote)
	r.With(limit).Post("/entries/{entryId}/attachments", h.AddAttachment)
	r.Get("/entries/{entryId}/attachments/{attachmentId}", h.Attachment)
	r.Get("/reports/cashbook", h.Cashbook)
	r.Get("/reports/categories", h.CategoriesReport)
	r.Get("/reports/statement", h.Statement)
	r.Get("/reports/receivables", h.Receivables)
	r.Get("/reports/export.csv", h.Export)
	r.Get("/categories", h.Categories)
	r.With(limit).Post("/categories", h.CreateCategory)
	r.With(limit).Patch("/categories/{code}", h.UpdateCategory)
}
