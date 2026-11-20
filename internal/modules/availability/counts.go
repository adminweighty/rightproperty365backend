package availability

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/auth"
)

// StaleCalendarDays is how long a published short stay's calendar may go
// unchanged before the lister's dashboard asks them to look at it.
const StaleCalendarDays = 30

// ListingCounts are the numbers a lister's dashboard shows about the
// listings they may manage (the same listings as GET /v1/me/listings).
type ListingCounts struct {
	Total    int
	ByStatus map[string]int
	// ShortStays is the number of short stays among them; StaleCalendars the
	// PUBLISHED ones whose calendar was never set or has not changed for
	// StaleCalendarDays days.
	ShortStays     int
	StaleCalendars int
}

// ListingCounts counts the caller's listings. A caller who manages no
// listings gets zeros.
func (s *Service) ListingCounts(ctx context.Context, p *auth.Principal) (ListingCounts, error) {
	out := ListingCounts{ByStatus: map[string]int{}}
	if !p.ManagesListings() {
		return out, nil
	}
	var seller *uuid.UUID
	if p.IsDirectOwner() {
		id := p.UserID
		seller = &id
	}
	records, err := s.listings.Owned(ctx, p.ListingOrganisationIDs(), seller)
	if err != nil {
		return ListingCounts{}, fmt.Errorf("load listings: %w", err)
	}
	var published []uuid.UUID
	now := s.clock()
	for _, r := range records {
		if !p.MayManageListing(r.OrganisationID, r.SellerUserID) {
			continue
		}
		// The effective status: a PUBLISHED listing past its period is
		// EXPIRED, whether or not the expiry sweep has stored it yet.
		status := r.EffectiveStatus(now)
		out.Total++
		out.ByStatus[status]++
		if r.Category == listings.CategoryShortStay {
			out.ShortStays++
			if status == listings.StatusPublished {
				published = append(published, r.ID)
			}
		}
	}
	if len(published) == 0 {
		return out, nil
	}
	states, err := s.store.States(ctx, published)
	if err != nil {
		return ListingCounts{}, fmt.Errorf("load calendars: %w", err)
	}
	cutoff := s.clock().AddDate(0, 0, -StaleCalendarDays)
	for _, id := range published {
		if updated := states[id].UpdatedAt; updated == nil || updated.Before(cutoff) {
			out.StaleCalendars++
		}
	}
	return out, nil
}
