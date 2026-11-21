package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/storage"
)

// MaintenanceDeps is what the maintenance service is built from.
type MaintenanceDeps struct {
	Store       maintenance.Store
	Tenancies   *tenancies.Service
	Ledger      *ledger.Service
	LedgerStore ledger.Store
	Reference   *reference.Service
	Markets     markets.Store
	Directory   directory.Store
	Enquiries   maintenance.Enquiries
	Files       storage.Storage
	Logger      *slog.Logger
	Now         func() time.Time
}

// MaintenanceService builds maintenance requests (API 0.36.2): the books
// and tenancies of the tenancies service, categories and priorities from
// reference data, costs posted through the ledger, providers from the
// public directory and assignments sent as business enquiries.
func MaintenanceService(d MaintenanceDeps) *maintenance.Service {
	return maintenance.NewService(maintenance.Deps{
		Store: d.Store, Tenancies: d.Tenancies, Ledger: d.Ledger, LedgerEntry: d.LedgerStore.Entry,
		Vocabulary: func(ctx context.Context, set string) ([]maintenance.CodeLabel, error) {
			items, err := d.Reference.Set(ctx, set, "")
			if err != nil {
				return nil, err
			}
			out := make([]maintenance.CodeLabel, 0, len(items))
			for _, it := range items {
				out = append(out, maintenance.CodeLabel{Code: it.Code, Label: it.Label})
			}
			return out, nil
		},
		Currencies: func(ctx context.Context) ([]string, error) {
			all, err := d.Markets.List(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(all))
			for _, m := range all {
				// The main currency and the extra permitted ones (API 0.56.0).
				out = append(out, m.Currencies...)
				if len(m.Currencies) == 0 {
					out = append(out, m.CurrencyCode)
				}
			}
			return out, nil
		},
		Providers: providerDirectory{directory: d.Directory, ref: d.Reference},
		Enquiries: d.Enquiries, Files: d.Files, Logger: d.Logger, Now: d.Now,
	})
}

// providerDirectory adapts the public directory to maintenance.Providers:
// only published, verified profiles.
type providerDirectory struct {
	directory directory.Store
	ref       *reference.Service
}

func (p providerDirectory) option(ctx context.Context, r directory.Record, trades, cities map[string]string) maintenance.ProviderOption {
	out := maintenance.ProviderOption{ID: r.ID, Kind: r.Kind, Slug: r.Slug, Name: r.Name, Verified: r.Verified}
	if r.TradeCode != "" {
		label := trades[r.TradeCode]
		if label == "" {
			label = r.TradeCode
		}
		out.Trade = &maintenance.CodeLabel{Code: r.TradeCode, Label: label}
	}
	city := r.CityName
	if r.CityCode != "" {
		city = cities[r.CityCode]
		if city == "" {
			city = r.CityCode
		}
	}
	if city != "" {
		out.City = &city
	}
	return out
}

func (p providerDirectory) labels(ctx context.Context) (map[string]string, map[string]string, error) {
	if p.ref == nil {
		return map[string]string{}, map[string]string{}, nil
	}
	trades, err := p.ref.Labels(ctx, reference.SetServiceTrades)
	if err != nil {
		return nil, nil, err
	}
	cities, err := p.ref.Labels(ctx, reference.SetCities)
	return trades, cities, err
}

// Provider implements maintenance.Providers.
func (p providerDirectory) Provider(ctx context.Context, id uuid.UUID) (maintenance.ProviderOption, bool, error) {
	if p.directory == nil {
		return maintenance.ProviderOption{}, false, nil
	}
	r, err := p.directory.ByID(ctx, id)
	if errors.Is(err, directory.ErrProfileNotFound) {
		return maintenance.ProviderOption{}, false, nil
	}
	if err != nil {
		return maintenance.ProviderOption{}, false, err
	}
	trades, cities, err := p.labels(ctx)
	if err != nil {
		return maintenance.ProviderOption{}, false, err
	}
	return p.option(ctx, r, trades, cities), true, nil
}

// Search implements maintenance.Providers.
func (p providerDirectory) Search(ctx context.Context, q, trade string, limit int) ([]maintenance.ProviderOption, error) {
	if p.directory == nil {
		return nil, nil
	}
	list, _, err := p.directory.List(ctx, maintenance.ProviderKind, directory.Filter{Q: q, Trade: trade}, limit, 0)
	if err != nil {
		return nil, err
	}
	trades, cities, err := p.labels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]maintenance.ProviderOption, 0, len(list))
	for _, r := range list {
		out = append(out, p.option(ctx, r, trades, cities))
	}
	return out, nil
}

// MaintenanceTenants finds the accounts to tell about a tenancy's requests:
// the active accounts whose verified email is an email of one of its live
// tenant contacts (the matching of GET /v1/me/tenancies).
func MaintenanceTenants(contacts crm.Store, identityStore identity.Store) maintenance.TenantAccounts {
	return func(ctx context.Context, t tenancies.Tenancy) ([]maintenance.Recipient, error) {
		var out []maintenance.Recipient
		seen := map[uuid.UUID]bool{}
		for _, id := range t.PartyIDs(tenancies.RoleTenant) {
			c, err := contacts.Get(ctx, id)
			if errors.Is(err, crm.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if !c.Live() || !tenancyBook(c.Book).Same(t.Book) {
				continue
			}
			for _, email := range c.Emails {
				u, err := identityStore.UserByEmail(ctx, identity.NormalizeEmail(email))
				if errors.Is(err, identity.ErrNotFound) {
					continue
				}
				if err != nil {
					return nil, err
				}
				if !u.Verified() || u.Status != identity.StatusActive || seen[u.ID] {
					continue
				}
				seen[u.ID] = true
				out = append(out, maintenance.Recipient{UserID: u.ID, Name: u.Name, Email: u.Email})
			}
		}
		return out, nil
	}
}

// MaintenanceNotices is the notifications module's source of maintenance
// events.
func MaintenanceNotices(store maintenance.Store, tenancyStore tenancies.Store, contacts crm.Store,
	identityStore identity.Store) func(ctx context.Context, id uuid.UUID) (maintenance.NotificationInfo, error) {
	return maintenance.Notices(store, tenancyStore.Tenancy, MaintenanceTenants(contacts, identityStore))
}
