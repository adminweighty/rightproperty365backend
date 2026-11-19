package tenancies

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Rent error codes.
const (
	CodeDueNotFound     = "RENT_DUE_NOT_FOUND"
	CodeReceiptNotFound = "RENT_RECEIPT_NOT_FOUND"
	CodeDueNotWaivable  = "RENT_DUE_NOT_WAIVABLE"
	CodeDueChanged      = "RENT_DUE_CHANGED"
)

// failRent answers the rent errors; false when err is none of them.
func (h *Handler) failRent(w http.ResponseWriter, r *http.Request, err error) bool {
	var nw *NotWaivableError
	switch {
	case errors.As(err, &nw):
		writeDetails(w, r, http.StatusConflict, CodeDueNotWaivable, "Only a due with something outstanding can be waived.",
			map[string]any{"status": nw.Status})
	case errors.Is(err, ErrDueChanged):
		writeDetails(w, r, http.StatusConflict, CodeDueChanged, "A due changed meanwhile. Reload the schedule and try again.", map[string]any{})
	case errors.Is(err, ErrDueNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeDueNotFound, "This due does not exist in these books.", nil)
	case errors.Is(err, ErrReceiptNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeReceiptNotFound, "This receipt does not exist on this tenancy.", nil)
	default:
		return false
	}
	return true
}

func dueFilter(r *http.Request, f map[string]string) DueFilter {
	q := r.URL.Query()
	out := DueFilter{Status: q.Get("status"), From: strings.TrimSpace(q.Get("from")), To: strings.TrimSpace(q.Get("to"))}
	if out.Status != "" && !slices.Contains(DueStatuses(), out.Status) {
		f["status"] = "must be " + strings.Join(DueStatuses(), ", ")
	}
	if out.From != "" && !validDate(out.From) {
		f["from"] = "must be a date (YYYY-MM-DD)"
	}
	if out.To != "" && !validDate(out.To) {
		f["to"] = "must be a date (YYYY-MM-DD)"
	}
	return out
}

// Dues serves GET .../tenancies/{tenancyId}/dues.
func (h *Handler) Dues(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "tenancyId")
	if !ok {
		return
	}
	f := map[string]string{}
	flt := dueFilter(r, f)
	if len(f) > 0 {
		h.fail(w, r, &ValidationError{Fields: f})
		return
	}
	out, err := h.svc.ListDues(r.Context(), p, book, ids[0], flt)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

// Receipts serves GET .../tenancies/{tenancyId}/receipts.
func (h *Handler) Receipts(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "tenancyId")
	if !ok {
		return
	}
	out, err := h.svc.ListReceipts(r.Context(), p, book, ids[0])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"items": out})
}

// Receipt serves GET .../tenancies/{tenancyId}/receipts/{receiptId}.
func (h *Handler) Receipt(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "tenancyId", "receiptId")
	if !ok {
		return
	}
	out, err := h.svc.GetReceipt(r.Context(), p, book, ids[0], ids[1])
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"receipt": out})
}

// RecordReceipt serves POST .../tenancies/{tenancyId}/receipts.
func (h *Handler) RecordReceipt(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "tenancyId")
	if !ok {
		return
	}
	var in ReceiptInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.RecordReceipt(r.Context(), p, book, ids[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, out)
}

// Waive serves POST .../dues/{dueId}/waive.
func (h *Handler) Waive(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "dueId")
	if !ok {
		return
	}
	var in WaiveInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	out, err := h.svc.WaiveDue(r.Context(), p, book, ids[0], in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"due": out})
}

