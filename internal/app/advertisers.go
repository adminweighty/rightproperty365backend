package app

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
)

// MemoryAdvertisers returns the function the in-memory listing store uses to
// resolve who advertises a listing at the moment it is read — the in-memory
// counterpart of the joins the Postgres adapter makes (listings/postgres.go,
// recordFrom), with the same rules:
//
//   - a business listing is verified when the organisation has a published,
//     verified profile of its own;
//   - a direct owner's listing is verified when the owner's account is ACTIVE
//     and their PRIVATE_SELLER or LANDLORD role is approved; the owner is
//     named by their account name;
//   - a profile that is not public is named but has no logo.
//
// Give it to listings.MemoryStore.SetAdvertisers. Without it the store keeps
// the advertiser copied onto each record, which is only good for unit tests.
func MemoryAdvertisers(profiles *directory.MemoryStore, accounts *identity.MemoryStore) func(listings.Record) listings.Advertiser {
	return func(r listings.Record) listings.Advertiser {
		var agent *listings.ProfileRef
		if r.AgentUserID != nil {
			if p, ok := profiles.AgentProfile(*r.AgentUserID); ok {
				ref := directory.RefOf(p)
				agent = &ref
			}
		}
		switch {
		case r.OrganisationID != nil:
			a := listings.Advertiser{Type: listings.AdvertiserAgency, Agent: agent}
			if o, ok := accounts.Organisation(*r.OrganisationID); ok {
				a.Name, a.OrganisationKind = o.Name, o.Kind
			}
			if p, ok := profiles.OwnProfile(*r.OrganisationID); ok {
				ref := directory.RefOf(p)
				a.Agency, a.Name, a.Verified = &ref, p.Name, ref.Verified
				if a.Verified {
					a.VerifiedAt = p.VerifiedAt
				}
			}
			return a
		case r.SellerUserID == nil && agent != nil:
			// An agent without a business: nobody verified stands behind it.
			return listings.Advertiser{Type: listings.AdvertiserAgency, Agent: agent}
		}
		a := listings.Advertiser{Type: listings.AdvertiserPrivateSeller, Name: listings.DefaultSellerName}
		if r.SellerUserID == nil {
			return a
		}
		if u, ok := accounts.Account(*r.SellerUserID); ok && u.Status == identity.StatusActive {
			a.Name = u.Name
			for _, role := range u.AccountRoles {
				if role.Status == identity.AccountRoleActive && slices.Contains(auth.DirectOwnerRoles(), role.Role) {
					// The memory store keeps no approval time: the account's own.
					created := u.CreatedAt
					a.Verified, a.VerifiedAt = true, &created
				}
			}
		}
		return a
	}
}

// verifiedListings wraps the request store so that a listing whose
// advertiser is not verified takes no property request, appointment or
// request to stay: it is reported as not found, exactly as the public
// catalogue reports it. Everything else passes through.
type verifiedListings struct {
	propertyrequests.Store
	catalogue *listings.Service
}

// GetListing implements propertyrequests.Store.
func (v verifiedListings) GetListing(ctx context.Context, id uuid.UUID) (propertyrequests.Listing, error) {
	l, err := v.Store.GetListing(ctx, id)
	if err != nil {
		return l, err
	}
	// A listing the catalogue does not know (it has no public page at all)
	// is left to the request store's own status rule.
	verified, known, err := v.catalogue.AdvertiserVerified(ctx, id)
	if err != nil {
		return propertyrequests.Listing{}, err
	}
	if known && !verified {
		return propertyrequests.Listing{}, propertyrequests.ErrListingNotFound
	}
	return l, nil
}

// ListingRecord returns the function the listing review emails read their
// listing with (notifications.Handlers.SetListingSource): the stored record,
// whatever its status, or listings.ErrNotFound.
func ListingRecord(store listings.Store) func(ctx context.Context, id uuid.UUID) (listings.Record, error) {
	return func(ctx context.Context, id uuid.UUID) (listings.Record, error) {
		records, err := store.ByIDs(ctx, []uuid.UUID{id})
		if err != nil {
			return listings.Record{}, err
		}
		if len(records) == 0 {
			return listings.Record{}, listings.ErrNotFound
		}
		return records[0], nil
	}
}
