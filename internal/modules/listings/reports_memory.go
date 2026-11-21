package listings

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

func (s *MemoryStore) reportWithListing(r ListingReport) ListingReport {
	if i := s.index(r.ListingID); i >= 0 {
		rec := s.records[i]
		r.ListingSlug, r.ListingTitle, r.ListingReference, r.MarketCode = rec.Slug, rec.Title, rec.Reference, rec.MarketCode
		r.ListingStatus = rec.Status
	}
	r.OpenReports = 0
	for _, o := range s.reports {
		if o.ListingID == r.ListingID && o.Status == ReportOpen {
			r.OpenReports++
		}
	}
	return r
}

// CreateReport implements ReportStore.
func (s *MemoryStore) CreateReport(_ context.Context, r ListingReport) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ReporterUserID != nil {
		for _, o := range s.reports {
			if o.ListingID == r.ListingID && o.Status == ReportOpen && o.ReporterUserID != nil && *o.ReporterUserID == *r.ReporterUserID {
				return false, nil
			}
		}
	}
	s.reports = append(s.reports, r)
	return true, nil
}

// ListReports implements ReportStore.
func (s *MemoryStore) ListReports(_ context.Context, f ReportFilter) ([]ListingReport, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := []ListingReport{}
	for _, r := range s.reports {
		r = s.reportWithListing(r)
		if (f.Status != "" && r.Status != f.Status) || (f.Markets != nil && !slices.Contains(f.Markets, r.MarketCode)) {
			continue
		}
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	total := len(all)
	size := max(f.PageSize, 1)
	from := min(max(f.Page-1, 0)*size, total)
	return all[from:min(from+size, total)], total, nil
}

// GetReport implements ReportStore.
func (s *MemoryStore) GetReport(_ context.Context, id uuid.UUID) (ListingReport, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.reports {
		if r.ID == id {
			return s.reportWithListing(r), nil
		}
	}
	return ListingReport{}, ErrReportNotFound
}

// DecideReports implements ReportStore.
func (s *MemoryStore) DecideReports(_ context.Context, id uuid.UUID, allOpen bool, status string, by uuid.UUID, byName, note string, audit adminaudit.Event, now time.Time) (ListingReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.reports, func(r ListingReport) bool { return r.ID == id })
	if i < 0 {
		return ListingReport{}, ErrReportNotFound
	}
	if s.reports[i].Status != ReportOpen {
		return ListingReport{}, ErrReportDecided
	}
	listing := s.reports[i].ListingID
	for j := range s.reports {
		if j == i || (allOpen && s.reports[j].ListingID == listing && s.reports[j].Status == ReportOpen) {
			at, who := now, by
			s.reports[j].Status, s.reports[j].DecidedBy, s.reports[j].DecidedByName, s.reports[j].DecidedAt, s.reports[j].DecisionNote = status, &who, byName, &at, note
		}
	}
	if s.placementAudit != nil {
		s.placementAudit.Append(audit)
	}
	return s.reportWithListing(s.reports[i]), nil
}
