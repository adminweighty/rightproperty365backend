package listings

// Listing reports and their moderation (API 0.54.0, BR-018): anyone may
// report a public listing; the listing reviewers dismiss a report, warn the
// advertiser, or take the listing down (CHANGES_REQUESTED with the reason:
// it leaves the public views and the advertiser may fix and resubmit it).
// Every decision is in the administrator audit log.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
)

// Report reasons.
const (
	ReportFraud        = "FRAUD"
	ReportMisleading   = "MISLEADING"
	ReportSoldOrLet    = "ALREADY_SOLD_OR_LET"
	ReportWrongPrice   = "WRONG_PRICE"
	ReportDuplicate    = "DUPLICATE"
	ReportOffensive    = "OFFENSIVE"
	ReportOther        = "OTHER"
	maxReportNoteRunes = 1000
)

// ReportReasons returns every reason.
func ReportReasons() []string {
	return []string{ReportFraud, ReportMisleading, ReportSoldOrLet, ReportWrongPrice, ReportDuplicate, ReportOffensive, ReportOther}
}

// Report statuses.
const (
	ReportOpen      = "OPEN"
	ReportDismissed = "DISMISSED"
	ReportActioned  = "ACTIONED"
)

// Report decisions and their audit actions.
const (
	ReportDecisionDismiss   = "dismiss"
	ReportDecisionUnpublish = "unpublish"
	ReportDecisionWarn      = "warn"

	ActionReportDismissed     = "LISTING_REPORT_DISMISSED"
	ActionUnpublishedOnReport = "LISTING_UNPUBLISHED_ON_REPORT"
	ActionWarnedOnReport      = "LISTING_WARNED_ON_REPORT"
)

// EventListingWarned emails the advertiser a moderator's warning.
const EventListingWarned = "LISTING_WARNED"

// Report errors.
var (
	ErrReportNotFound = errors.New("report not found")
	ErrReportDecided  = errors.New("the report was already decided")
)

// ListingReport is one report.
type ListingReport struct {
	ID             uuid.UUID
	ListingID      uuid.UUID
	Reason         string
	Note           string
	ReporterUserID *uuid.UUID
	Status         string
	DecidedBy      *uuid.UUID
	DecidedByName  string
	DecidedAt      *time.Time
	DecisionNote   string
	CreatedAt      time.Time

	// The listing, for the queue.
	ListingSlug      string
	ListingTitle     string
	ListingReference string
	ListingStatus    string
	MarketCode       string
	OpenReports      int
}

// ReportFilter selects reports for the queue.
type ReportFilter struct {
	Status   string
	Markets  []string // nil: every country
	Page     int
	PageSize int
}

// ReportStore persists reports.
type ReportStore interface {
	// CreateReport stores a report; with a reporter who already has an OPEN
	// report on the listing, nothing is written (created=false).
	CreateReport(ctx context.Context, r ListingReport) (created bool, err error)
	ListReports(ctx context.Context, f ReportFilter) ([]ListingReport, int, error)
	GetReport(ctx context.Context, id uuid.UUID) (ListingReport, error)
	// DecideReports closes the report (and, with allOpen, every OPEN report
	// of its listing) with status, writing the audit event in the same
	// transaction. ErrReportDecided when the report is not OPEN.
	DecideReports(ctx context.Context, id uuid.UUID, allOpen bool, status string, by uuid.UUID, byName, note string, audit adminaudit.Event, now time.Time) (ListingReport, error)
}

