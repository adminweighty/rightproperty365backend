package devseed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/reference"
)

// addCRMReference appends the contact types and sources (migration 0030).
func addCRMReference(sets map[string][]reference.Item) {
	rows := func(pairs ...string) []reference.Item {
		var out []reference.Item
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, reference.Item{Code: pairs[i], Label: pairs[i+1], SortOrder: (i/2 + 1) * 10, Active: true})
		}
		return out
	}
	sets[reference.SetContactTypes] = rows("buyer", "Buyer", "tenant", "Tenant", "landlord", "Landlord", "owner", "Owner", "other", "Other")
	sets[reference.SetContactSources] = rows("enquiry", "Enquiry", "walk-in", "Walk-in", "referral", "Referral", "phone", "Phone call",
		"import", "Import", "website", "Website", "other", "Other")
}

// Sample contacts (fixed IDs, c5xx).
var (
	// ContactTariroID is auto-linked to sample appointment 6 (the guest
	// viewing of listing 10): same email, and the same phone written the
	// national way.
	ContactTariroID = id("c501")
	// ContactMarufuID and ContactMarufuDupID are the duplicate pair to merge
	// (the same email address, written differently).
	ContactMarufuID    = id("c507")
	ContactMarufuDupID = id("c508")
	// ContactNyashaID has the overdue reminder.
	ContactNyashaID = id("c503")
	// SecondStreetContactIDs are Second Street Realty's two contacts (the
	// isolation test); the first shares an email address with ContactTariroID.
	SecondStreetContactIDs = []uuid.UUID{id("c511"), id("c512")}
)

