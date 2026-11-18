package crm

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	CodeContactNotFound   = "CONTACT_NOT_FOUND"
	CodeContactDuplicate  = "CONTACT_DUPLICATE"
	CodeContactMerged     = "CONTACT_MERGED"
	CodeContactConflict   = "CONTACT_CHANGED"
	CodeReminderNotFound  = "CONTACT_REMINDER_NOT_FOUND"
	CodeListingNotFound   = "LISTING_NOT_FOUND"
	CodeRequestNotFound   = "REQUEST_NOT_FOUND"
	CodeImportTooManyRows = "IMPORT_TOO_MANY_ROWS"
)

// Paging of GET .../contacts.
const (
	DefaultPageSize = 25
	MaxPageSize     = 100
)

// Handler serves /v1/agent/organisations/{id}/contacts. The organisation
// (book) is the route's {id}; a contact is {contactId}.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

// duplicateBody is the 409 CONTACT_DUPLICATE answer: the error envelope
// with error.details.duplicates ([{id, name}]).
type duplicateBody struct {
	Error   duplicateError `json:"error"`
	TraceID string         `json:"trace_id"`
}

type duplicateError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details struct {
		Duplicates []DuplicateRef `json:"duplicates"`
	} `json:"details"`
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	var dup *DuplicateError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &dup):
		body := duplicateBody{Error: duplicateError{Code: CodeContactDuplicate, Message: "A contact with the same email address or phone number already exists. " +
			"Open it, merge, or send ?allow_duplicate=true to keep both."}, TraceID: logging.RequestID(r.Context())}
		body.Error.Details.Duplicates = dup.Contacts
		httpx.WriteJSON(w, http.StatusConflict, body)
	case errors.Is(err, ErrUnauthenticated):
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeContactNotFound, "This contact does not exist.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Your role does not allow this.", nil)
	case errors.Is(err, ErrMerged):
		httpx.WriteError(w, r, http.StatusConflict, CodeContactMerged, "This contact was merged into another one; change that one instead.", nil)
	case errors.Is(err, ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, CodeContactConflict, "The contact was changed by somebody else at the same time. Please try again.", nil)
	case errors.Is(err, ErrReminderNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeReminderNotFound, "This reminder does not exist.", nil)
	case errors.Is(err, ErrListingNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeListingNotFound, "This listing does not exist in your organisation.", nil)
	case errors.Is(err, ErrItemNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeRequestNotFound, "This request, enquiry or appointment does not exist in your inbox.", nil)
	case errors.Is(err, ErrImportTooManyRows):
		httpx.WriteError(w, r, http.StatusBadRequest, CodeImportTooManyRows, fmt.Sprintf("A file may hold at most %d contacts.", MaxImportRows), nil)
	case errors.Is(err, ErrImportTooLarge):
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge, "The file is too large (2 MB at most).", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func private(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
}

// ids parses the book and (when named) other path IDs. A malformed ID is
// the same 404 as an unknown one.
func ids(r *http.Request, names ...string) ([]uuid.UUID, bool) {
	out := make([]uuid.UUID, 0, len(names)+1)
	for _, n := range append([]string{"id"}, names...) {
		id, err := uuid.Parse(chi.URLParam(r, n))
		if err != nil {
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}

func (h *Handler) parse(w http.ResponseWriter, r *http.Request, names ...string) (*auth.Principal, []uuid.UUID, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		h.fail(w, r, ErrUnauthenticated)
		return nil, nil, false
	}
	got, ok := ids(r, names...)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return nil, nil, false
	}
	return p, got, true
}

func allowDuplicate(r *http.Request) bool {
	v := r.URL.Query().Get("allow_duplicate")
	return v == "true" || v == "1"
}

