package app

import (
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/packages"
	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// MemoryPackages builds the listing packages' memory store (API 0.37.0) over
// the memory listing and identity stores, and makes the listing store check
// the quota on a go-live (the memory twin of packages.EnforceTx).
func MemoryPackages(listingStore *listings.MemoryStore, people *identity.MemoryStore, audit *adminaudit.MemoryLog) *packages.MemoryStore {
	store := packages.NewMemoryStore(audit)
	store.SetDeps(packages.MemoryDeps{
		LiveListings:   listingStore.LiveListings,
		ListingMarkets: listingStore.ListingMarkets,
		AccountName: func(acct packages.Account) (string, bool) {
			if acct.Kind == packages.KindOrganisation {
				o, ok := people.Organisation(acct.ID)
				return o.Name, ok
			}
			u, ok := people.Account(acct.ID)
			if !ok || u.Status == identity.StatusDeleted {
				return "", false
			}
			return u.Name, true
		},
		HomeMarket: func(organisationID uuid.UUID) string {
			o, _ := people.Organisation(organisationID)
			return o.MarketCode
		},
	})
	listingStore.SetQuota(func(acct packages.Account, market string, now time.Time) packages.Resolved {
		return store.ResolveNow(acct, market, now)
	})
	return store
}
