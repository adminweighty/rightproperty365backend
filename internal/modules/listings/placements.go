package listings

// Featured and sponsored placements (API 0.53.0, FR-046/FR-047, Handover
// section 8): an administrator grants a listing a FEATURED or SPONSORED
// placement for a period, optionally counted against the advertiser's
// subscription (its MAX_FEATURED_LISTINGS), and may end it. The listing
// shows the active placement (featured, placement_kind); a sync, run at once
// on every change and by the worker, keeps those columns in step with the
// periods and writes LISTING_UPDATED so the search index follows. Every
// grant and end is in the administrator audit log.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
)

// Placement kinds.
const (
	PlacementFeatured  = "FEATURED"
	PlacementSponsored = "SPONSORED"
)

// PlacementKinds returns every kind.
func PlacementKinds() []string { return []string{PlacementFeatured, PlacementSponsored} }

// PlacementLabel is how a kind is shown on the listing.
func PlacementLabel(kind string) string {
	if kind == PlacementSponsored {
		return "Sponsored"
	}
	return "Featured"
}

// PlacementView is the placement a public listing shows.
type PlacementView struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

func placementView(kind string) *PlacementView {
	if kind == "" {
		return nil
	}
	return &PlacementView{Kind: kind, Label: PlacementLabel(kind)}
}

// Placement statuses (derived from the times).
const (
	PlacementActive    = "ACTIVE"
	PlacementScheduled = "SCHEDULED"
	PlacementEnded     = "ENDED"
)

// Limits and timing.
const (
	MaxPlacementDays       = 366
	PlacementSyncInterval  = 5 * time.Minute
	maxPlacementNoteRunes  = 500
	maxPlacementReasonRune = 500
)

// Audit vocabulary.
const (
	ActionPlacementGranted = "LISTING_PLACEMENT_GRANTED"
	ActionPlacementEnded   = "LISTING_PLACEMENT_ENDED"
	TargetListing          = "LISTING"
)

// Placement errors.
var (
	ErrPlacementNotFound     = errors.New("placement not found")
	ErrPlacementOverlap      = errors.New("the listing already has a placement in that period")
	ErrPlacementLimit        = errors.New("the subscription's featured listings are all in use")
	ErrPlacementEnded        = errors.New("the placement has already ended")
	ErrPlacementNotPublished = errors.New("only a published listing can be given a placement")
)

// PlacementRoles may read, grant and end placements (MARKET_ADMIN in its
// own markets).
func PlacementRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformFinanceAdmin, auth.PlatformMarketAdmin}
}

// PlacementMarkets returns the countries whose placements the caller may
// manage: all for the global roles, the MARKET_ADMIN markets otherwise.
func PlacementMarkets(p *auth.Principal) (codes []string, all, ok bool) {
	if p == nil || p.SessionScope != auth.ScopeAdmin {
		return nil, false, false
	}
	for _, r := range p.PlatformRoles {
		switch r.Role {
		case auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformFinanceAdmin:
			return nil, true, true
		case auth.PlatformMarketAdmin:
			if r.MarketCode != "" && !slices.Contains(codes, r.MarketCode) {
				codes = append(codes, r.MarketCode)
			}
		}
	}
	return codes, false, len(codes) > 0
}

// Placement is one placement of a listing.
type Placement struct {
	ID             uuid.UUID
	ListingID      uuid.UUID
	Kind           string
	StartsAt       time.Time
	EndsAt         time.Time
	SubscriptionID *uuid.UUID
	Note           string
	GrantedBy      uuid.UUID
	GrantedByName  string
	GrantedAt      time.Time
	EndedAt        *time.Time
	EndedBy        *uuid.UUID
	EndReason      string

	// The listing, for the admin views.
	ListingSlug      string
	ListingTitle     string
	ListingReference string
	MarketCode       string
}

// Until is when the placement stops: its end, or when it was ended.
func (p Placement) Until() time.Time {
	if p.EndedAt != nil && p.EndedAt.Before(p.EndsAt) {
		return *p.EndedAt
	}
	return p.EndsAt
}

// Status is the placement's status at now.
func (p Placement) Status(now time.Time) string {
	switch {
	case !p.Until().After(now) || (p.EndedAt != nil && !p.EndedAt.After(p.StartsAt)):
		return PlacementEnded
	case p.StartsAt.After(now):
		return PlacementScheduled
	}
	return PlacementActive
}

