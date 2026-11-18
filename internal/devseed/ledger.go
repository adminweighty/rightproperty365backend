package devseed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

// Sample ledger entries (fixed IDs, a9xx; notes a8xx).
var (
	// LedgerOpeningUSDCashID, LedgerOpeningUSDBankID and LedgerOpeningZARBankID
	// are Sample Estates' opening balances (ZAR at the Johannesburg branch).
	LedgerOpeningUSDCashID = id("a901")
	LedgerOpeningUSDBankID = id("a902")
	LedgerOpeningZARBankID = id("a903")
	// LedgerReconciledID is the reconciled rent payment (linked to Tariro Moyo and listing 1).
	LedgerReconciledID = id("a904")
	// LedgerCorrectedID was entered as 70.00 instead of 700.00:
	// LedgerCorrectionReversalID reverses it and LedgerCorrectionID replaces it.
	LedgerCorrectedID          = id("a905")
	LedgerCorrectionReversalID = id("a906")
	LedgerCorrectionID         = id("a907")
	// LedgerTransferOutID / LedgerTransferInID are the cash-to-bank transfer pair.
	LedgerTransferOutID = id("a913")
	LedgerTransferInID  = id("a914")
	// LedgerReceivableOverdueID is 31 to 60 days overdue; LedgerReceivablePartID
	// is partly settled by LedgerSettlementID.
	LedgerReceivableOverdueID = id("a916")
	LedgerReceivablePartID    = id("a917")
	LedgerSettlementID        = id("a918")
	// LedgerSecondStreetID is Second Street Realty's only entry (the isolation tests).
	LedgerSecondStreetID = id("a920")
)

