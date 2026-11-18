package devseed

import (
	"slices"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/modules/crm"
)

func TestCRMSamples(t *testing.T) {
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	ch := CRMSamples(now)
	perBook := map[string]int{}
	stages := map[string]bool{}
	byID := map[string]crm.Contact{}
	for _, c := range ch.Contacts {
		perBook[c.Book.ID().String()]++
		byID[c.ID.String()] = c
		if c.Book.ID() == SampleEstates.ID {
			stages[c.Stage] = true
		}
		for _, p := range c.Phones {
			if n, err := crm.NormalisePhone(p, "ZW"); err != nil || n != p {
				t.Fatalf("%s: phone %q is not E.164", c.Name, p)
			}
		}
	}
	if perBook[SampleEstates.ID.String()] != 8 || perBook[SecondStreetRealty.ID.String()] != 2 {
		t.Fatalf("books: %v", perBook)
	}
	if len(stages) != len(crm.Stages()) {
		t.Fatalf("stages used: %v", stages)
	}
	a, b := byID[ContactMarufuID.String()], byID[ContactMarufuDupID.String()]
	if !slices.Equal(a.Emails, b.Emails) {
		t.Fatal("the duplicate pair does not share an email")
	}
	// The auto-linked contact shares the email and phone of sample appointment 6.
	appt := Appointments(now)[5].Appointment
	tariro := byID[ContactTariroID.String()]
	if appt.ID != SampleAppointmentID(6) || appt.OrganisationID == nil || *appt.OrganisationID != SampleEstates.ID ||
		!slices.Contains(tariro.Emails, appt.CustomerEmail) || !slices.Contains(tariro.Phones, appt.CustomerPhone) ||
		!slices.ContainsFunc(ch.Links, func(l crm.Link) bool { return l.ContactID == ContactTariroID && l.ItemID == appt.ID }) {
		t.Fatalf("auto-link sample: %+v / %+v", appt, tariro)
	}
	due := 0
	for _, r := range ch.Reminders {
		if r.DueAt.Before(now) {
			due++
		}
	}
	if due != 1 {
		t.Fatalf("%d due reminders", due)
	}
	store := crm.NewMemoryStore()
	SeedMemoryCRM(store, now)
	rows, total, err := store.List(t.Context(), crm.OrganisationBook(SampleEstates.ID), crm.ListFilter{})
	if err != nil || total != 8 || len(rows) != 8 {
		t.Fatalf("memory seed: %d %v", total, err)
	}
}