// PlacementFilter selects placements for the admin list.
type PlacementFilter struct {
	ListingID *uuid.UUID
	Status    string
	// Markets limits to those countries; nil is every country.
	Markets  []string
	Now      time.Time
	Page     int
	PageSize int
}

// PlacementQuota checks a subscription a placement is tied to: it must be
// ACTIVE and belong to the listing's advertiser. It returns the plan's
// MAX_FEATURED_LISTINGS (nil: no limit) or a field message.
type PlacementQuota func(ctx context.Context, subscriptionID uuid.UUID, r Record) (limit *int, problem string, err error)

// PlacementStore persists placements.
type PlacementStore interface {
	ListPlacements(ctx context.Context, f PlacementFilter) ([]Placement, int, error)
	GetPlacement(ctx context.Context, id uuid.UUID) (Placement, error)
	// GrantPlacement writes the placement and its audit event, refusing an
	// overlap on the listing (ErrPlacementOverlap) and, when limit is set,
	// more than *limit placements tied to the subscription that overlap the
	// period (ErrPlacementLimit), all under the listing's lock; then syncs
	// the listing.
	GrantPlacement(ctx context.Context, p Placement, limit *int, audit adminaudit.Event, now time.Time) error
	// EndPlacement records the end (ErrPlacementEnded when over) with its
	// audit event, then syncs the listing.
	EndPlacement(ctx context.Context, id uuid.UUID, by uuid.UUID, reason string, audit func(Placement) adminaudit.Event, now time.Time) (Placement, error)
	// SyncPlacements makes every listing that has placements show exactly
	// its active one, writing LISTING_UPDATED for each listing changed.
	SyncPlacements(ctx context.Context, now time.Time) (int, error)
}

// PlacementInput is the body of POST /v1/admin/placements.
type PlacementInput struct {
	ListingID      string  `json:"listing_id"`
	Kind           string  `json:"kind"`
	StartsAt       *string `json:"starts_at"`
	EndsAt         string  `json:"ends_at"`
	SubscriptionID *string `json:"subscription_id"`
	Note           string  `json:"note"`
}

// Placements is the placement service.
type Placements struct {
	store    PlacementStore
	listings Store
	quota    PlacementQuota
	now      func() time.Time
}

// NewPlacements builds the service. quota may be nil (tying to a
// subscription is then refused).
func NewPlacements(store PlacementStore, listings Store, quota PlacementQuota, now func() time.Time) *Placements {
	if now == nil {
		now = time.Now
	}
	return &Placements{store: store, listings: listings, quota: quota, now: now}
}

func (s *Placements) clock() time.Time { return s.now().UTC().Truncate(time.Second) }

// inMarket reports whether the caller may manage a listing of that market.
func inMarket(p *auth.Principal, market string) bool {
	codes, all, ok := PlacementMarkets(p)
	return ok && (all || slices.Contains(codes, market))
}

// List returns the placements the caller may see.
func (s *Placements) List(ctx context.Context, p *auth.Principal, f PlacementFilter) ([]Placement, int, error) {
	codes, all, ok := PlacementMarkets(p)
	if !ok {
		return []Placement{}, 0, nil
	}
	if !all {
		f.Markets = codes
	}
	f.Now = s.clock()
	return s.store.ListPlacements(ctx, f)
}

