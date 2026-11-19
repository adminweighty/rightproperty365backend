package reviews

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore keeps reviews in memory (development and tests). It mirrors
// the Postgres store's guards.
type MemoryStore struct {
	mu      sync.Mutex
	reviews map[uuid.UUID]Review
	reports map[uuid.UUID]Report
	outbox  []outbox.Event
	sink    outbox.Sink
	audit   *adminaudit.MemoryLog
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{reviews: map[uuid.UUID]Review{}, reports: map[uuid.UUID]Report{}}
}

// SetAuditLog receives the administrator audit records this store writes.
func (s *MemoryStore) SetAuditLog(log *adminaudit.MemoryLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = log
}

// SetOutboxSink registers a receiver for outbox events as they are written.
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// Outbox returns the outbox events written so far (tests).
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.outbox)
}

func cloneReview(r Review) Review {
	if r.Reply != nil {
		reply := *r.Reply
		r.Reply = &reply
	}
	return r
}

func (s *MemoryStore) counted(r Review) Review {
	r = cloneReview(r)
	r.OpenReports = 0
	for _, rp := range s.reports {
		if rp.ReviewID == r.ID && rp.Status == ReportOpen {
			r.OpenReports++
		}
	}
	return r
}

// Review implements Store.
func (s *MemoryStore) Review(_ context.Context, id uuid.UUID) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reviews[id]
	if !ok {
		return Review{}, ErrNotFound
	}
	return s.counted(r), nil
}

func sameID(a, b *uuid.UUID) bool { return a != nil && b != nil && *a == *b }

func (f Filter) matches(r Review) bool {
	switch {
	case f.ProfileID != nil && r.ProfileID != *f.ProfileID,
		f.ProviderOrganisationID != nil && r.ProviderOrganisationID != *f.ProviderOrganisationID,
		f.ReviewerKind != "" && r.ReviewerKind != f.ReviewerKind,
		f.ReviewerUserID != nil && !sameID(r.ReviewerUserID, f.ReviewerUserID),
		f.ReviewerOrganisationID != nil && !sameID(r.ReviewerOrganisationID, f.ReviewerOrganisationID),
		len(f.Statuses) > 0 && !slices.Contains(f.Statuses, r.Status),
		f.Queue && r.Status != StatusPending && r.OpenReports == 0,
		f.Reported && r.OpenReports == 0,
		f.BasisKind != "" && r.BasisKind != f.BasisKind,
		f.Rating != 0 && r.Rating != f.Rating:
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Q)); q != "" {
		return strings.Contains(strings.ToLower(r.ProviderName), q) || strings.Contains(strings.ToLower(r.Text), q)
	}
	return true
}

// order sorts a list as Filter.Order says.
func order(list []Review, how string) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		switch how {
		case "published":
			pa, pb := a.CreatedAt, b.CreatedAt
			if a.PublishedAt != nil {
				pa = *a.PublishedAt
			}
			if b.PublishedAt != nil {
				pb = *b.PublishedAt
			}
			if !pa.Equal(pb) {
				return pa.After(pb)
			}
			return a.ID.String() > b.ID.String()
		case "queue":
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.Before(b.CreatedAt)
			}
			return a.ID.String() < b.ID.String()
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID.String() > b.ID.String()
	})
}

// Reviews implements Store.
func (s *MemoryStore) Reviews(_ context.Context, f Filter) ([]Review, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Review
	for _, r := range s.reviews {
		r = s.counted(r)
		if f.matches(r) {
			out = append(out, r)
		}
	}
	order(out, f.Order)
	total := len(out)
	start := min(max(f.Offset, 0), total)
	end := total
	if f.Limit > 0 {
		end = min(start+f.Limit, total)
	}
	return append([]Review{}, out[start:end]...), total, nil
}

// ByBasis implements Store.
func (s *MemoryStore) ByBasis(_ context.Context, kind string, ids []uuid.UUID) (map[uuid.UUID]Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]Review{}
	for _, r := range s.reviews {
		if r.BasisKind == kind && slices.Contains(ids, r.BasisID) {
			out[r.BasisID] = s.counted(r)
		}
	}
	return out, nil
}

// Reports implements Store.
func (s *MemoryStore) Reports(_ context.Context, reviewID uuid.UUID) ([]Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Report{}
	for _, rp := range s.reports {
		if rp.ReviewID == reviewID {
			out = append(out, rp)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out, nil
}

// Ratings implements Store.
func (s *MemoryStore) Ratings(_ context.Context, profileIDs []uuid.UUID) (map[uuid.UUID]Rating, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]Rating{}
	for _, r := range s.reviews {
		if r.Status == StatusPublished && slices.Contains(profileIDs, r.ProfileID) {
			rt := out[r.ProfileID]
			rt.Count++
			rt.Sum += r.Rating
			out[r.ProfileID] = rt
		}
	}
	return out, nil
}

// QueueCounts implements Store.
func (s *MemoryStore) QueueCounts(context.Context) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, reported := 0, 0
	for _, r := range s.reviews {
		if r.Status == StatusPending {
			pending++
		}
		if s.counted(r).OpenReports > 0 {
			reported++
		}
	}
	return pending, reported, nil
}

