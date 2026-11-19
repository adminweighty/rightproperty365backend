package devseed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/tenancies"
)

// Sample rent receipts (fixed IDs: receipts b73x, their ledger entries b74x).
var (
	// RentFlat1FirstID and RentFlat1SecondID pay Flat 1's first two dues.
	RentFlat1FirstID  = id("b731")
	RentFlat1SecondID = id("b732")
	// RentCottageFirstID and RentCottageSecondID pay the cottage's first two
	// dues; RentCottagePartID pays 300.00 of its fourth (an explicit
	// allocation), so its third is unpaid and 45 days overdue.
	RentCottageFirstID  = id("b733")
	RentCottageSecondID = id("b734")
	RentCottagePartID   = id("b735")
)

// rentEntryID is the ledger entry of a sample receipt (b731 -> b741 ...).
func rentEntryID(receipt uuid.UUID) uuid.UUID {
	b := receipt
	b[15] += 0x10
	return b
}

// SeedDueID is the fixed ID of a sample due: derived from its tenancy and due date.
func SeedDueID(tenancyID uuid.UUID, dueDate string) uuid.UUID {
	return uuid.NewSHA1(tenancyID, []byte("rent-due:"+dueDate))
}

type sampleReceipt struct {
	id        uuid.UUID
	tenancy   tenancies.Tenancy
	due       int
	amount    string
	method    string
	reference string
	// late is how many days after the due date it was received.
	late int
}

// RentSamples returns the sample schedules and receipts of Sample Estates'
// two ACTIVE tenancies (the ENDED one predates part b and has none): the
// receipts' RENT_RECEIVED ledger entries, and the dues, receipts and
// allocations. Dates are relative to now.
func RentSamples(now time.Time) (ledger.Change, tenancies.Change) {
	now = now.UTC().Truncate(time.Second)
	today := now.Format(time.DateOnly)
	samples := TenancySamples(now)
	chipo := EstatesAgent2ID
	byID := map[uuid.UUID]tenancies.Tenancy{}
	var lch ledger.Change
	var tch tenancies.Change
	dues := map[uuid.UUID][]tenancies.Due{}
	for _, t := range samples.Tenancies {
		byID[t.ID] = t
		if t.Status != tenancies.StatusActive {
			continue
		}
		through, _ := tenancies.ScheduleThrough(t, today)
		for i, d := range tenancies.DueDates(t.RentFrequency, t.RentDueDay, t.StartDate, through) {
			due := tenancies.Due{ID: SeedDueID(t.ID, d), TenancyID: t.ID, Sequence: i + 1, DueDate: d, Amount: t.RentAmount, Currency: t.RentCurrency,
				CreatedAt: *t.ActivatedAt}
			dues[t.ID] = append(dues[t.ID], due)
			tch.Dues = append(tch.Dues, due)
		}
	}
	flat1, cottage := byID[TenancyFlat1ID], byID[TenancyCottageID]
	plan := []sampleReceipt{
		{RentFlat1FirstID, flat1, 0, "950.00", ledger.MethodBank, "EFT KS-0001", 0},
		{RentFlat1SecondID, flat1, 1, "950.00", ledger.MethodBank, "EFT KS-0002", 0},
		{RentCottageFirstID, cottage, 0, "700.00", ledger.MethodMobileMoney, "EcoCash TM-11", 1},
		{RentCottageSecondID, cottage, 1, "700.00", ledger.MethodCash, "Cash receipt 0042", 0},
		{RentCottagePartID, cottage, 3, "300.00", ledger.MethodBank, "EFT TM-PART", 2},
	}
	tenant := map[uuid.UUID]struct {
		id   uuid.UUID
		name string
	}{TenancyFlat1ID: {ContactKudzaiID, "Kudzai Sithole"}, TenancyCottageID: {ContactTariroID, "Tariro Moyo"}}
	labels := map[uuid.UUID]string{TenancyFlat1ID: "Avondale Court Flat 1", TenancyCottageID: "Borrowdale Cottage"}
	listing := propertyrequests.SeedListingID(2)
	for _, r := range plan {
		list := dues[r.tenancy.ID]
		if r.due >= len(list) {
			continue
		}
		due := list[r.due]
		at, _ := time.Parse(time.DateOnly, due.DueDate)
		received := min(at.AddDate(0, 0, r.late).Format(time.DateOnly), today)
		at, _ = time.Parse(time.DateOnly, received)
		recordedAt := at.Add(10 * time.Hour)
		who := tenant[r.tenancy.ID]
		contact, tenancyID := who.id, r.tenancy.ID
		entry := ledger.Entry{ID: rentEntryID(r.id), Book: ledger.OrganisationBook(SampleEstates.ID), Type: ledger.TypeRentReceived,
			Direction: ledger.In, Category: "rent", Method: r.method, Amount: r.amount, Currency: r.tenancy.RentCurrency, EffectiveDate: received,
			RecordedAt: recordedAt, AuthorUserID: &chipo, AuthorName: "Chipo Dube", Status: ledger.StatusUnverified, ContactID: &contact,
			TenancyID: &tenancyID, Description: "Rent: " + labels[r.tenancy.ID], Reference: r.reference}
		if r.tenancy.ID == TenancyFlat1ID {
			lease := SampleDraftLeaseID
			entry.LeaseID, entry.ListingID = &lease, &listing
		}
		lch.Entries = append(lch.Entries, entry)
		tch.Receipts = append(tch.Receipts, tenancies.Receipt{ID: r.id, TenancyID: r.tenancy.ID, Amount: r.amount, Currency: r.tenancy.RentCurrency,
			Method: r.method, ReceivedOn: received, Reference: r.reference, ContactID: &contact, ContactName: who.name, LedgerEntryID: entry.ID,
			RecordedBy: &chipo, RecordedByName: "Chipo Dube", RecordedAt: recordedAt,
			Allocations: []tenancies.Allocation{{DueID: due.ID, Amount: r.amount}}})
	}
	return lch, tch
}

