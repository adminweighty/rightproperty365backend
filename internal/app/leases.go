package app

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
)

// leaseListings gives the lease module a listing, whatever its status, with
// its place as the public sees it (listings have no street address).
type leaseListings struct{ svc *listings.Service }

func (l leaseListings) LeaseListing(ctx context.Context, id uuid.UUID) (leases.ListingInfo, error) {
	r, err := l.svc.Record(ctx, id)
	if errors.Is(err, listings.ErrNotFound) {
		return leases.ListingInfo{}, leases.ErrListingNotFound
	}
	if err != nil {
		return leases.ListingInfo{}, err
	}
	shown, err := l.svc.Present(ctx, []listings.Record{r})
	if err != nil {
		return leases.ListingInfo{}, err
	}
	var parts []string
	if len(shown) == 1 {
		p := shown[0]
		if p.Suburb != nil && p.Suburb.Label != "" {
			parts = append(parts, p.Suburb.Label)
		}
		if p.City.Label != "" {
			parts = append(parts, p.City.Label)
		}
		if p.Region != nil && p.Region.Label != "" {
			parts = append(parts, p.Region.Label)
		}
		if p.Country.Name != "" {
			parts = append(parts, p.Country.Name)
		}
	}
	return leases.ListingInfo{ID: r.ID, Reference: r.Reference, Title: r.Title, MarketCode: r.MarketCode, OrganisationID: r.OrganisationID,
		AgentUserID: r.AgentUserID, SellerUserID: r.SellerUserID, Address: strings.Join(parts, ", ")}, nil
}

// countryNames looks a country's name up.
func countryNames(store markets.Store) func(ctx context.Context, code string) (string, bool, error) {
	return func(ctx context.Context, code string) (string, bool, error) {
		all, err := store.List(ctx)
		if err != nil {
			return "", false, err
		}
		for _, m := range all {
			if m.Code == code {
				return m.Name, true, nil
			}
		}
		return "", false, nil
	}
}

// personOf returns an account's current name and email address.
func personOf(store identity.Store) func(ctx context.Context, id uuid.UUID) (string, string, bool, error) {
	return func(ctx context.Context, id uuid.UUID) (string, string, bool, error) {
		u, err := store.UserByID(ctx, id)
		if errors.Is(err, identity.ErrNotFound) {
			return "", "", false, nil
		}
		if err != nil {
			return "", "", false, err
		}
		return u.Name, u.Email, true, nil
	}
}
