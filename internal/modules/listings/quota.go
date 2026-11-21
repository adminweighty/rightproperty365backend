package listings

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/packages"
)

// Listing packages and quotas (API 0.37.0, FR-008). The only transition that
// makes a listing publicly live is the approval of a NEW review (a first
// publication, or a relist of a listing published before): Decide sets
// Change.Quota on it, and the store checks the account's package in the same
// transaction as the write. Submit and relist are never blocked; the views
// carry the account's usage and a warning when it is at its limit.

// Quota is the package usage behind the listing views and the quota error.
type Quota interface {
	Usage(ctx context.Context, acct packages.Account, market string) (packages.MarketUsage, error)
	MarketName(ctx context.Context, code string) string
	AccountName(ctx context.Context, acct packages.Account) string
}

// SetQuota supplies the package usage of the views (nil: none shown).
func (mg *Manager) SetQuota(q Quota) { mg.quota = q }

// quotaCheck is the Change.Quota of a go-live of m.
func quotaCheck(m ManagedListing, now time.Time) *QuotaCheck {
	acct, ok := packages.AccountOf(m.OrganisationID, m.SellerUserID)
	if !ok || m.MarketCode == "" {
		return nil
	}
	return &QuotaCheck{Account: acct, Market: m.MarketCode, Now: now}
}

// warnStatuses are the statuses a listing view warns in when the account is
// at its limit: the listing is not live and may still be approved later.
var warnStatuses = []string{StatusDraft, StatusPendingReview, StatusChangesRequested, StatusArchived, StatusExpired}

// usage fills the package usage and the warning of a view.
func (mg *Manager) usage(ctx context.Context, v *ManagedView, m ManagedListing, status string, reviewer bool) error {
	if mg.quota == nil {
		return nil
	}
	acct, ok := packages.AccountOf(m.OrganisationID, m.SellerUserID)
	if !ok || m.MarketCode == "" {
		return nil
	}
	u, err := mg.quota.Usage(ctx, acct, m.MarketCode)
	if err != nil {
		return err
	}
	v.PackageUsage = &u
	v.QuotaWarning = packages.Warning(&u, !slices.Contains(warnStatuses, status), reviewer)
	return nil
}

// QuotaAnswer is the message and details of 409 LISTING_QUOTA_REACHED.
func (mg *Manager) QuotaAnswer(ctx context.Context, e *packages.QuotaError) (string, map[string]any) {
	market, name := e.MarketCode, ""
	if mg.quota != nil {
		market, name = mg.quota.MarketName(ctx, e.MarketCode), mg.quota.AccountName(ctx, e.Account)
	}
	return packages.QuotaMessage(e, market), packages.QuotaDetails(e, name)
}

// SetQuota makes SaveListing check the listing packages on a go-live, with
// resolve returning the package of an account in a market (the packages
// memory store's ResolveNow).
func (s *MemoryStore) SetQuota(resolve func(acct packages.Account, market string, now time.Time) packages.Resolved) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quota = resolve
}

func accountMatches(r Record, acct packages.Account) bool {
	got, ok := packages.AccountOf(r.OrganisationID, r.SellerUserID)
	return ok && got == acct
}

// liveLocked returns the expiries of the account's active listings in the
// market, leaving out exclude. Caller holds the lock.
func (s *MemoryStore) liveLocked(acct packages.Account, market string, exclude uuid.UUID, now time.Time) []time.Time {
	out := []time.Time{}
	for _, r := range s.records {
		if r.ID != exclude && r.MarketCode == market && r.HoldsSlot(now) && accountMatches(r, acct) {
			out = append(out, r.ExpiresAt.UTC())
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out
}

// enforceLocked is packages.EnforceTx for the memory store. Caller holds
// the lock, which serialises every write as the advisory lock does.
func (s *MemoryStore) enforceLocked(q *QuotaCheck, listingID uuid.UUID) error {
	if q == nil || s.quota == nil {
		return nil
	}
	r := s.quota(q.Account, q.Market, q.Now)
	if r.Package == nil {
		return nil
	}
	live := s.liveLocked(q.Account, q.Market, listingID, q.Now)
	if len(live) >= r.Package.ListingLimit {
		return &packages.QuotaError{Limit: r.Package.ListingLimit, Used: len(live), Package: *r.Package, MarketCode: q.Market, Account: q.Account}
	}
	return nil
}

// LiveListings returns the expiries of the account's active listings in the
// market, earliest first (the packages memory store's reader).
func (s *MemoryStore) LiveListings(acct packages.Account, market string, now time.Time) []time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.liveLocked(acct, market, uuid.Nil, now)
}

// ListingMarkets returns the markets the account has listings in.
func (s *MemoryStore) ListingMarkets(acct packages.Account) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []string{}
	for _, r := range s.records {
		if r.MarketCode != "" && accountMatches(r, acct) && !slices.Contains(out, r.MarketCode) {
			out = append(out, r.MarketCode)
		}
	}
	return out
}
