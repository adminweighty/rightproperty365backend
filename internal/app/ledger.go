package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/platform/storage"
)

// LedgerService builds the agent ledger's service: entries may be linked to
// the book's own contacts (CRM), listings and leases, and use the ISO 4217
// currencies of the countries in reference data.
func LedgerService(store ledger.Store, files storage.Storage, contacts crm.Store, listingStore listings.Store, leaseStore leases.Store,
	marketStore markets.Store, logger *slog.Logger, now func() time.Time) *ledger.Service {
	return ledger.NewService(ledger.Deps{
		Store: store, Files: files, Logger: logger, Now: now, Zone: MarketZone(marketStore),
		Links: ledgerLinks{contacts: contacts, listings: listingStore, leases: leaseStore},
		Currencies: func(ctx context.Context) ([]string, error) {
			all, err := marketStore.List(ctx)
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
	})
}

// ledgerLinks resolves an entry's links, inside its own book only.
type ledgerLinks struct {
	contacts crm.Store
	listings listings.Store
	leases   leases.Store
}

func crmBook(b ledger.Book) crm.Book {
	if b.OrganisationID != nil {
		return crm.OrganisationBook(*b.OrganisationID)
	}
	return crm.PersonalBook(b.ID())
}

// Contact implements ledger.Links: a live contact of the same book.
func (l ledgerLinks) Contact(ctx context.Context, book ledger.Book, id uuid.UUID) (ledger.ContactRef, bool, error) {
	if l.contacts == nil {
		return ledger.ContactRef{}, false, nil
	}
	c, err := l.contacts.Get(ctx, id)
	if errors.Is(err, crm.ErrNotFound) {
		return ledger.ContactRef{}, false, nil
	}
	if err != nil {
		return ledger.ContactRef{}, false, err
	}
	if !c.Book.Same(crmBook(book)) || c.DeletedAt != nil {
		return ledger.ContactRef{}, false, nil
	}
	return ledger.ContactRef{ID: c.ID, Name: c.Name}, true, nil
}

// Listing implements ledger.Links: a listing of the organisation, or of the
// direct owner (any status).
func (l ledgerLinks) Listing(ctx context.Context, book ledger.Book, id uuid.UUID) (ledger.ListingRef, bool, error) {
	if l.listings == nil {
		return ledger.ListingRef{}, false, nil
	}
	records, err := l.listings.ByIDs(ctx, []uuid.UUID{id})
	if err != nil || len(records) == 0 {
		return ledger.ListingRef{}, false, err
	}
	r := records[0]
	mine := false
	if book.OrganisationID != nil {
		mine = r.OrganisationID != nil && *r.OrganisationID == *book.OrganisationID
	} else {
		mine = r.OrganisationID == nil && r.SellerUserID != nil && *r.SellerUserID == book.ID()
	}
	if !mine {
		return ledger.ListingRef{}, false, nil
	}
	return ledger.ListingRef{ID: r.ID, Title: r.Title, Reference: r.Reference}, true, nil
}

// Lease implements ledger.Links: a lease of the organisation, or one the
// direct owner made without an organisation.
func (l ledgerLinks) Lease(ctx context.Context, book ledger.Book, id uuid.UUID) (ledger.LeaseRef, bool, error) {
	if l.leases == nil {
		return ledger.LeaseRef{}, false, nil
	}
	ls, err := l.leases.Lease(ctx, id)
	if errors.Is(err, leases.ErrLeaseNotFound) {
		return ledger.LeaseRef{}, false, nil
	}
	if err != nil {
		return ledger.LeaseRef{}, false, err
	}
	mine := false
	if book.OrganisationID != nil {
		mine = ls.OrganisationID != nil && *ls.OrganisationID == *book.OrganisationID
	} else {
		mine = ls.OrganisationID == nil && ls.CreatedBy == book.ID()
	}
	if !mine {
		return ledger.LeaseRef{}, false, nil
	}
	return ledger.LeaseRef{ID: ls.ID, Reference: ls.Reference}, true, nil
}
