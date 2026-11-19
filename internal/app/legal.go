package app

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/legal"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/tax"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/i18n"
)

// Per-market legal documents, tax rules and translations (API 0.43.0,
// BR-001): the wiring.

// knownMarket reports whether code is an enabled country.
func knownMarket(store markets.Store) func(ctx context.Context, code string) (bool, error) {
	return func(ctx context.Context, code string) (bool, error) {
		all, err := store.List(ctx)
		if err != nil {
			return false, err
		}
		for _, m := range all {
			if m.Code == code {
				return true, nil
			}
		}
		return false, nil
	}
}

// MemoryLegalAcceptances keeps the legal acceptances in the in-memory
// identity store's consent records, where registration writes the privacy
// notice and the terms (development and tests).
type MemoryLegalAcceptances struct{ Identity *identity.MemoryStore }

// Acceptances implements legal.AcceptanceStore.
func (m MemoryLegalAcceptances) Acceptances(_ context.Context, userID uuid.UUID) ([]legal.Acceptance, error) {
	out := []legal.Acceptance{}
	for _, c := range m.Identity.Consents() {
		if c.UserID == userID {
			out = append(out, legal.Acceptance{UserID: c.UserID, Kind: legal.KindOfConsent(c.ConsentType), Version: c.Version,
				MarketCode: c.MarketCode, Context: c.Context, AcceptedAt: c.AcceptedAt})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AcceptedAt.After(out[j].AcceptedAt) })
	return out, nil
}

// Accept implements legal.AcceptanceStore.
func (m MemoryLegalAcceptances) Accept(_ context.Context, a legal.Acceptance) error {
	m.Identity.AddConsent(identity.MemoryConsent{UserID: a.UserID, ConsentType: legal.ConsentType(a.Kind), Version: a.Version,
		AcceptedAt: a.AcceptedAt, MarketCode: a.MarketCode, Context: a.Context})
	return nil
}

// legalStores picks the legal and tax stores: the given ones, or seeded
// memory stores writing to the in-memory audit log.
func legalStores(d Deps) (legal.Store, legal.AcceptanceStore, tax.Store) {
	memAudit, _ := d.AdminAudit.(*adminaudit.MemoryLog)
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	docs := d.Legal
	if docs == nil {
		docs = legal.NewMemoryStore(legal.SeedVersions(now), memAudit)
	}
	accepts := d.LegalAcceptances
	if accepts == nil {
		if mem, ok := d.Identity.(*identity.MemoryStore); ok {
			accepts = MemoryLegalAcceptances{Identity: mem}
		} else if pg, ok := docs.(legal.AcceptanceStore); ok {
			accepts = pg
		}
	}
	rules := d.Tax
	if rules == nil {
		rules = tax.NewMemoryStore(tax.SeedRules(now), memAudit)
	}
	return docs, accepts, rules
}

// languagesHandler serves GET /v1/i18n/languages (public, cacheable).
func languagesHandler(w http.ResponseWriter, r *http.Request) {
	httpx.WriteCacheableJSON(w, r, struct {
		Default   string          `json:"default"`
		Languages []i18n.Language `json:"languages"`
	}{Default: i18n.DefaultLanguage, Languages: i18n.Languages()}, time.Hour, time.Hour)
}