// LedgerSamples returns the sample books: Sample Estates has opening
// balances in USD (cash and bank) and ZAR (bank, Johannesburg branch), a
// month of rent received, commissions, expenses and a deposit held, one
// corrected entry (a reversal plus a new entry), one reconciled entry, one
// transfer from cash to bank and two receivables (one partly settled).
// Second Street Realty has one opening balance. Dates are relative to now.
func LedgerSamples(now time.Time) ledger.Change {
	now = now.UTC().Truncate(time.Second)
	estates, second := ledger.OrganisationBook(SampleEstates.ID), ledger.OrganisationBook(SecondStreetRealty.ID)
	rudo, tendai, chipo, blake := EstatesOwnerID, identity.SeedAgentUserID, EstatesAgent2ID, identity.SeedAgentBUserID
	names := map[uuid.UUID]string{rudo: "Rudo Moyo", tendai: "Tendai Ncube", chipo: "Chipo Dube", blake: "Blake Agent"}
	ptr := func(u uuid.UUID) *uuid.UUID { return &u }
	day := func(d int) string { return now.AddDate(0, 0, d).Format(time.DateOnly) }
	at := func(d int) time.Time { return now.AddDate(0, 0, d).Add(-6 * time.Hour) }
	tariro, farai, tapiwa := ContactTariroID, id("c502"), id("c504")
	listing1 := propertyrequests.SeedListingID(1)

	var ch ledger.Change
	add := func(e ledger.Entry, by uuid.UUID, d int) ledger.Entry {
		e.AuthorUserID, e.AuthorName = ptr(by), names[by]
		if e.Book.ID() == uuid.Nil {
			e.Book = estates
		}
		if e.EffectiveDate == "" {
			e.EffectiveDate = day(d)
		}
		e.RecordedAt = at(d)
		if e.Status == "" {
			e.Status = ledger.StatusUnverified
		}
		ch.Entries = append(ch.Entries, e)
		return e
	}

	add(ledger.Entry{ID: LedgerOpeningUSDCashID, Type: ledger.TypeOpeningBalance, Direction: ledger.In, Method: ledger.MethodCash,
		Amount: "500.00", Currency: "USD", Description: "Cash on hand at the start of the books"}, rudo, -40)
	add(ledger.Entry{ID: LedgerOpeningUSDBankID, Type: ledger.TypeOpeningBalance, Direction: ledger.In, Method: ledger.MethodBank,
		Amount: "12000.00", Currency: "USD", Description: "USD bank account balance (statement of the month end)", Reference: "STMT-OPEN"}, rudo, -40)
	add(ledger.Entry{ID: LedgerOpeningZARBankID, Type: ledger.TypeOpeningBalance, Direction: ledger.In, Method: ledger.MethodBank,
		Amount: "25000.00", Currency: "ZAR", Branch: "Johannesburg", Description: "ZAR bank account balance"}, rudo, -40)

	reconciledAt := at(-25)
	add(ledger.Entry{ID: LedgerReconciledID, Type: ledger.TypeRentReceived, Direction: ledger.In, Category: "rent", Method: ledger.MethodBank,
		Amount: "700.00", Currency: "USD", Description: "Rent, two-bedroom flat", Reference: "RCPT-1001", ContactID: ptr(tariro),
		ListingID: ptr(listing1), Status: ledger.StatusReconciled, ReconciledAt: &reconciledAt, ReconciledBy: ptr(rudo),
		ReconciledName: names[rudo], ReconcileNote: "Matched to the bank statement"}, chipo, -30)
	add(ledger.Entry{ID: id("a912"), Type: ledger.TypeDepositHeld, Direction: ledger.In, Category: "deposit", Method: ledger.MethodBank,
		Amount: "700.00", Currency: "USD", Description: "Deposit, two-bedroom flat", Reference: "DEP-1001", ContactID: ptr(tariro),
		ListingID: ptr(listing1)}, chipo, -30)

	// Entered as 70.00 instead of 700.00, then corrected.
	add(ledger.Entry{ID: LedgerCorrectedID, Type: ledger.TypeRentReceived, Direction: ledger.In, Category: "rent", Method: ledger.MethodCash,
		Amount: "70.00", Currency: "USD", Description: "Rent, garden cottage", Reference: "RCPT-1002"}, tendai, -28)
	reason := "Amount entered as 70.00 instead of 700.00"
	add(ledger.Entry{ID: LedgerCorrectionReversalID, Type: ledger.TypeReversal, Direction: ledger.Out, Category: "rent", Method: ledger.MethodCash,
		Amount: "70.00", Currency: "USD", ReversalOf: ptr(LedgerCorrectedID), Reason: reason, EffectiveDate: day(-28),
		Description: "Reversal of RENT_RECEIVED 70.00 USD " + day(-28), Reference: "RCPT-1002"}, rudo, -27)
	add(ledger.Entry{ID: LedgerCorrectionID, Type: ledger.TypeRentReceived, Direction: ledger.In, Category: "rent", Method: ledger.MethodCash,
		Amount: "700.00", Currency: "USD", CorrectionOf: ptr(LedgerCorrectedID), Reason: reason, EffectiveDate: day(-28),
		Description: "Rent, garden cottage", Reference: "RCPT-1002"}, rudo, -27)
	ch.PostLinks = append(ch.PostLinks, ledger.LinkSet{EntryID: LedgerCorrectedID, ReversedBy: ptr(LedgerCorrectionReversalID),
		CorrectedBy: ptr(LedgerCorrectionID)})

	add(ledger.Entry{ID: id("a908"), Type: ledger.TypeCommission, Direction: ledger.In, Category: "commission", Method: ledger.MethodBank,
		Amount: "1250.00", Currency: "USD", Description: "Sale commission, Avondale townhouse", Reference: "INV-2031", ContactID: ptr(farai)}, tendai, -20)
	add(ledger.Entry{ID: id("a909"), Type: ledger.TypeExpense, Direction: ledger.Out, Category: "maintenance", Method: ledger.MethodCash,
		Amount: "85.50", Currency: "USD", Description: "Plumber, flat 4"}, chipo, -18)
	add(ledger.Entry{ID: id("a910"), Type: ledger.TypeExpense, Direction: ledger.Out, Category: "utilities", Method: ledger.MethodBank,
		Amount: "120.00", Currency: "USD", Description: "Office electricity"}, rudo, -15)
	add(ledger.Entry{ID: id("a911"), Type: ledger.TypeExpense, Direction: ledger.Out, Category: "advertising", Method: ledger.MethodBank,
		Amount: "3500.00", Currency: "ZAR", Branch: "Johannesburg", Description: "Property portal advertising"}, rudo, -12)

	add(ledger.Entry{ID: LedgerTransferOutID, Type: ledger.TypeTransfer, Direction: ledger.Out, Method: ledger.MethodCash, Amount: "600.00",
		Currency: "USD", Description: "Cash banked", Reference: "DEP-SLIP-77"}, rudo, -10)
	add(ledger.Entry{ID: LedgerTransferInID, Type: ledger.TypeTransfer, Direction: ledger.In, Method: ledger.MethodBank, Amount: "600.00",
		Currency: "USD", Description: "Cash banked", Reference: "DEP-SLIP-77", TransferPair: ptr(LedgerTransferOutID)}, rudo, -10)
	ch.PostLinks = append(ch.PostLinks, ledger.LinkSet{EntryID: LedgerTransferOutID, TransferPair: ptr(LedgerTransferInID)})

	add(ledger.Entry{ID: id("a915"), Type: ledger.TypeCommission, Direction: ledger.In, Category: "commission", Method: ledger.MethodBank,
		Amount: "8000.00", Currency: "ZAR", Branch: "Johannesburg", Description: "Letting commission, Sandton flat"}, rudo, -8)

	add(ledger.Entry{ID: LedgerReceivableOverdueID, Type: ledger.TypeReceivable, Direction: ledger.In, Category: "fees", Amount: "450.00",
		Currency: "USD", Description: "Management fee due", Reference: "INV-2020", ContactID: ptr(tapiwa)}, rudo, -45)
	add(ledger.Entry{ID: LedgerReceivablePartID, Type: ledger.TypeReceivable, Direction: ledger.In, Category: "rent", Amount: "700.00",
		Currency: "USD", Description: "Rent due, two-bedroom flat", Reference: "RENT-DUE-11", ContactID: ptr(tariro), ListingID: ptr(listing1)}, chipo, -5)
	add(ledger.Entry{ID: LedgerSettlementID, Type: ledger.TypeRentReceived, Direction: ledger.In, Category: "rent", Method: ledger.MethodCash,
		Amount: "300.00", Currency: "USD", Description: "Part payment of rent due", Reference: "RCPT-1010", ContactID: ptr(tariro),
		ListingID: ptr(listing1), Settles: ptr(LedgerReceivablePartID)}, chipo, -3)

	add(ledger.Entry{ID: LedgerSecondStreetID, Book: second, Type: ledger.TypeOpeningBalance, Direction: ledger.In, Method: ledger.MethodCash,
		Amount: "100.00", Currency: "USD", Description: "Second Street Realty's own cash"}, blake, -20)

	ch.Notes = append(ch.Notes,
		ledger.Note{ID: id("a801"), EntryID: LedgerReconciledID, Text: "Tenant paid by EFT; proof of payment on file.", AuthorUserID: ptr(chipo),
			AuthorName: names[chipo], CreatedAt: at(-30)},
		ledger.Note{ID: id("a802"), EntryID: LedgerCorrectionID, Text: "Corrected after counting the cash book against the receipt.",
			AuthorUserID: ptr(rudo), AuthorName: names[rudo], CreatedAt: at(-27)})
	return ch
}

