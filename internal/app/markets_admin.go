package app

import (
	"context"

	"rightproperty365.com/backend/internal/modules/legal"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/packages"
)

// everyMarket lists every market whatever its status (API 0.56.0): the
// administrators' tools (listing packages, legal documents, tax rules)
// must accept a PLANNED country so it can be prepared before it goes live.
// Public readers keep markets.Store.List (ACTIVE countries only).
type everyMarket struct{ store markets.Store }

func (e everyMarket) List(ctx context.Context) ([]markets.Market, error) {
	if all, ok := e.store.(markets.AdminStore); ok {
		return all.All(ctx)
	}
	return e.store.List(ctx)
}

// marketChecker answers the activation checks of a market.
type marketChecker struct {
	packages packages.Store
	legal    *legal.Service
}

// PackageActive implements markets.Checker.
func (c marketChecker) PackageActive(ctx context.Context, market, code string) (bool, error) {
	list, err := c.packages.Packages(ctx, packages.PackageFilter{Market: market})
	if err != nil {
		return false, err
	}
	for _, p := range list {
		if p.Code == code && p.Active {
			return true, nil
		}
	}
	return false, nil
}

// LegalSource implements markets.Checker.
func (c marketChecker) LegalSource(ctx context.Context, market, kind string) (string, error) {
	r, err := c.legal.Current(ctx, kind, market, "en")
	if err == legal.ErrNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if r.Resolution.FallbackMarket {
		return "GLOBAL", nil
	}
	return "MARKET", nil
}
