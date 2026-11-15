package devseed

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

// AppointmentCount is the number of sample appointments.
const AppointmentCount = 10

// SampleAppointment is a sample appointment with its timeline.
type SampleAppointment struct {
	Appointment appointments.Appointment
	Timeline    []appointments.TimelineEntry
}

// SampleAppointmentID returns the ID of sample appointment n (1..AppointmentCount).
func SampleAppointmentID(n int) uuid.UUID { return id(fmt.Sprintf("f0%02d", n)) }

// Appointments returns the sample appointments, with times relative to now so
// that there is always something upcoming and something past:
//
//	1  VIEWING listing 1 (Tendai Ncube)        consumer   REQUESTED            in 2 days
//	2  VIEWING listing 2 (Tendai Ncube)        consumer   CONFIRMED            in 3 days
//	3  VIEWING listing 7 (Chipo Dube)          consumer   RESCHEDULE_REQUESTED in 4 days, the agent proposed day 5
//	4  VIEWING listing 5 (Tendai Ncube)        consumer   COMPLETED            7 days ago
//	5  CONSULTATION solicitors/sample-law-chambers  consumer  REQUESTED        in 6 days
//	6  VIEWING listing 10 (Chipo Dube)         a guest    REQUESTED            in 8 days
//	7  CONSULTATION agents/tendai-ncube        consumer   CONFIRMED            tomorrow
//	8  CONSULTATION property-managers/sample-property-management  tenant  REQUESTED  in 3 days
//	9  CONSULTATION developers/sample-developments                tenant  CONFIRMED  in 5 days
//	10 CONSULTATION service-providers/sample-plumbers             tenant  REQUESTED  in 2 days
//
// 8 to 10 give the property manager, developer and service provider sample
// accounts (and the tenant) something on their dashboards.
func Appointments(now time.Time) []SampleAppointment {
	now = now.UTC().Truncate(time.Microsecond)
	// 08:00 UTC is 10:00 in Harare.
	day := func(offset int, hour int) time.Time {
		d := now.AddDate(0, 0, offset)
		return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, time.UTC)
	}
	routing := map[uuid.UUID]propertyrequests.Listing{}
	for _, l := range Listings() {
		routing[l.ID] = l
	}
	records := map[uuid.UUID]int{}
	listingRecords := ListingRecords(now)
	for i, r := range listingRecords {
		records[r.ID] = i
	}
	profiles := map[string]Profile{}
	for _, p := range Profiles() {
		profiles[p.Key()] = p
	}
	consumer := identity.SeedConsumerUserID

	var out []SampleAppointment
	add := func(a appointments.Appointment, created time.Time, steps ...appointments.TimelineEntry) {
		n := len(out) + 1
		a.ID, a.Reference = SampleAppointmentID(n), fmt.Sprintf("AP-DEM2222%d", n+1)
		a.MarketID, a.Timezone, a.DurationMinutes = markets.ZimbabweID, "Africa/Harare", appointments.DefaultDurationMinutes
		a.PreferredContactMethod = propertyrequests.ContactEmail
		a.CreatedAt, a.UpdatedAt = created, created
		if a.CustomerEmail == "" {
			a.CustomerUserID, a.CustomerName, a.CustomerEmail = &consumer, "Casey Consumer", identity.SeedConsumerEmail
		}
		start := a.StartUTC
		entries := []appointments.TimelineEntry{{
			Event: appointments.TimelineRequested, ActorType: appointments.PartyCustomer, ActorUserID: a.CustomerUserID,
			ToStatus: appointments.StatusRequested, StartUTC: &start, At: created,
		}}
		entries = append(entries, steps...)
		for i := range entries {
			entries[i].ID, entries[i].AppointmentID = id(fmt.Sprintf("f1%x%d", n, i)), a.ID
			if i > 0 {
				entries[i].FromStatus = entries[i-1].ToStatus
				a.UpdatedAt = entries[i].At
			}
		}
		out = append(out, SampleAppointment{Appointment: a, Timeline: entries})
	}
	viewing := func(listing int, status string, start time.Time) appointments.Appointment {
		l := routing[propertyrequests.SeedListingID(listing)]
		a := appointments.Appointment{
			Type: appointments.TypeViewing, Status: status, ListingID: &l.ID, StartUTC: start,
			RecipientType: propertyrequests.RecipientAgent, OrganisationID: l.OrganisationID, AssignedUserID: l.AgentUserID,
			CustomerNote: "I would like to view this property.",
		}
		if i, ok := records[l.ID]; ok {
			r := listingRecords[i]
			a.ListingSlug, a.ListingTitle, a.ListingReference = r.Slug, r.Title, r.Reference
			if ref := r.Advertiser.Agent; ref != nil {
				a.WithKind, a.WithSlug, a.WithName = ref.Kind, ref.Slug, ref.Name
			} else if ref := r.Advertiser.Agency; ref != nil {
				a.WithKind, a.WithSlug, a.WithName = ref.Kind, ref.Slug, ref.Name
			}
		}
		return a
	}
	consultation := func(profile, status string, start time.Time) appointments.Appointment {
		p := profiles[profile]
		a := appointments.Appointment{
			Type: appointments.TypeConsultation, Status: status, StartUTC: start,
			ProfileKind: p.Kind, ProfileSlug: p.Slug, WithKind: p.Kind, WithSlug: p.Slug, WithName: p.Name,
			RecipientType: appointments.RecipientOrganisation, AssignedUserID: p.AgentUserID,
			CustomerNote: "I would like some advice before I make an offer.",
		}
		if p.Organisation != nil {
			a.OrganisationID = &p.Organisation.ID
		}
		if p.AgentUserID != nil {
			a.RecipientType = appointments.RecipientAgent
		}
		return a
	}
	professional := func(userID *uuid.UUID, event, to string, at time.Time, start *time.Time) appointments.TimelineEntry {
		return appointments.TimelineEntry{Event: event, ActorType: appointments.PartyProfessional, ActorUserID: userID, ToStatus: to, StartUTC: start, At: at}
	}

	add(viewing(1, appointments.StatusRequested, day(2, 8)), now.Add(-3*time.Hour))

	a := viewing(2, appointments.StatusConfirmed, day(3, 9))
	a.LocationNote = "Meet at the main gate of the property."
	add(a, now.Add(-26*time.Hour), professional(a.AssignedUserID, appointments.TimelineConfirmed, appointments.StatusConfirmed, now.Add(-24*time.Hour), &a.StartUTC))

	a = viewing(7, appointments.StatusRescheduleRequested, day(4, 8))
	proposed := day(5, 12)
	a.ProposedStartUTC, a.ProposedBy = &proposed, appointments.PartyProfessional
	add(a, now.Add(-20*time.Hour), professional(a.AssignedUserID, appointments.TimelineRescheduleRequested, appointments.StatusRescheduleRequested, now.Add(-2*time.Hour), &proposed))

	a = viewing(5, appointments.StatusCompleted, day(-7, 8))
	add(a, day(-10, 7),
		professional(a.AssignedUserID, appointments.TimelineConfirmed, appointments.StatusConfirmed, day(-9, 7), &a.StartUTC),
		professional(a.AssignedUserID, appointments.TimelineCompleted, appointments.StatusCompleted, day(-7, 9), nil))

	add(consultation("solicitors/sample-law-chambers", appointments.StatusRequested, day(6, 12)), now.Add(-5*time.Hour))

	a = viewing(10, appointments.StatusRequested, day(8, 8))
	a.CustomerName, a.CustomerEmail, a.CustomerPhone = "Tariro Moyo", "tariro@example.com", "+263771234567"
	add(a, now.Add(-90*time.Minute))

	a = consultation("agents/tendai-ncube", appointments.StatusConfirmed, day(1, 13))
	add(a, now.Add(-30*time.Hour), professional(a.AssignedUserID, appointments.TimelineConfirmed, appointments.StatusConfirmed, now.Add(-28*time.Hour), &a.StartUTC))

	// The tenant's consultations with the business sample accounts.
	tenant := TenantID
	asTenant := func(a appointments.Appointment) appointments.Appointment {
		a.CustomerUserID, a.CustomerName, a.CustomerEmail = &tenant, "Tariro Tenant", TenantEmail
		a.CustomerNote = "I am looking for a place to rent and would like some advice."
		return a
	}
	add(asTenant(consultation("property-managers/sample-property-management", appointments.StatusRequested, day(3, 12))), now.Add(-4*time.Hour))
	a = asTenant(consultation("developers/sample-developments", appointments.StatusConfirmed, day(5, 9)))
	add(a, now.Add(-48*time.Hour), professional(nil, appointments.TimelineConfirmed, appointments.StatusConfirmed, now.Add(-46*time.Hour), &a.StartUTC))
	add(asTenant(consultation("service-providers/sample-plumbers", appointments.StatusRequested, day(2, 13))), now.Add(-time.Hour))

	return out
}

// SeedMemoryAppointments puts the sample appointments into the in-memory store.
func SeedMemoryAppointments(store *appointments.MemoryStore, now time.Time) {
	for _, s := range Appointments(now) {
		store.Seed(s.Appointment, s.Timeline...)
	}
}