// SeedMemoryLedger stores the sample books in memory (no audit).
func SeedMemoryLedger(store *ledger.MemoryStore, now time.Time) error {
	for _, b := range []ledger.Book{ledger.OrganisationBook(SampleEstates.ID), ledger.OrganisationBook(SecondStreetRealty.ID)} {
		if err := store.EnsureCategories(context.Background(), b, ledger.DefaultCategories()); err != nil {
			return err
		}
	}
	return store.Put(LedgerSamples(now))
}

// seedPostgresLedger adds the sample books (needs migration 0033; after the
// contacts, which entries link to). Posted entries can never be deleted or
// changed, so the samples are written once: when the first one exists the
// entries are left as they are (their contact and listing links are
// restored if a re-seed of the contacts cleared them).
func seedPostgresLedger(ctx context.Context, tx pgx.Tx, now time.Time) error {
	for _, b := range []ledger.Book{ledger.OrganisationBook(SampleEstates.ID), ledger.OrganisationBook(SecondStreetRealty.ID)} {
		if err := ledger.EnsureCategoriesTx(ctx, tx, b, ledger.DefaultCategories()); err != nil {
			return err
		}
	}
	ch := LedgerSamples(now)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_entries WHERE id = $1)`, LedgerOpeningUSDCashID).Scan(&exists); err != nil {
		return fmt.Errorf("seed ledger: %w", err)
	}
	if !exists {
		return ledger.ApplyTx(ctx, tx, ch)
	}
	for _, e := range ch.Entries {
		if _, err := tx.Exec(ctx, `UPDATE ledger_entries SET contact_id = $2, listing_id = $3 WHERE id = $1
			AND (contact_id IS DISTINCT FROM $2 OR listing_id IS DISTINCT FROM $3)`, e.ID, e.ContactID, e.ListingID); err != nil {
			return fmt.Errorf("seed ledger links: %w", err)
		}
	}
	return nil
}
