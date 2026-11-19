package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/tenancies"
)

// TenanciesService builds properties, units and tenancies: parties are the
// book's own CRM contacts, a property may link one of the book's listings
// and a tenancy one of its leases, currencies and countries come from
// reference data, and a tenant is matched by their verified account email.
func TenanciesService(store tenancies.Store, contacts crm.Store, listingStore listings.Store, leaseStore leases.Store,
	marketStore markets.Store, identityStore identity.Store, logger *slog.Logger, now func() time.Time) *tenancies.Service {
	list := func(ctx context.Context, pick func(markets.Market) string) ([]string, error) {
		all, err := marketStore.List(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(all))
		for _, m := range all {
			out = append(out, pick(m))
		}
		return out, nil
	}
	return tenancies.NewService(tenancies.Deps{
		Store: store, Logger: logger, Now: now,
		Links: tenancyLinks{contacts: contacts, ledger: ledgerLinks{contacts: contacts, listings: listingStore, leases: leaseStore}},
		Currencies: func(ctx context.Context) ([]string, error) {
			return list(ctx, func(m markets.Market) string { return m.CurrencyCode })
		},
		Markets: func(ctx context.Context) ([]string, error) {
			return list(ctx, func(m markets.Market) string { return m.Code })
		},
		Account: func(ctx context.Context, userID uuid.UUID) (string, bool, error) {
			u, err := identityStore.UserByID(ctx, userID)
			if errors.Is(err, identity.ErrNotFound) {
				return "", false, nil
			}
			if err != nil {
				return "", false, err
			}
			return u.Email, u.Verified() && u.Status == identity.StatusActive, nil
		},
	})
}

// tenancyLinks resolves a property's or tenancy's links, inside its own book.
type tenancyLinks struct {
	contacts crm.Store
	ledger   ledgerLinks
}

func tenancyLedgerBook(b tenancies.Book) ledger.Book {
	if b.OrganisationID != nil {
		return ledger.OrganisationBook(*b.OrganisationID)
	}
	return ledger.PersonalBook(b.ID())
}

func tenancyBook(b crm.Book) tenancies.Book {
	if b.OrganisationID != nil {
		return tenancies.OrganisationBook(*b.OrganisationID)
	}
	return tenancies.PersonalBook(b.ID())
}

// Contact implements tenancies.Links: a live contact of the same book.
func (l tenancyLinks) Contact(ctx context.Context, book tenancies.Book, id uuid.UUID) (tenancies.ContactInfo, bool, error) {
	if l.contacts == nil {
		return tenancies.ContactInfo{}, false, nil
	}
	c, err := l.contacts.Get(ctx, id)
	if errors.Is(err, crm.ErrNotFound) {
		return tenancies.ContactInfo{}, false, nil
	}
	if err != nil {
		return tenancies.ContactInfo{}, false, err
	}
	if !c.Live() || !tenancyBook(c.Book).Same(book) {
		return tenancies.ContactInfo{}, false, nil
	}
	return tenancies.ContactInfo{ID: c.ID, Book: book, Name: c.Name}, true, nil
}

// Listing implements tenancies.Links (the ledger's rule: the book's own listing, any status).
func (l tenancyLinks) Listing(ctx context.Context, book tenancies.Book, id uuid.UUID) (tenancies.ListingRef, bool, error) {
	r, ok, err := l.ledger.Listing(ctx, tenancyLedgerBook(book), id)
	return tenancies.ListingRef{ID: r.ID, Title: r.Title, Reference: r.Reference}, ok, err
}

// Lease implements tenancies.Links (the ledger's rule: the organisation's
// lease, or one the direct owner made without an organisation).
func (l tenancyLinks) Lease(ctx context.Context, book tenancies.Book, id uuid.UUID) (tenancies.LeaseRef, bool, error) {
	r, ok, err := l.ledger.Lease(ctx, tenancyLedgerBook(book), id)
	return tenancies.LeaseRef{ID: r.ID, Reference: r.Reference}, ok, err
}

// ContactsWithEmail implements tenancies.Links.
func (l tenancyLinks) ContactsWithEmail(ctx context.Context, email string) ([]tenancies.ContactInfo, error) {
	if l.contacts == nil {
		return nil, nil
	}
	list, err := l.contacts.WithEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	out := make([]tenancies.ContactInfo, 0, len(list))
	for _, c := range list {
		out = append(out, tenancies.ContactInfo{ID: c.ID, Book: tenancyBook(c.Book), Name: c.Name})
	}
	return out, nil
}