// List serves GET /v1/agent/organisations/{id}/contacts.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := ListFilter{Q: q.Get("q"), Type: q.Get("type"), Stage: q.Get("stage"), Tag: q.Get("tag"), Sort: q.Get("sort")}
	bad := map[string]string{}
	if f.Sort == "" {
		f.Sort = SortLastContact
	} else if f.Sort != SortLastContact && f.Sort != SortName && f.Sort != SortCreated {
		bad["sort"] = "must be last_contact, name or created"
	}
	if f.Stage != "" && !validStage(f.Stage) {
		bad["stage"] = "must be one of " + strings.Join(Stages(), ", ")
	}
	if !ResolveOwnerFilter(p, q.Get("owner"), &f) {
		bad["owner"] = "must be a user ID, me or none"
	}
	switch v := q.Get("has_reminder_due"); v {
	case "", "false":
	case "true":
		now := h.svc.clock()
		f.ReminderDueBefore = &now
	default:
		bad["has_reminder_due"] = "must be true or false"
	}
	page, size := 1, DefaultPageSize
	if raw := q.Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 100000 {
			bad["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := q.Get("page_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > MaxPageSize {
			bad["page_size"] = "must be a whole number from 1 to " + strconv.Itoa(MaxPageSize)
		} else {
			size = n
		}
	}
	if len(bad) > 0 {
		// The book is checked first: another organisation never learns more than 404.
		if _, err := h.svc.access(r.Context(), p, got[0]); err != nil {
			h.fail(w, r, err)
			return
		}
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", bad)
		return
	}
	out, err := h.svc.List(r.Context(), p, got[0], f, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Create serves POST .../contacts[?allow_duplicate=true].
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r)
	if !ok {
		return
	}
	if _, err := h.svc.access(r.Context(), p, got[0]); err != nil {
		h.fail(w, r, err)
		return
	}
	var in CreateInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.Create(r.Context(), p, got[0], in, allowDuplicate(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// Get serves GET .../contacts/{contactId}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	out, err := h.svc.Get(r.Context(), p, got[0], got[1])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// decodeFor checks the book (404 first) and then decodes the body.
func (h *Handler) decodeFor(w http.ResponseWriter, r *http.Request, p *auth.Principal, book uuid.UUID, dst any) bool {
	if _, err := h.svc.access(r.Context(), p, book); err != nil {
		h.fail(w, r, err)
		return false
	}
	return httpx.DecodeJSON(w, r, dst)
}

// Patch serves PATCH .../contacts/{contactId}[?allow_duplicate=true].
func (h *Handler) Patch(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	var in PatchInput
	if !h.decodeFor(w, r, p, got[0], &in) {
		return
	}
	out, err := h.svc.Patch(r.Context(), p, got[0], got[1], in, allowDuplicate(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Delete serves DELETE .../contacts/{contactId}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), p, got[0], got[1]); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AddNote serves POST .../contacts/{contactId}/notes {text}.
func (h *Handler) AddNote(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if !h.decodeFor(w, r, p, got[0], &in) {
		return
	}
	out, err := h.svc.AddNote(r.Context(), p, got[0], got[1], in.Text)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// Notes serves GET .../contacts/{contactId}/notes.
func (h *Handler) Notes(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	out, err := h.svc.Notes(r.Context(), p, got[0], got[1])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// AddReminder serves POST .../contacts/{contactId}/reminders {due_at, text}.
func (h *Handler) AddReminder(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	var in struct {
		DueAt *time.Time `json:"due_at"`
		Text  string     `json:"text"`
	}
	if !h.decodeFor(w, r, p, got[0], &in) {
		return
	}
	out, err := h.svc.AddReminder(r.Context(), p, got[0], got[1], in.DueAt, in.Text)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// PatchReminder serves PATCH .../contacts/{contactId}/reminders/{reminderId} {done}.
func (h *Handler) PatchReminder(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	rid, err := uuid.Parse(chi.URLParam(r, "reminderId"))
	var in struct {
		Done *bool `json:"done"`
	}
	if !h.decodeFor(w, r, p, got[0], &in) {
		return
	}
	if err != nil {
		if _, err := h.svc.Get(r.Context(), p, got[0], got[1]); err != nil {
			h.fail(w, r, err)
			return
		}
		h.fail(w, r, ErrReminderNotFound)
		return
	}
	out, err := h.svc.PatchReminder(r.Context(), p, got[0], got[1], rid, in.Done)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Stage serves POST .../contacts/{contactId}/stage {stage, note?}.
func (h *Handler) Stage(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	var in struct {
		Stage string `json:"stage"`
		Note  string `json:"note"`
	}
	if !h.decodeFor(w, r, p, got[0], &in) {
		return
	}
	out, err := h.svc.ChangeStage(r.Context(), p, got[0], got[1], in.Stage, in.Note)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Owner serves POST .../contacts/{contactId}/owner {user_id}.
func (h *Handler) Owner(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	var in struct {
		UserID Optional[uuid.UUID] `json:"user_id"`
	}
	if !h.decodeFor(w, r, p, got[0], &in) {
		return
	}
	if !in.UserID.Set {
		if _, err := h.svc.Get(r.Context(), p, got[0], got[1]); err != nil {
			h.fail(w, r, err)
			return
		}
		h.fail(w, r, &ValidationError{Fields: map[string]string{"user_id": "is required (null gives the contact to nobody)"}})
		return
	}
	out, err := h.svc.ChangeOwner(r.Context(), p, got[0], got[1], in.UserID.Value)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// LinkListing serves POST .../contacts/{contactId}/listings {listing_id}.
func (h *Handler) LinkListing(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	var in struct {
		ListingID *uuid.UUID `json:"listing_id"`
	}
	if !h.decodeFor(w, r, p, got[0], &in) {
		return
	}
	if in.ListingID == nil {
		if _, err := h.svc.Get(r.Context(), p, got[0], got[1]); err != nil {
			h.fail(w, r, err)
			return
		}
		h.fail(w, r, &ValidationError{Fields: map[string]string{"listing_id": "is required"}})
		return
	}
	out, err := h.svc.LinkListing(r.Context(), p, got[0], got[1], *in.ListingID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// UnlinkListing serves DELETE .../contacts/{contactId}/listings/{listingId}.
func (h *Handler) UnlinkListing(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	lid, err := uuid.Parse(chi.URLParam(r, "listingId"))
	if err != nil {
		lid = uuid.Nil
	}
	if err := h.svc.UnlinkListing(r.Context(), p, got[0], got[1], lid); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Timeline serves GET .../contacts/{contactId}/timeline.
func (h *Handler) Timeline(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r, "contactId")
	if !ok {
		return
	}
	out, err := h.svc.Timeline(r.Context(), p, got[0], got[1])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// Merge serves POST .../contacts/merge {keep_id, merge_ids}.
func (h *Handler) Merge(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r)
	if !ok {
		return
	}
	var in struct {
		KeepID   uuid.UUID   `json:"keep_id"`
		MergeIDs []uuid.UUID `json:"merge_ids"`
	}
	if !h.decodeFor(w, r, p, got[0], &in) {
		return
	}
	out, err := h.svc.Merge(r.Context(), p, got[0], in.KeepID, in.MergeIDs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Import serves POST .../contacts/import[?dry_run=true]: multipart/form-data
// with the CSV in the field "file".
func (h *Handler) Import(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r)
	if !ok {
		return
	}
	a, err := h.svc.access(r.Context(), p, got[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !a.manage || a.book.OrganisationID == nil {
		h.fail(w, r, ErrForbidden)
		return
	}
	dry := false
	switch v := r.URL.Query().Get("dry_run"); v {
	case "", "false", "0":
	case "true", "1":
		dry = true
	default:
		h.fail(w, r, &ValidationError{Fields: map[string]string{"dry_run": "must be true or false"}})
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.fail(w, r, ErrImportTooLarge)
			return
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			h.fail(w, r, &ValidationError{Fields: map[string]string{"file": "is required: a CSV file in the multipart field \"file\""}})
			return
		}
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, httpx.CodeUnsupportedMediaType, "Content-Type must be multipart/form-data.", nil)
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxImportBytes+1))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.svc.Import(r.Context(), p, got[0], raw, dry)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	status := http.StatusOK
	if !dry && out.Summary.Created != nil && *out.Summary.Created > 0 {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, out)
}

// cell neutralises spreadsheet formulas: a value that starts with = + - @
// (or a tab or carriage return) is prefixed with an apostrophe. The import
// removes it again.
func cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// Export serves GET .../contacts/export.csv.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.Export(r.Context(), p, got[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "text/csv; charset=utf-8")
	hd.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="contacts-%s.csv"`, h.svc.clock().Format("2006-01-02")))
	hd.Set("X-Total-Count", strconv.Itoa(len(rows)))
	private(w)
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.Write(ExportHeader())
	for _, rec := range rows {
		for i := range rec {
			rec[i] = cell(rec[i])
		}
		if err := cw.Write(rec); err != nil {
			h.logger.ErrorContext(r.Context(), "contact export failed part way", slog.String("error", err.Error()))
			return
		}
	}
	cw.Flush()
}

// FromRequest serves POST .../contacts/from-request/{requestId}[?kind=&type=]:
// 201 with a new contact, or 200 with the existing contact it was linked to.
func (h *Handler) FromRequest(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r)
	if !ok {
		return
	}
	itemID, err := uuid.Parse(chi.URLParam(r, "requestId"))
	if err != nil {
		if _, err := h.svc.access(r.Context(), p, got[0]); err != nil {
			h.fail(w, r, err)
			return
		}
		h.fail(w, r, ErrItemNotFound)
		return
	}
	q := r.URL.Query()
	out, created, err := h.svc.FromItem(r.Context(), p, got[0], itemID, q.Get("kind"), q.Get("type"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, out)
}

// Tags serves GET .../contacts/tags.
func (h *Handler) Tags(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Tags(r.Context(), p, got[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// Assignees serves GET .../contacts/assignees.
func (h *Handler) Assignees(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Assignees(r.Context(), p, got[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// Reminders serves GET .../contacts/reminders?due=today|overdue|week[&mine=true].
func (h *Handler) Reminders(w http.ResponseWriter, r *http.Request) {
	p, got, ok := h.parse(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	out, err := h.svc.DueReminders(r.Context(), p, got[0], q.Get("due"), q.Get("mine") == "true")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out, "total": len(out)})
}
