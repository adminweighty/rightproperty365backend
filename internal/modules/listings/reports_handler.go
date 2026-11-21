package listings

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Report error codes.
const (
	CodeReportNotFound = "REPORT_NOT_FOUND"
	CodeReportDecided  = "REPORT_DECIDED"
)

// ReportHandler serves reporting and the moderation queue.
type ReportHandler struct {
	svc    *Reports
	logger *slog.Logger
}

// NewReportHandler builds the handler.
func NewReportHandler(svc *Reports, logger *slog.Logger) *ReportHandler {
	return &ReportHandler{svc: svc, logger: logger}
}

type reportListingView struct {
	ID          uuid.UUID `json:"id"`
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Reference   string    `json:"reference"`
	Status      string    `json:"status"`
	MarketCode  string    `json:"market_code"`
	OpenReports int       `json:"open_reports"`
}

type reportView struct {
	ID           uuid.UUID         `json:"id"`
	Listing      reportListingView `json:"listing"`
	Reason       string            `json:"reason"`
	Note         *string           `json:"note"`
	Reporter     string            `json:"reporter"`
	Status       string            `json:"status"`
	CreatedAt    time.Time         `json:"created_at"`
	DecidedBy    *string           `json:"decided_by"`
	DecidedAt    *time.Time        `json:"decided_at"`
	DecisionNote *string           `json:"decision_note"`
}

func viewReport(r ListingReport) reportView {
	who := "guest"
	if r.ReporterUserID != nil {
		who = "signed-in user"
	}
	return reportView{ID: r.ID, Listing: reportListingView{ID: r.ListingID, Slug: r.ListingSlug, Title: r.ListingTitle, Reference: r.ListingReference,
		Status: r.ListingStatus, MarketCode: r.MarketCode, OpenReports: r.OpenReports},
		Reason: r.Reason, Note: opt(r.Note), Reporter: who, Status: r.Status, CreatedAt: r.CreatedAt,
		DecidedBy: opt(r.DecidedByName), DecidedAt: r.DecidedAt, DecisionNote: opt(r.DecisionNote)}
}

func (h *ReportHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	var serr *StateError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &serr):
		httpx.WriteError(w, r, http.StatusConflict, serr.Code, serr.Message, nil)
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrUnavailable):
		httpx.WriteError(w, r, http.StatusNotFound, CodeListingNotFound, "This listing does not exist.", nil)
	case errors.Is(err, ErrReportNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeReportNotFound, "This report does not exist.", nil)
	case errors.Is(err, ErrReportDecided):
		httpx.WriteError(w, r, http.StatusConflict, CodeReportDecided, "This report was already decided.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

// Report serves POST /v1/listings/{slug}/reports (anyone).
func (h *ReportHandler) Report(w http.ResponseWriter, r *http.Request) {
	var in ReportInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	var reporter *uuid.UUID
	if p := auth.FromContext(r.Context()); p != nil && p.SessionScope != auth.ScopeAdmin {
		id := p.UserID
		reporter = &id
	}
	if err := h.svc.Report(r.Context(), reporter, chi.URLParam(r, "slug"), in); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]bool{"received": true})
}

// List serves GET /v1/admin/listing-reports.
func (h *ReportHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ReportFilter{Status: q.Get("status"), Page: 1, PageSize: 50}
	switch f.Status {
	case "", ReportOpen, ReportDismissed, ReportActioned:
	default:
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.",
			map[string]string{"status": "must be OPEN, DISMISSED or ACTIONED"})
		return
	}
	if raw := q.Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			f.Page = n
		}
	}
	list, total, err := h.svc.List(r.Context(), auth.FromContext(r.Context()), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]reportView, 0, len(list))
	for _, rep := range list {
		items = append(items, viewReport(rep))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": f.Page, "page_size": f.PageSize})
}

// Decide serves POST /v1/admin/listing-reports/{id}/{dismiss|unpublish|warn}.
func (h *ReportHandler) Decide(decision string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			h.fail(w, r, ErrReportNotFound)
			return
		}
		var in struct {
			Note    string `json:"note"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if !httpx.DecodeJSON(w, r, &in) {
			return
		}
		text := map[string]string{ReportDecisionDismiss: in.Note, ReportDecisionUnpublish: in.Reason, ReportDecisionWarn: in.Message}[decision]
		rep, err := h.svc.Decide(r.Context(), auth.FromContext(r.Context()), id, decision, text)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"report": viewReport(rep)})
	}
}
