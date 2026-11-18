package devseed

import (
	"context"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/modules/ledger"
)

func TestLedgerSamples(t *testing.T) {
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	store := ledger.NewMemoryStore()
	if err := SeedMemoryLedger(store, now); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, total, err := store.Entries(ctx, ledger.OrganisationBook(SampleEstates.ID), ledger.Filter{})
	if err != nil || total != 18 {
		t.Fatalf("Sample Estates: %d %v", total, err)
	}
	kinds := map[string]int{}
	currencies := map[string]bool{}
	for _, e := range list {
		kinds[e.Type]++
		currencies[e.Currency] = true
		if e.Status == ledger.StatusGatewayConfirmed {
			t.Fatal("a sample claims gateway confirmation")
		}
	}
	if kinds[ledger.TypeOpeningBalance] != 3 || kinds[ledger.TypeReversal] != 1 || kinds[ledger.TypeTransfer] != 2 || !currencies["USD"] || !currencies["ZAR"] {
		t.Fatalf("samples: %v %v", kinds, currencies)
	}
	orig, _ := store.Entry(ctx, LedgerCorrectedID)
	if orig.ReversedBy == nil || *orig.ReversedBy != LedgerCorrectionReversalID || orig.CorrectedBy == nil || *orig.CorrectedBy != LedgerCorrectionID {
		t.Fatalf("the corrected entry: %+v", orig)
	}
	out, _ := store.Entry(ctx, LedgerTransferOutID)
	if out.TransferPair == nil || *out.TransferPair != LedgerTransferInID {
		t.Fatalf("the transfer: %+v", out)
	}
	if rec, _ := store.Entry(ctx, LedgerReconciledID); rec.Status != ledger.StatusReconciled || rec.ReconciledAt == nil {
		t.Fatalf("the reconciled entry: %+v", rec)
	}
	if _, n, _ := store.Entries(ctx, ledger.OrganisationBook(SecondStreetRealty.ID), ledger.Filter{}); n != 1 {
		t.Fatalf("Second Street: %d", n)
	}
	// Every linked contact is one of Sample Estates' own.
	contacts := map[string]bool{}
	for _, c := range CRMSamples(now).Contacts {
		if c.Book.ID() == SampleEstates.ID {
			contacts[c.ID.String()] = true
		}
	}
	for _, e := range list {
		if e.ContactID != nil && !contacts[e.ContactID.String()] {
			t.Fatalf("%s links contact %s of another book", e.ID, e.ContactID)
		}
	}
}
