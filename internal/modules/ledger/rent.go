package ledger

import (
	"context"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Rent receipts (API 0.36.1, FR-047). The tenancies module records a rent
// receipt and its RENT_RECEIVED entry in one transaction: it asks the ledger
// to validate and build the entry (PrepareRentReceipt), then applies the
// returned Change inside its own transaction (ApplyTx, or MemoryStore.
// ApplyWith). A ledger reversal of that entry undoes the receipt through the
// stores' reversal hook (SetReversalHook), in the reversal's transaction.

// RentReceipt is the entry of one rent receipt.
type RentReceipt struct {
	Amount        string
	Currency      string
	Method        string
	EffectiveDate string
	Reference     string
	Description   string
	TenancyID     uuid.UUID
	ContactID     *uuid.UUID
	ListingID     *uuid.UUID
	LeaseID       *uuid.UUID
}

// PrepareRentReceipt validates a rent receipt's entry with the rules of
// POST .../entries (write access to the book, amount, currency, method,
// date, the book's rent category, the links of the same book) and returns
// the Change that posts it (the entry and its audit record) without
// applying it. A *ValidationError names the ledger's own fields.
func (s *Service) PrepareRentReceipt(ctx context.Context, p *auth.Principal, bookID uuid.UUID, in RentReceipt) (Change, Entry, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Change{}, Entry{}, err
	}
	if !a.write {
		return Change{}, Entry{}, ErrForbidden
	}
	method, amount, desc, ref := in.Method, in.Amount, in.Description, in.Reference
	d, err := s.build(ctx, a, EntryInput{Type: TypeRentReceived, Method: &method, Amount: &amount, Currency: in.Currency,
		EffectiveDate: in.EffectiveDate, Description: &desc, Reference: &ref, ContactID: in.ContactID, ListingID: in.ListingID,
		LeaseID: in.LeaseID}, map[string]string{}, nil)
	if err != nil {
		return Change{}, Entry{}, err
	}
	e := d.entries[0]
	tenancy := in.TenancyID
	e.TenancyID = &tenancy
	ch := Change{Entries: []Entry{e},
		Audit: s.event(ctx, a, orgaudit.ActionLedgerEntryCreated, orgaudit.TargetLedgerEntry, e.ID, Label(e), nil, entryAudit(e))}
	return ch, e, nil
}

// SetRentArrears sets where the receivables report reads the rent arrears
// (the tenancies service, built after this one).
func (s *Service) SetRentArrears(fn func(ctx context.Context, book Book, asOf string) ([]RentArrear, error)) {
	s.arrears = fn
}

// RentArrear is one overdue rent due of a book as of a date (the tenancies
// module). Amounts have exactly the currency's minor-unit decimals.
type RentArrear struct {
	// Tenancy is the tenancy as the API shows it (contract 0.36 TenancyBrief).
	Tenancy any
	// Tenants are the tenancy's tenant contacts (the contact_id filter).
	Tenants     []uuid.UUID
	DueID       uuid.UUID
	DueDate     string
	Currency    string
	Amount      string
	Settled     string
	Outstanding string
}

// RentAgedItem is one rent arrear of the receivables report.
type RentAgedItem struct {
	Tenancy     any       `json:"tenancy"`
	DueID       uuid.UUID `json:"due_id"`
	DueDate     string    `json:"due_date"`
	Currency    string    `json:"currency"`
	MinorUnit   int       `json:"minor_unit"`
	Amount      string    `json:"amount"`
	Settled     string    `json:"settled"`
	Outstanding string    `json:"outstanding"`
	DaysOverdue int       `json:"days_overdue"`
	Bucket      string    `json:"bucket"`
}