// Apply implements Store.
func (s *MemoryStore) Apply(_ context.Context, ch Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apply(ch)
}

// Put applies a Change (seed helper).
func (s *MemoryStore) Put(ch Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apply(ch)
}

func (s *MemoryStore) apply(ch Change) error {
	// Checks first: nothing is written when one fails.
	if c := ch.Create; c != nil {
		for _, r := range s.reviews {
			if r.BasisKind == c.BasisKind && r.BasisID == c.BasisID && r.ID != c.ID {
				return ErrBasisReviewed
			}
		}
	}
	if u := ch.Update; u != nil {
		cur, ok := s.reviews[u.ID]
		if !ok {
			return ErrNotFound
		}
		if cur.Version != ch.ExpectVersion {
			return ErrConflict
		}
	}
	for _, rp := range ch.Reports {
		r, ok := s.reviews[rp.ReviewID]
		if !ok || r.Status != StatusPublished {
			return ErrNotFound
		}
		for _, other := range s.reports {
			if other.ReviewID == rp.ReviewID && other.Status == ReportOpen && sameID(other.ReporterUserID, rp.ReporterUserID) {
				return ErrAlreadyReported
			}
		}
	}
	notices := make([][]byte, 0, len(ch.Notices))
	for _, n := range ch.Notices {
		raw, err := json.Marshal(n)
		if err != nil {
			return err
		}
		notices = append(notices, raw)
	}

	if c := ch.Create; c != nil {
		r := cloneReview(*c)
		if r.Version == 0 {
			r.Version = 1
		}
		s.reviews[r.ID] = r
	}
	if u := ch.Update; u != nil {
		r := cloneReview(*u)
		r.Version = ch.ExpectVersion + 1
		s.reviews[r.ID] = r
	}
	if ch.Delete != nil {
		delete(s.reviews, *ch.Delete)
		for id, rp := range s.reports {
			if rp.ReviewID == *ch.Delete {
				delete(s.reports, id)
			}
		}
	}
	if res := ch.Resolve; res != nil {
		for id, rp := range s.reports {
			if rp.ReviewID == res.ReviewID && rp.Status == ReportOpen {
				at, by := res.At, res.By
				rp.Status, rp.ResolvedAt, rp.ResolvedBy, rp.Resolution = ReportResolved, &at, &by, res.Resolution
				s.reports[id] = rp
			}
		}
	}
	for _, rp := range ch.Reports {
		s.reports[rp.ID] = rp
	}
	for _, e := range ch.Audit {
		if s.audit != nil {
			s.audit.Append(e)
		}
	}
	for i, n := range ch.Notices {
		ev := outbox.Event{ID: uuid.New(), Type: OutboxModerated, AggregateType: AggregateReview, AggregateID: n.ReviewID,
			Payload: notices[i], Attempts: 1, CreatedAt: n.OccurredAt}
		s.outbox = append(s.outbox, ev)
		if s.sink != nil {
			s.sink(ev)
		}
	}
	return nil
}

// ForgetUser handles a deleted account (the users row is anonymised, so
// nothing cascades): the person is unlinked everywhere; a review they wrote
// as a customer or direct owner keeps its text and rating with the public
// name "Former customer"; an organisation's review keeps the
// organisation's name and loses its author's name.
func (s *MemoryStore) ForgetUser(userID uuid.UUID, deletedName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.reviews {
		if sameID(r.ReviewerUserID, &userID) {
			r.ReviewerUserID = nil
			if r.ReviewerKind == ReviewerOrganisation {
				r.AuthorName = deletedName
			} else {
				r.ReviewerName, r.AuthorName = FormerCustomer, ""
			}
		}
		if sameID(r.ModeratedBy, &userID) {
			r.ModeratedBy = nil
		}
		if r.Reply != nil && sameID(r.Reply.ByUserID, &userID) {
			r.Reply.ByUserID = nil
		}
		s.reviews[id] = r
	}
	for id, rp := range s.reports {
		if sameID(rp.ReporterUserID, &userID) {
			rp.ReporterUserID = nil
		}
		if sameID(rp.ResolvedBy, &userID) {
			rp.ResolvedBy = nil
		}
		s.reports[id] = rp
	}
}
