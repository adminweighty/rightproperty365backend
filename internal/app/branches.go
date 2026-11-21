package app

import (
	"context"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/branches"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// MemoryBranches builds the in-memory branches store (API 0.57.0) and
// connects it to the memory stores that read branches: the identity store
// (a member's pin), the listing and request stores (a listing's branch) and
// the ledger (a branch named by an entry).
func MemoryBranches(ids *identity.MemoryStore, listingStore *listings.MemoryStore, requests *propertyrequests.MemoryStore,
	book *ledger.MemoryStore, audit orgaudit.Sink) *branches.MemoryStore {
	store := branches.NewMemoryStore(branches.MemoryDeps{Audit: audit})
	store.SetDeps(branches.MemoryDeps{
		Audit: audit,
		Member: func(org, user uuid.UUID) (string, bool) {
			name, ok, err := people{identity: ids}.Member(context.Background(), org, user)
			return name, ok && err == nil
		},
		Listing: func(id uuid.UUID) (*uuid.UUID, string, bool) {
			found, err := listingStore.ByIDs(context.Background(), []uuid.UUID{id})
			if err != nil || len(found) == 0 {
				return nil, "", false
			}
			return found[0].OrganisationID, found[0].Reference + " " + found[0].Title, true
		},
		OnListingBranch: func(id uuid.UUID, branch *uuid.UUID) {
			listingStore.SetBranch(id, branch)
			if requests != nil {
				requests.SetListingBranch(id, branch)
			}
		},
	})
	ids.SetBranchLookup(store.BranchOf)
	if book != nil {
		book.SetBranchResolver(func(org uuid.UUID, name string) *uuid.UUID {
			b, err := branches.ResolveName(context.Background(), store, org, name)
			if err != nil || b == nil {
				return nil
			}
			id := b.ID
			return &id
		})
	}
	return store
}
