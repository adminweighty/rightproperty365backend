package devseed

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/tenancies"
)

// Sample properties, units and tenancies (fixed IDs, b7xx).
var (
	// PropertyAvondaleID has three units (Flat 1, Flat 2, Flat 3).
	PropertyAvondaleID = id("b701")
	UnitFlat1ID        = id("b711")
	UnitFlat2ID        = id("b712")
	UnitFlat3ID        = id("b713")
	// PropertyCottageID has no units (it is let as a whole).
	PropertyCottageID = id("b702")
	// PropertySecondStreetID is Second Street Realty's only property (the isolation tests).
	PropertySecondStreetID = id("b703")
	// TenancyFlat1ID is ACTIVE: Kudzai Sithole, whose contact emails include
	// the sample tenant account's (tenant@example.test), with landlord Tapiwa
	// Chirwa and the sample draft lease.
	TenancyFlat1ID = id("b721")
	// TenancyCottageID is ACTIVE: Tariro Moyo, periodic (no end date).
	TenancyCottageID = id("b722")
	// TenancyFlat2EndedID is ENDED: Tariro Moyo's earlier tenancy of Flat 2.
	TenancyFlat2EndedID = id("b723")
)

// TenancySamples returns the sample properties and tenancies, dates
// relative to now: Sample Estates has Avondale Court (three units) and
// Borrowdale Cottage (no units), two ACTIVE tenancies and one ENDED; Second
// Street Realty has one property and no tenancy.
func TenancySamples(now time.Time) tenancies.Change {
	now = now.UTC().Truncate(time.Second)
	estates, second := tenancies.OrganisationBook(SampleEstates.ID), tenancies.OrganisationBook(SecondStreetRealty.ID)
	rudo, chipo, blake := EstatesOwnerID, EstatesAgent2ID, identity.SeedAgentBUserID
	ptr := func(u uuid.UUID) *uuid.UUID { return &u }
	at := func(d int) time.Time { return now.AddDate(0, 0, d) }
	tp := func(t time.Time) *time.Time { return &t }
	day := func(d int) string { return now.AddDate(0, 0, d).Format(time.DateOnly) }
	listing := propertyrequests.SeedListingID(2)

	var ch tenancies.Change
	ch.Properties = []tenancies.Property{
		{ID: PropertyAvondaleID, Book: estates, Name: "Avondale Court", Address: "12 King George Road, Avondale, Harare", MarketCode: "ZW",
			ListingID: &listing, Notes: "Three flats; the caretaker lives on site.", CreatedBy: ptr(rudo), CreatedByName: "Rudo Moyo",
			CreatedAt: at(-420), UpdatedAt: at(-420)},
		{ID: PropertyCottageID, Book: estates, Name: "Borrowdale Cottage", Address: "4 Pendennis Road, Borrowdale, Harare", MarketCode: "ZW",
			CreatedBy: ptr(chipo), CreatedByName: "Chipo Dube", CreatedAt: at(-130), UpdatedAt: at(-130)},
		{ID: PropertySecondStreetID, Book: second, Name: "Second Street House", Address: "9 Second Street, Harare", MarketCode: "ZW",
			CreatedBy: ptr(blake), CreatedByName: "Blake Agent", CreatedAt: at(-20), UpdatedAt: at(-20)},
	}
	for i, u := range []struct {
		id    uuid.UUID
		label string
	}{{UnitFlat1ID, "Flat 1"}, {UnitFlat2ID, "Flat 2"}, {UnitFlat3ID, "Flat 3"}} {
		ch.Units = append(ch.Units, tenancies.Unit{ID: u.id, PropertyID: PropertyAvondaleID, Label: u.label, Position: i + 1,
			CreatedAt: at(-420), UpdatedAt: at(-420)})
	}
	tenant := func(id uuid.UUID, name string) tenancies.Party {
		return tenancies.Party{ContactID: id, Role: tenancies.RoleTenant, Name: name}
	}
	landlord := tenancies.Party{ContactID: ContactTapiwaID, Role: tenancies.RoleLandlord, Name: "Tapiwa Chirwa"}
	base := func(t tenancies.Tenancy, by uuid.UUID, byName string, created int) tenancies.Tenancy {
		t.Book, t.BookName, t.RentCurrency, t.RentFrequency = estates, SampleEstates.Name, "USD", tenancies.FrequencyMonthly
		t.CreatedBy, t.CreatedByName, t.CreatedAt, t.UpdatedAt = ptr(by), byName, at(created), at(created)
		t.ActivatedAt, t.ActivatedBy, t.ActivatedByName = tp(at(created+1)), ptr(by), byName
		return t
	}
	flat1Start := now.AddDate(0, -2, 0)
	ch.Tenancies = []tenancies.Tenancy{
		base(tenancies.Tenancy{ID: TenancyFlat1ID, PropertyID: PropertyAvondaleID, UnitID: ptr(UnitFlat1ID), LeaseID: ptr(SampleDraftLeaseID),
			Parties: []tenancies.Party{tenant(ContactKudzaiID, "Kudzai Sithole"), landlord}, RentAmount: "950.00", RentDueDay: 1,
			DepositAmount: "950.00", DepositCurrency: "USD", StartDate: flat1Start.Format(time.DateOnly),
			EndDate: flat1Start.AddDate(1, 0, -1).Format(time.DateOnly), Status: tenancies.StatusActive,
			Notes: "Deposit held in the trust account."}, chipo, "Chipo Dube", -70),
		base(tenancies.Tenancy{ID: TenancyCottageID, PropertyID: PropertyCottageID, Parties: []tenancies.Party{tenant(ContactTariroID, "Tariro Moyo")},
			RentAmount: "700.00", RentDueDay: 5, StartDate: day(-120), Status: tenancies.StatusActive}, chipo, "Chipo Dube", -125),
	}
	ended := base(tenancies.Tenancy{ID: TenancyFlat2EndedID, PropertyID: PropertyAvondaleID, UnitID: ptr(UnitFlat2ID),
		Parties: []tenancies.Party{tenant(ContactTariroID, "Tariro Moyo"), landlord}, RentAmount: "650.00", RentDueDay: 1,
		DepositAmount: "650.00", DepositCurrency: "USD", StartDate: day(-400), EndDate: day(-35), EndedOn: day(-35),
		Status: tenancies.StatusEnded, EndReason: "The fixed term came to its end; the tenant moved to Borrowdale Cottage."}, rudo, "Rudo Moyo", -410)
	ended.EndedAt, ended.EndedBy, ended.EndedByName, ended.UpdatedAt = tp(at(-35)), ptr(rudo), "Rudo Moyo", at(-35)
	ch.Tenancies = append(ch.Tenancies, ended)
	return ch
}

// SeedMemoryTenancies stores the sample properties and tenancies in memory (no audit).
func SeedMemoryTenancies(store *tenancies.MemoryStore, now time.Time) { store.Put(TenancySamples(now)) }

// seedPostgresTenancies writes the samples (needs migration 0035; after the
// contacts and the leases, which they reference). Every row is upserted by
// its fixed ID, so a re-seed restores them; the contacts' re-seed deletes and
// re-inserts the same IDs in the same transaction, which the deferred
// foreign key of tenancy_parties allows.
func seedPostgresTenancies(ctx context.Context, tx pgx.Tx, now time.Time) error {
	return tenancies.ApplyTx(ctx, tx, TenancySamples(now))
}
