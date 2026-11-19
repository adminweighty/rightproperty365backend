package ledger

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Maintenance costs (API 0.36.2, FR-047). The maintenance module records a
// cost and its EXPENSE entry in one transaction, as the tenancies module
// does a rent receipt: PrepareMaintenanceCost validates and builds the
// entry, the caller applies the returned Change inside its own transaction
// (ApplyTx, or MemoryStore.ApplyWith).

// CategoryMaintenance is the category of a maintenance cost's entry.
const CategoryMaintenance = "maintenance"

// MaintenanceCost is the entry of one maintenance cost.
type MaintenanceCost struct {
	Amount        string
	Currency      string
	Method        string
	EffectiveDate string
	Reference     string
	Description   string
	PropertyID    uuid.UUID
	ListingID     *uuid.UUID
}

// PrepareMaintenanceCost validates a maintenance cost's entry with the rules
// of POST .../entries (write access to the book, amount, currency, method,
// date, the book's maintenance category, the links of the same book) and
// returns the Change that posts it without applying it. The book gains the
// default maintenance category first when it has none with that code (a
// deactivated one is refused). A *ValidationError names the ledger's own
// fields.
func (s *Service) PrepareMaintenanceCost(ctx context.Context, p *auth.Principal, bookID uuid.UUID, in MaintenanceCost) (Change, Entry, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Change{}, Entry{}, err
	}
	if !a.write {
		return Change{}, Entry{}, ErrForbidden
	}
	cats, err := s.categories(ctx, a.book)
	if err != nil {
		return Change{}, Entry{}, err
	}
	if !slices.ContainsFunc(cats, func(c Category) bool { return c.Code == CategoryMaintenance }) {
		for _, c := range DefaultCategories() {
			if c.Code != CategoryMaintenance {
				continue
			}
			if err := s.store.EnsureCategories(ctx, a.book, withClock([]Category{c}, s.clock())); err != nil {
				return Change{}, Entry{}, err
			}
		}
	}
	method, amount, desc, ref, cat := in.Method, in.Amount, in.Description, in.Reference, CategoryMaintenance
	d, err := s.build(ctx, a, EntryInput{Type: TypeExpense, Method: &method, Amount: &amount, Currency: in.Currency,
		EffectiveDate: in.EffectiveDate, Description: &desc, Reference: &ref, Category: &cat, ListingID: in.ListingID},
		map[string]string{}, nil)
	if err != nil {
		return Change{}, Entry{}, err
	}
	e := d.entries[0]
	property := in.PropertyID
	e.PropertyID = &property
	ch := Change{Entries: []Entry{e},
		Audit: s.event(ctx, a, orgaudit.ActionLedgerEntryCreated, orgaudit.TargetLedgerEntry, e.ID, Label(e), nil, entryAudit(e))}
	return ch, e, nil
}