// Arrears serves GET .../arrears.
func (h *Handler) Arrears(w http.ResponseWriter, r *http.Request) {
	p, book, _, ok := h.begin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := map[string]string{}
	aq := ArrearsQuery{AsOf: strings.TrimSpace(q.Get("as_of")), Currency: q.Get("currency"), PropertyID: optionalID(q, "property_id", f)}
	if len(f) > 0 {
		h.fail(w, r, &ValidationError{Fields: f})
		return
	}
	out, err := h.svc.Arrears(r.Context(), p, book, aq)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

func statementFormat(r *http.Request) (string, bool) {
	f := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if f == "" {
		f = FormatJSON
	}
	return f, f == FormatJSON || f == FormatCSV || f == FormatPDF
}

// writeStatement answers a statement as JSON, CSV or PDF.
func (h *Handler) writeStatement(w http.ResponseWriter, r *http.Request, st Statement, format string) {
	switch format {
	case FormatCSV:
		content, err := StatementCSV(st)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		h.file(w, "text/csv; charset=utf-8", StatementFileName(st, FormatCSV), content)
	case FormatPDF:
		content, err := StatementPDF(st)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		h.file(w, "application/pdf", StatementFileName(st, FormatPDF), content)
	default:
		h.ok(w, http.StatusOK, st)
	}
}

func (h *Handler) file(w http.ResponseWriter, contentType, name string, content []byte) {
	private(w)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func badFormat() error {
	return &ValidationError{Fields: map[string]string{"format": "must be json, csv or pdf"}}
}

// Statement serves GET .../tenancies/{tenancyId}/statement.
func (h *Handler) Statement(w http.ResponseWriter, r *http.Request) {
	p, book, ids, ok := h.begin(w, r, "tenancyId")
	if !ok {
		return
	}
	format, valid := statementFormat(r)
	q := r.URL.Query()
	st, err := h.svc.TenancyStatement(r.Context(), p, book, ids[0], q.Get("from"), q.Get("to"))
	if err == nil && !valid {
		err = badFormat()
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeStatement(w, r, st, format)
}

// rentRoutes mounts the rent routes of a tenancy (under .../tenancies).
func (h *Handler) rentRoutes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/{tenancyId}/dues", h.Dues)
	r.Get("/{tenancyId}/receipts", h.Receipts)
	r.With(limit).Post("/{tenancyId}/receipts", h.RecordReceipt)
	r.Get("/{tenancyId}/receipts/{receiptId}", h.Receipt)
	r.Get("/{tenancyId}/statement", h.Statement)
}

// DueRoutes mounts .../dues under a router whose path holds {id}.
func (h *Handler) DueRoutes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.With(limit).Post("/{dueId}/waive", h.Waive)
}

// ---------------------------------------------------------------------------
// The tenant's own dues, receipts and statement
// ---------------------------------------------------------------------------

func (h *Handler) myID(w http.ResponseWriter, r *http.Request) (*auth.Principal, uuid.UUID, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		h.fail(w, r, ErrUnauthenticated)
		return nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, ErrTenancyNotFound)
		return nil, uuid.Nil, false
	}
	return p, id, true
}

// MyDues serves GET /v1/me/tenancies/{id}/dues.
func (h *Handler) MyDues(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.myID(w, r)
	if !ok {
		return
	}
	f := map[string]string{}
	flt := dueFilter(r, f)
	if len(f) > 0 {
		h.fail(w, r, &ValidationError{Fields: f})
		return
	}
	out, err := h.svc.MyDues(r.Context(), p, id, flt)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

// MyReceipts serves GET /v1/me/tenancies/{id}/receipts.
func (h *Handler) MyReceipts(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.myID(w, r)
	if !ok {
		return
	}
	out, err := h.svc.MyReceipts(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, map[string]any{"items": out})
}

// MyStatement serves GET /v1/me/tenancies/{id}/statement.
func (h *Handler) MyStatement(w http.ResponseWriter, r *http.Request) {
	p, id, ok := h.myID(w, r)
	if !ok {
		return
	}
	format, valid := statementFormat(r)
	q := r.URL.Query()
	st, err := h.svc.MyStatement(r.Context(), p, id, q.Get("from"), q.Get("to"))
	if err == nil && !valid {
		err = badFormat()
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeStatement(w, r, st, format)
}