// CRMSamples returns the sample contact books: Sample Estates has 8 contacts
// across the stages (tags, notes, an overdue reminder, one auto-linked to a
// sample appointment, one duplicate pair); Second Street Realty has 2.
func CRMSamples(now time.Time) crm.Change {
	now = now.UTC().Truncate(time.Second)
	estates, second := crm.OrganisationBook(SampleEstates.ID), crm.OrganisationBook(SecondStreetRealty.ID)
	rudo, tendai, chipo, blake := EstatesOwnerID, identity.SeedAgentUserID, EstatesAgent2ID, identity.SeedAgentBUserID
	ptr := func(u uuid.UUID) *uuid.UUID { return &u }
	at := func(days int) time.Time { return now.AddDate(0, 0, days) }
	tp := func(t time.Time) *time.Time { return &t }
	var ch crm.Change
	n := 0
	add := func(c crm.Contact, by uuid.UUID, byName string, created time.Time) {
		c.CreatedBy, c.CreatedAt, c.UpdatedAt = ptr(by), created, created
		ch.Contacts = append(ch.Contacts, c)
		n++
		ch.Events = append(ch.Events, crm.Event{ID: id(fmt.Sprintf("c8%02d", n)), ContactID: c.ID, Kind: crm.EventCreated, ActorUserID: ptr(by),
			ActorName: byName, Data: map[string]any{"stage": crm.StageNew}, At: created})
		if c.Stage != crm.StageNew {
			ch.Events = append(ch.Events, crm.Event{ID: id(fmt.Sprintf("c9%02d", n)), ContactID: c.ID, Kind: crm.EventStageChanged, ActorUserID: ptr(by),
				ActorName: byName, Data: map[string]any{"from": crm.StageNew, "to": c.Stage}, At: created.Add(time.Hour)})
		}
	}
	note := func(nid string, contact uuid.UUID, text string, by uuid.UUID, byName string, when time.Time) {
		ch.Notes = append(ch.Notes, crm.Note{ID: id(nid), ContactID: contact, Text: text, AuthorUserID: ptr(by), AuthorName: byName, CreatedAt: when})
	}

	add(crm.Contact{ID: ContactTariroID, Book: estates, Name: "Tariro Moyo", Type: "tenant", Stage: crm.StageViewingRequested,
		OwnerUserID: ptr(chipo), Source: crm.SourceEnquiry, Emails: []string{"tariro@example.com"}, Phones: []string{"+263771234567"},
		Tags: []string{"rental", "harare"}, LastContactAt: tp(at(-1))}, chipo, "Chipo Dube", at(-6))
	ch.Links = append(ch.Links, crm.Link{ContactID: ContactTariroID, Kind: crm.KindAppointment, ItemID: SampleAppointmentID(6), Auto: true, LinkedAt: at(-6)})
	ch.Events = append(ch.Events, crm.Event{ID: id("c8a1"), ContactID: ContactTariroID, Kind: crm.EventItemLinked,
		Data: map[string]any{"kind": crm.KindAppointment, "id": SampleAppointmentID(6).String(), "reference": "AP-DEM22227"}, At: at(-6)})
	note("c601", ContactTariroID, "Looking for a two-bedroom flat near the city centre; budget USD 600 a month.", chipo, "Chipo Dube", at(-5))

	add(crm.Contact{ID: id("c502"), Book: estates, Name: "Farai Mukoni", Type: "buyer", Stage: crm.StageClosed, OwnerUserID: ptr(tendai),
		Source: crm.SourceEnquiry, Emails: []string{"farai.mukoni@example.com"}, Tags: []string{"first-time buyer"}, LastContactAt: tp(at(-20))},
		tendai, "Tendai Ncube", at(-40))
	note("c602", id("c502"), "Bought the townhouse in Avondale. Ask for a referral in three months.", tendai, "Tendai Ncube", at(-20))

	add(crm.Contact{ID: ContactNyashaID, Book: estates, Name: "Nyasha Banda", Type: "buyer", Stage: crm.StageContacted, OwnerUserID: ptr(tendai),
		Source: "referral", Phones: []string{"+263772000111"}, Tags: []string{"investor"}, LastContactAt: tp(at(-3))}, tendai, "Tendai Ncube", at(-10))
	note("c603", ContactNyashaID, "Interested in stands in Borrowdale; prefers WhatsApp.", tendai, "Tendai Ncube", at(-3))
	ch.Reminders = append(ch.Reminders, crm.Reminder{ID: id("c701"), ContactID: ContactNyashaID, DueAt: now.Add(-2 * time.Hour),
		Text: "Call back about the Borrowdale stands", CreatedBy: ptr(tendai), CreatedByName: "Tendai Ncube", CreatedAt: at(-3)})

	add(crm.Contact{ID: id("c504"), Book: estates, Name: "Tapiwa Chirwa", Type: "landlord", Stage: crm.StageNegotiating, OwnerUserID: ptr(rudo),
		Source: "walk-in", Emails: []string{"tapiwa.chirwa@example.com"}, Phones: []string{"+263712345678"}, Tags: []string{"landlord", "borrowdale"},
		Marketing:     &crm.Marketing{Granted: true, GrantedAt: at(-30), Source: "Signed the office's marketing form", RecordedBy: ptr(rudo)},
		LastContactAt: tp(at(-2))}, rudo, "Rudo Moyo", at(-30))
	note("c604", id("c504"), "Wants to let three flats; discussing the management fee.", rudo, "Rudo Moyo", at(-2))

	add(crm.Contact{ID: id("c505"), Book: estates, Name: "Rumbi Gumbo", Type: "owner", Stage: crm.StageViewingConfirmed, OwnerUserID: ptr(chipo),
		Source: "website", Emails: []string{"rumbi.gumbo@example.com"}, Tags: []string{"valuation"}}, chipo, "Chipo Dube", at(-8))
	ch.Reminders = append(ch.Reminders, crm.Reminder{ID: id("c702"), ContactID: id("c505"), DueAt: at(1),
		Text: "Confirm the valuation visit", CreatedBy: ptr(chipo), CreatedByName: "Chipo Dube", CreatedAt: at(-1)})

	add(crm.Contact{ID: id("c506"), Book: estates, Name: "Kudzai Sithole", Type: "tenant", Stage: crm.StageUnsuccessful, OwnerUserID: ptr(chipo),
		Source: "phone", Phones: []string{"+263733111222"}, LastContactAt: tp(at(-15))}, chipo, "Chipo Dube", at(-25))

	add(crm.Contact{ID: ContactMarufuID, Book: estates, Name: "Tendai Marufu", Type: "buyer", Stage: crm.StageNew, OwnerUserID: ptr(tendai),
		Source: "walk-in", Emails: []string{"t.marufu@example.com"}, Phones: []string{"+263773555444"}, Tags: []string{"cash buyer"}},
		tendai, "Tendai Ncube", at(-4))
	note("c607", ContactMarufuID, "Walked in asking about houses in Mount Pleasant.", tendai, "Tendai Ncube", at(-4))

	add(crm.Contact{ID: ContactMarufuDupID, Book: estates, Name: "T. Marufu", Type: "buyer", Stage: crm.StageContacted,
		Source: "phone", Emails: []string{"t.marufu@example.com"}, Phones: []string{"+263773555445"}, Tags: []string{"mount pleasant"},
		LastContactAt: tp(at(-1))}, rudo, "Rudo Moyo", at(-2))
	note("c608", ContactMarufuDupID, "Called from a second number; same person as the walk-in? Merge if so.", rudo, "Rudo Moyo", at(-1))

	add(crm.Contact{ID: SecondStreetContactIDs[0], Book: second, Name: "Tariro M.", Type: "tenant", Stage: crm.StageNew, OwnerUserID: ptr(blake),
		Source: crm.SourceEnquiry, Emails: []string{"tariro@example.com"}}, blake, "Blake Agent", at(-3))
	add(crm.Contact{ID: SecondStreetContactIDs[1], Book: second, Name: "Blessing Ncube", Type: "buyer", Stage: crm.StageContacted, OwnerUserID: ptr(blake),
		Source: "referral", Phones: []string{"+263774999888"}}, blake, "Blake Agent", at(-9))
	note("c611", SecondStreetContactIDs[1], "Second Street Realty's private note.", blake, "Blake Agent", at(-9))
	return ch
}

// SeedMemoryCRM stores the sample contact books in memory (no events, no audit).
func SeedMemoryCRM(store *crm.MemoryStore, now time.Time) { store.Put(CRMSamples(now)) }

// seedPostgresCRM replaces the sample contacts (needs migration 0030; after
// the appointments, which one contact links to).
func seedPostgresCRM(ctx context.Context, tx pgx.Tx, now time.Time) error {
	ch := CRMSamples(now)
	ids := make([]uuid.UUID, 0, len(ch.Contacts))
	for _, c := range ch.Contacts {
		ids = append(ids, c.ID)
	}
	if _, err := tx.Exec(ctx, `UPDATE contacts SET merged_into = NULL WHERE merged_into = ANY($1)`, ids); err != nil {
		return fmt.Errorf("seed contacts: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM contacts WHERE id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("seed contacts: %w", err)
	}
	return crm.ApplyTx(ctx, tx, ch)
}
