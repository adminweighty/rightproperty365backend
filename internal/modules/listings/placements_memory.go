package listings

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// SetPlacementAudit records the placement audit events in log (memory).
func (s *MemoryStore) SetPlacementAudit(log *adminaudit.MemoryLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.placementAudit = log
}

func (s *MemoryStore) placementWithListing(p Placement) Placement {
	if i := s.index(p.ListingID); i >= 0 {
		r := s.records[i]
		p.ListingSlug, p.ListingTitle, p.ListingReference, p.MarketCode = r.Slug, r.Title, r.Reference, r.MarketCode
	}
	return p
}

// ListPlacements implements PlacementStore.
func (s *MemoryStore) ListPlacements(_ context.Context, f PlacementFilter) ([]Placement, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := []Placement{}
	for _, p := range s.placements {
		p = s.placementWithListing(p)
		if (f.ListingID != nil && p.ListingID != *f.ListingID) || (f.Status != "" && p.Status(f.Now) != f.Status) ||
			(f.Markets != nil && !slices.Contains(f.Markets, p.MarketCode)) {
			continue
		}
		all = append(all, p)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].StartsAt.After(all[j].StartsAt) })
	total := len(all)
	size := max(f.PageSize, 1)
	from := min(max(f.Page-1, 0)*size, total)
	return all[from:min(from+size, total)], total, nil
}

// GetPlacement implements PlacementStore.
func (s *MemoryStore) GetPlacement(_ context.Context, id uuid.UUID) (Placement, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.placements {
		if p.ID == id {
			return s.placementWithListing(p), nil
		}
	}
	return Placement{}, ErrPlacementNotFound
}

func overlaps(p Placement, from, until time.Time) bool {
	return p.StartsAt.Before(until) && from.Before(p.Until()) && p.Until().After(p.StartsAt)
}

// GrantPlacement implements PlacementStore.
func (s *MemoryStore) GrantPlacement(_ context.Context, p Placement, limit *int, audit adminaudit.Event, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tied := 0
	for _, o := range s.placements {
		if o.ListingID == p.ListingID && overlaps(o, p.StartsAt, p.EndsAt) {
			return ErrPlacementOverlap
		}
		if limit != nil && o.SubscriptionID != nil && p.SubscriptionID != nil && *o.SubscriptionID == *p.SubscriptionID && overlaps(o, p.StartsAt, p.EndsAt) {
			tied++
		}
	}
	if limit != nil && tied >= *limit {
		return ErrPlacementLimit
	}
	s.placements = append(s.placements, p)
	if s.placementAudit != nil {
		s.placementAudit.Append(audit)
	}
	s.syncPlacements(now)
	return nil
}

// EndPlacement implements PlacementStore.
func (s *MemoryStore) EndPlacement(_ context.Context, id uuid.UUID, by uuid.UUID, reason string, audit func(Placement) adminaudit.Event, now time.Time) (Placement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.placements {
		if p.ID != id {
			continue
		}
		if p.Status(now) == PlacementEnded {
			return Placement{}, ErrPlacementEnded
		}
		at := now
		p.EndedAt, p.EndedBy, p.EndReason = &at, &by, reason
		s.placements[i] = p
		p = s.placementWithListing(p)
		if s.placementAudit != nil {
			s.placementAudit.Append(audit(p))
		}
		s.syncPlacements(now)
		return p, nil
	}
	return Placement{}, ErrPlacementNotFound
}

// SyncPlacements implements PlacementStore.
func (s *MemoryStore) SyncPlacements(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncPlacements(now), nil
}

// syncPlacements applies the active placements; callers hold the lock.
func (s *MemoryStore) syncPlacements(now time.Time) int {
	want := map[uuid.UUID]string{}
	for _, p := range s.placements {
		if _, seen := want[p.ListingID]; !seen {
			want[p.ListingID] = ""
		}
		if p.Status(now) == PlacementActive {
			want[p.ListingID] = p.Kind
		}
	}
	changed := 0
	for id, kind := range want {
		i := s.index(id)
		if i < 0 || (s.records[i].PlacementKind == kind && s.records[i].Featured == (kind != "")) {
			continue
		}
		s.records[i].PlacementKind, s.records[i].Featured = kind, kind != ""
		s.record(id, Change{Events: []ChangeEvent{indexEvent(EventListingUpdated, id)}}, now)
		changed++
	}
	return changed
}