// SeedMemoryRent stores the sample schedules and receipts in memory (no
// audit), with the receipts' ledger entries. After the ledger and the
// tenancies samples.
func SeedMemoryRent(store *tenancies.MemoryStore, ledgerStore *ledger.MemoryStore, now time.Time) error {
	lch, tch := RentSamples(now)
	if err := ledgerStore.Put(lch); err != nil {
		return err
	}
	store.Put(tch)
	return nil
}

// seedPostgresRent writes the samples (needs migration 0036; after the
// ledger and the tenancies). The seeded tenancies' dues, receipts and
// allocations are replaced (their dates are relative to now); the receipts'
// ledger entries are posted once and never changed (posted entries are
// immutable), so a re-seed keeps the first seed's receipt dates.
func seedPostgresRent(ctx context.Context, tx pgx.Tx, now time.Time) error {
	lch, tch := RentSamples(now)
	ids := []uuid.UUID{TenancyFlat1ID, TenancyCottageID}
	for _, q := range []string{
		`DELETE FROM rent_allocations WHERE tenancy_id = ANY($1)`,
		`DELETE FROM rent_receipts WHERE tenancy_id = ANY($1)`,
		`DELETE FROM rent_dues WHERE tenancy_id = ANY($1)`,
	} {
		if _, err := tx.Exec(ctx, q, ids); err != nil {
			return fmt.Errorf("seed rent: %w", err)
		}
	}
	var fresh ledger.Change
	for i, e := range lch.Entries {
		var date string
		err := tx.QueryRow(ctx, `SELECT effective_date::text FROM ledger_entries WHERE id = $1`, e.ID).Scan(&date)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			fresh.Entries = append(fresh.Entries, e)
		case err != nil:
			return fmt.Errorf("seed rent: %w", err)
		default:
			// Posted once; the contact link is restored (the CRM re-seed
			// re-inserts the contacts, which sets it NULL).
			tch.Receipts[i].ReceivedOn = date
			if _, err := tx.Exec(ctx, `UPDATE ledger_entries SET contact_id = $2 WHERE id = $1 AND contact_id IS DISTINCT FROM $2`,
				e.ID, e.ContactID); err != nil {
				return fmt.Errorf("seed rent links: %w", err)
			}
		}
	}
	if len(fresh.Entries) > 0 {
		if err := ledger.ApplyTx(ctx, tx, fresh); err != nil {
			return err
		}
	}
	return tenancies.ApplyTx(ctx, tx, tenancies.Change{Dues: tch.Dues, Receipts: tch.Receipts})
}
