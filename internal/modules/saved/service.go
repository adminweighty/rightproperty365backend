package saved

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
)

// Service implements saving, removing and listing saved listings.
type Service struct {
	store     Store
	catalogue Catalogue
	now       func() time.Time
}

// NewService builds the Service. now nil means time.Now.
func NewService(store Store, catalogue Catalogue, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, catalogue: catalogue, now: now}
}

// customer returns the caller's user ID: any customer-scope session may
// save, whatever its roles; an admin-scope session may not.
func customer(p *auth.Principal) (uuid.UUID, error) {
	switch {
	case p == nil:
		return uuid.Nil, ErrUnauthenticated
	case p.SessionScope != auth.ScopeCustomer:
		return uuid.Nil, ErrForbidden
	}
	return p.UserID, nil
}

func (s *Service) at() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// Save saves a listing that is public right now. Saving again is a no-op
// that keeps the first saved_at (Created false).
func (s *Service) Save(ctx context.Context, p *auth.Principal, listingID uuid.UUID) (SaveResult, error) {
	userID, err := customer(p)
	if err != nil {
		return SaveResult{}, err
	}
	states, err := s.catalogue.CardStates(ctx, []uuid.UUID{listingID})
	if err != nil {
		return SaveResult{}, fmt.Errorf("load listing: %w", err)
	}
	st, ok := states[listingID]
	if !ok || !st.Public {
		return SaveResult{}, ErrListingNotFound
	}
	at := s.at()
	stored, created, err := s.store.Save(ctx, Saved{UserID: userID, ListingID: listingID, SavedAt: at, Snapshot: SnapshotOf(st.Listing), SnapshotAt: at})
	if err != nil {
		return SaveResult{}, err
	}
	return SaveResult{Saved: true, SavedAt: stored.SavedAt.UTC(), Created: created}, nil
}

// Remove removes a save. It succeeds whether or not the listing was saved
// and whatever the listing's status.
func (s *Service) Remove(ctx context.Context, p *auth.Principal, listingID uuid.UUID) error {
	userID, err := customer(p)
	if err != nil {
		return err
	}
	_, err = s.store.Remove(ctx, userID, listingID, s.at())
	return err
}

// List returns one page of the caller's saved listings, newest first. A
// listing that is public is shown with its current card (and its snapshot
// is refreshed); any other is shown from its snapshot, without a slug.
func (s *Service) List(ctx context.Context, p *auth.Principal, page, size int) (Page, error) {
	userID, err := customer(p)
	if err != nil {
		return Page{}, err
	}
	rows, total, err := s.store.List(ctx, userID, size, (page-1)*size)
	if err != nil {
		return Page{}, err
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, x := range rows {
		ids = append(ids, x.ListingID)
	}
	states, err := s.catalogue.CardStates(ctx, ids)
	if err != nil {
		return Page{}, fmt.Errorf("load listings: %w", err)
	}
	out := Page{Items: make([]Item, 0, len(rows)), Page: page, PageSize: size, Total: total}
	if total > 0 {
		out.TotalPages = (total + size - 1) / size
	}
	at := s.at()
	for _, x := range rows {
		item := Item{ListingID: x.ListingID, SavedAt: x.SavedAt.UTC()}
		st, known := states[x.ListingID]
		if known && st.Public {
			item.Available, item.Listing = true, SnapshotOf(st.Listing)
			if !sameSnapshot(item.Listing, x.Snapshot) {
				if err := s.store.UpdateSnapshot(ctx, userID, x.ListingID, item.Listing, at); err != nil {
					return Page{}, err
				}
			}
		} else {
			reason := ReasonWithdrawn // a listing the catalogue no longer has
			if known {
				reason = ReasonFor(st.Status)
			}
			label := ReasonLabel(reason)
			item.UnavailableReason, item.UnavailableLabel = &reason, &label
			item.Listing = x.Snapshot
			// Never a way back to the listing or into search.
			item.Listing.Slug = nil
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// IDs returns the IDs of the caller's saved listings that are public right
// now, newest first, at most MaxIDs.
func (s *Service) IDs(ctx context.Context, p *auth.Principal) ([]uuid.UUID, error) {
	userID, err := customer(p)
	if err != nil {
		return nil, err
	}
	all, err := s.store.ListingIDs(ctx, userID)
	if err != nil {
		return nil, err
	}
	states, err := s.catalogue.CardStates(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("load listings: %w", err)
	}
	out := []uuid.UUID{}
	for _, id := range all {
		if st, ok := states[id]; ok && st.Public {
			out = append(out, id)
			if len(out) == MaxIDs {
				break
			}
		}
	}
	return out, nil
}

func sameSnapshot(a, b Snapshot) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}