// Grant gives a listing a placement.
func (s *Placements) Grant(ctx context.Context, p *auth.Principal, listingID uuid.UUID, in PlacementInput) (Placement, error) {
	now := s.clock()
	found, err := s.listings.ByIDs(ctx, []uuid.UUID{listingID})
	if err != nil {
		return Placement{}, err
	}
	if len(found) == 0 || !inMarket(p, found[0].MarketCode) {
		return Placement{}, ErrNotFound
	}
	r := found[0]
	f := map[string]string{}
	if !slices.Contains(PlacementKinds(), in.Kind) {
		f["kind"] = "must be FEATURED or SPONSORED"
	}
	starts := now
	if in.StartsAt != nil && strings.TrimSpace(*in.StartsAt) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*in.StartsAt))
		switch {
		case err != nil:
			f["starts_at"] = "must be an RFC 3339 time"
		case t.Before(now.Add(-time.Minute)):
			f["starts_at"] = "must not be in the past"
		default:
			starts = t.UTC().Truncate(time.Second)
		}
	}
	ends, err := time.Parse(time.RFC3339, strings.TrimSpace(in.EndsAt))
	switch {
	case err != nil:
		f["ends_at"] = "is required, as an RFC 3339 time"
	case !ends.After(starts):
		f["ends_at"] = "must be after the start"
	case ends.Sub(starts) > MaxPlacementDays*24*time.Hour:
		f["ends_at"] = fmt.Sprintf("must be at most %d days after the start", MaxPlacementDays)
	}
	note := strings.Join(strings.Fields(in.Note), " ")
	if len([]rune(note)) > maxPlacementNoteRunes {
		f["note"] = fmt.Sprintf("must be at most %d characters", maxPlacementNoteRunes)
	}
	var sub *uuid.UUID
	var limit *int
	if in.SubscriptionID != nil && strings.TrimSpace(*in.SubscriptionID) != "" {
		id, err := uuid.Parse(strings.TrimSpace(*in.SubscriptionID))
		switch {
		case err != nil:
			f["subscription_id"] = "must be a UUID"
		case s.quota == nil:
			f["subscription_id"] = "cannot be checked here"
		default:
			l, problem, err := s.quota(ctx, id, r)
			if err != nil {
				return Placement{}, err
			}
			if problem != "" {
				f["subscription_id"] = problem
			} else {
				sub, limit = &id, l
				if limit == nil {
					unlimited := -1
					limit = &unlimited
				}
			}
		}
	}
	if len(f) > 0 {
		return Placement{}, &ValidationError{Fields: f}
	}
	if r.Status != StatusPublished || !r.ExpiresAt.After(now) {
		return Placement{}, ErrPlacementNotPublished
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Placement{}, err
	}
	pl := Placement{ID: id, ListingID: r.ID, Kind: in.Kind, StartsAt: starts, EndsAt: ends.UTC().Truncate(time.Second), SubscriptionID: sub,
		Note: note, GrantedBy: p.UserID, GrantedByName: p.Name, GrantedAt: now,
		ListingSlug: r.Slug, ListingTitle: r.Title, ListingReference: r.Reference, MarketCode: r.MarketCode}
	after := map[string]any{"placement_id": pl.ID.String(), "kind": pl.Kind, "starts_at": pl.StartsAt, "ends_at": pl.EndsAt}
	if sub != nil {
		after["subscription_id"] = sub.String()
	}
	audit := adminaudit.New(p.UserID, p.Name, ActionPlacementGranted, TargetListing, r.ID.String(), r.Reference+" "+r.Title, note, nil, after, now)
	if limit != nil && *limit < 0 {
		limit = nil
	}
	if err := s.store.GrantPlacement(ctx, pl, limit, audit, now); err != nil {
		return Placement{}, err
	}
	return pl, nil
}

// End ends a placement now (a scheduled one is cancelled).
func (s *Placements) End(ctx context.Context, p *auth.Principal, id uuid.UUID, reason string) (Placement, error) {
	now := s.clock()
	cur, err := s.store.GetPlacement(ctx, id)
	if errors.Is(err, ErrPlacementNotFound) || (err == nil && !inMarket(p, cur.MarketCode)) {
		return Placement{}, ErrPlacementNotFound
	}
	if err != nil {
		return Placement{}, err
	}
	reason = strings.Join(strings.Fields(reason), " ")
	switch {
	case reason == "":
		return Placement{}, &ValidationError{Fields: map[string]string{"reason": "is required"}}
	case len([]rune(reason)) > maxPlacementReasonRune:
		return Placement{}, &ValidationError{Fields: map[string]string{"reason": "must be at most 500 characters"}}
	}
	return s.store.EndPlacement(ctx, id, p.UserID, reason, func(pl Placement) adminaudit.Event {
		return adminaudit.New(p.UserID, p.Name, ActionPlacementEnded, TargetListing, pl.ListingID.String(), pl.ListingReference+" "+pl.ListingTitle, reason,
			map[string]any{"placement_id": pl.ID.String(), "kind": pl.Kind, "ends_at": pl.EndsAt}, map[string]any{"ended_at": now}, now)
	}, now)
}

// Sync runs one placement sync (the worker).
func (s *Placements) Sync(ctx context.Context) (int, error) {
	return s.store.SyncPlacements(ctx, s.clock())
}