// ReportInput is the body of POST /v1/listings/{slug}/reports.
type ReportInput struct {
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

// Reports is the report service.
type Reports struct {
	store   ReportStore
	svc     *Service
	manager *Manager
	logger  *slog.Logger
}

// NewReports builds the service.
func NewReports(store ReportStore, svc *Service, manager *Manager, logger *slog.Logger) *Reports {
	return &Reports{store: store, svc: svc, manager: manager, logger: logger}
}

func (s *Reports) clock() time.Time { return s.svc.now().UTC().Truncate(time.Microsecond) }

// Report records a report on a public listing; reporter is nil for a guest.
func (s *Reports) Report(ctx context.Context, reporter *uuid.UUID, slug string, in ReportInput) error {
	f := map[string]string{}
	if !slices.Contains(ReportReasons(), in.Reason) {
		f["reason"] = "must be one of " + strings.Join(ReportReasons(), ", ")
	}
	note := strings.TrimSpace(in.Note)
	switch {
	case len([]rune(note)) > maxReportNoteRunes:
		f["note"] = fmt.Sprintf("must be at most %d characters", maxReportNoteRunes)
	case strings.ContainsFunc(note, func(r rune) bool { return r < 0x20 && r != '\n' && r != '\t' }):
		f["note"] = "must not contain control characters"
	case in.Reason == ReportOther && note == "":
		f["note"] = "is required when the reason is OTHER"
	}
	if len(f) > 0 {
		return &ValidationError{Fields: f}
	}
	r, err := s.svc.PublicRecord(ctx, slug)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	created, err := s.store.CreateReport(ctx, ListingReport{ID: id, ListingID: r.ID, Reason: in.Reason, Note: note, ReporterUserID: reporter,
		Status: ReportOpen, CreatedAt: s.clock()})
	if err != nil {
		return err
	}
	// Identifiers only: a report's note is personal data.
	s.logger.InfoContext(ctx, "listing reported", slog.String("listing_id", r.ID.String()), slog.String("reason", in.Reason), slog.Bool("created", created))
	return nil
}

// List returns the queue the moderator may see.
func (s *Reports) List(ctx context.Context, p *auth.Principal, f ReportFilter) ([]ListingReport, int, error) {
	codes, all, ok := p.ListingReviewMarkets()
	if !ok {
		return []ListingReport{}, 0, nil
	}
	if !all {
		f.Markets = codes
	}
	return s.store.ListReports(ctx, f)
}

// Decide records a moderator's decision on a report.
func (s *Reports) Decide(ctx context.Context, p *auth.Principal, id uuid.UUID, decision, text string) (ListingReport, error) {
	rep, err := s.store.GetReport(ctx, id)
	if errors.Is(err, ErrReportNotFound) || (err == nil && !p.MayReviewListing(rep.MarketCode)) {
		return ListingReport{}, ErrReportNotFound
	}
	if err != nil {
		return ListingReport{}, err
	}
	if rep.Status != ReportOpen {
		return ListingReport{}, ErrReportDecided
	}
	field := map[string]string{ReportDecisionDismiss: "note", ReportDecisionUnpublish: "reason", ReportDecisionWarn: "message"}[decision]
	text = strings.TrimSpace(text)
	switch {
	case field == "":
		return ListingReport{}, &ValidationError{Fields: map[string]string{"decision": "must be dismiss, unpublish or warn"}}
	case text == "":
		return ListingReport{}, &ValidationError{Fields: map[string]string{field: "is required"}}
	case len([]rune(text)) > MaxReasonLength:
		return ListingReport{}, &ValidationError{Fields: map[string]string{field: fmt.Sprintf("must be at most %d characters", MaxReasonLength)}}
	}
	now := s.clock()
	status, action, allOpen := ReportDismissed, ActionReportDismissed, false
	switch decision {
	case ReportDecisionUnpublish:
		if err := s.manager.ModeratorUnpublish(ctx, p, rep.ListingID, text); err != nil {
			return ListingReport{}, err
		}
		status, action, allOpen = ReportActioned, ActionUnpublishedOnReport, true
	case ReportDecisionWarn:
		if err := s.manager.Warn(ctx, p, rep.ListingID, text); err != nil {
			return ListingReport{}, err
		}
		status, action, allOpen = ReportActioned, ActionWarnedOnReport, true
	}
	audit := adminaudit.New(p.UserID, p.Name, action, TargetListing, rep.ListingID.String(), rep.ListingReference+" "+rep.ListingTitle, text,
		map[string]any{"report_id": rep.ID.String(), "reason": rep.Reason}, map[string]any{"report_status": status}, now)
	return s.store.DecideReports(ctx, id, allOpen, status, p.UserID, p.Name, text, audit, now)
}

// ModeratorUnpublish takes a PUBLISHED or PAUSED listing out of the public
// views: it becomes CHANGES_REQUESTED with the reason, the advertiser is
// emailed (the changes-requested email) and may fix and resubmit it.
func (mg *Manager) ModeratorUnpublish(ctx context.Context, p *auth.Principal, id uuid.UUID, reason string) error {
	m, err := mg.reviewable(ctx, p, id)
	if err != nil {
		return err
	}
	if m.Status != StatusPublished && m.Status != StatusPaused {
		return conflict("Only a published or paused listing can be taken down.")
	}
	if reason, err = checkReason(reason); err != nil {
		return err
	}
	now := mg.clock()
	expect, from := m.UpdatedAt, m.Status
	m.Status, m.ReviewReason, m.Revision, m.UpdatedAt = StatusChangesRequested, reason, nil, now
	ch := Change{Events: []ChangeEvent{indexEvent(EventListingUnpublished, m.ID), reviewEvent(EventListingChangesRequested, m, ReviewKindNew, reason, now)}}
	ch.Entry = mg.entry(m, p, ActorReviewer, ActionChangesRequested, from, m.Status, ReviewKindNew, reason, now)
	if err := mg.save(ctx, m, expect, ch); err != nil {
		return err
	}
	mg.audit(ctx, "listing.unpublished_on_report", p, m.ID)
	return nil
}

// Warn emails the listing's advertiser a moderator's message; the listing
// stays as it is.
func (mg *Manager) Warn(ctx context.Context, p *auth.Principal, id uuid.UUID, message string) error {
	m, err := mg.reviewable(ctx, p, id)
	if err != nil {
		return err
	}
	if message, err = checkReason(message); err != nil {
		return err
	}
	now := mg.clock()
	if err := mg.save(ctx, m, m.UpdatedAt, Change{Events: []ChangeEvent{reviewEvent(EventListingWarned, m, "", message, now)}}); err != nil {
		return err
	}
	mg.audit(ctx, "listing.warned_on_report", p, m.ID)
	return nil
}
