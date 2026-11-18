package devseed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/venues"
)

// The sample showroom's hourly settings and bookings (API 0.33.0; fixed
// IDs b5xx).
var (
	// ShowroomListingID is the hourly-bookable sample showroom (Harare).
	ShowroomListingID = propertyrequests.SeedListingID(ShowroomListingNumber)
	// TenantConfirmedBookingID is tenant@example.test's CONFIRMED booking;
	// ConsumerOverlapBookingID is consumer@example.test's REQUESTED booking
	// that overlaps it (confirming it gives 409 SLOT_TAKEN);
	// TenantRequestedBookingID is the tenant's other, REQUESTED booking.
	TenantConfirmedBookingID = id("b501")
	TenantRequestedBookingID = id("b502")
	ConsumerOverlapBookingID = id("b503")
)

// ShowroomZone is the showroom's time zone (Zimbabwe's).
const ShowroomZone = "Africa/Harare"

// ShowroomSettings are the sample showroom's hourly settings: USD 50 an
// hour in 30-minute steps, 2 to 8 hours, a 30-minute cleaning buffer,
// Monday to Saturday 08:00 to 18:00 (closed on Sunday and on Christmas
// Day), 15% VAT from 2026-01-01 and a USD 100 deposit.
func ShowroomSettings(now time.Time) venues.Settings {
	deposit := "100.00"
	var hours []venues.OpeningRange
	for wd := 1; wd <= 6; wd++ {
		hours = append(hours, venues.OpeningRange{Weekday: wd, Opens: "08:00", Closes: "18:00"})
	}
	note := "Christmas Day"
	return venues.Settings{
		ListingID: ShowroomListingID, Enabled: true, Currency: "USD", RatePerHour: "50.00", IncrementMinutes: 30,
		MinMinutes: 120, MaxMinutes: 480, CleaningBufferMinutes: 30, Capacity: 1, Deposit: &deposit,
		OpeningHours: hours,
		Exceptions:   []venues.Exception{{Date: christmasAfter(now), Closed: true, Note: &note}},
		Fees:         []venues.Fee{{Code: "TAX", Label: "VAT 15%", Kind: venues.KindPercent, Amount: "15", EffectiveFrom: "2026-01-01"}},
		CancellationPolicy: "Free cancellation up to 48 hours before the start. After that the host may keep the deposit, " +
			"as agreed with you when the booking is confirmed.",
		FreeCancellationHours: 48, LeadTimeHours: 24, HorizonDays: 90,
		UpdatedAt: now.UTC().Truncate(time.Second),
	}
}

// christmasAfter is the next 25 December on or after now (Harare time).
func christmasAfter(now time.Time) string {
	local := now.In(mustZone(ShowroomZone))
	d := time.Date(local.Year(), 12, 25, 0, 0, 0, 0, time.UTC)
	if d.Before(time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)) {
		d = d.AddDate(1, 0, 0)
	}
	return d.Format("2006-01-02")
}

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// openDayAfter is the first local date at least days after now that the
// showroom is open (not a Sunday, not the exception).
func openDayAfter(now time.Time, days int, s venues.Settings) time.Time {
	loc := mustZone(ShowroomZone)
	local := now.In(loc)
	d := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, days)
	for d.Weekday() == time.Sunday || d.Format("2006-01-02") == s.Exceptions[0].Date {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// VenueSample is one seeded booking with its quote and history.
type VenueSample struct {
	Quote   venues.Quote
	Booking venues.Booking
	Events  []venues.Event
}

// VenueSamplesAt returns the showroom's settings and the sample bookings,
// relative to now: the tenant's CONFIRMED booking 10:00 to 14:00 a week
// ahead, another customer's REQUESTED booking 12:00 to 15:00 the same day
// (it overlaps), and the tenant's REQUESTED booking 09:00 to 12:00 two weeks
// ahead.
func VenueSamplesAt(now time.Time) (venues.Settings, []VenueSample) {
	now = now.UTC().Truncate(time.Second)
	s := ShowroomSettings(now)
	var org *uuid.UUID
	for _, l := range Listings() {
		if l.ID == ShowroomListingID {
			org = l.OrganisationID
		}
	}
	loc := mustZone(ShowroomZone)
	week, fortnight := openDayAfter(now, 7, s), openDayAfter(now, 14, s)
	at := func(day time.Time, hour int) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, loc).UTC()
	}
	created := now.Add(-48 * time.Hour)
	sample := func(bookingID, quoteSuffix uuid.UUID, customer uuid.UUID, name, email string, start, end time.Time, reference string) VenueSample {
		pricing, err := venues.Price(s, start, end, ShowroomZone)
		if err != nil {
			panic(fmt.Sprintf("devseed: showroom pricing: %v", err))
		}
		q := venues.Quote{ID: quoteSuffix, ListingID: ShowroomListingID, Start: start, End: end, BufferMinutes: s.CleaningBufferMinutes,
			TimeZone: ShowroomZone, Pricing: pricing, CreatedAt: created, ExpiresAt: created.Add(venues.QuoteLifetime)}
		b := venues.Booking{ID: bookingID, Reference: reference, ListingID: ShowroomListingID, ListingSlug: ShowroomListingSlug,
			ListingTitle: "Glass-fronted showroom to hire by the hour", ListingReference: "RP-ZW-10021", OrganisationID: org,
			CustomerUserID: customer, CustomerName: name, CustomerEmail: email, Status: venues.StatusRequested,
			Start: start, End: end, BufferMinutes: s.CleaningBufferMinutes, TimeZone: ShowroomZone, QuoteID: q.ID, Pricing: pricing,
			PolicyText: s.CancellationPolicy, FreeCancellationHours: s.FreeCancellationHours, PrivacyVersion: privacyVersion,
			ConsentAcceptedAt: created, CreatedAt: created, UpdatedAt: created}
		st, en := start, end
		user := customer
		e := venues.Event{ID: uuid.NewSHA1(bookingID, []byte("requested")), BookingID: bookingID, Action: venues.ActionRequested,
			ToStatus: venues.StatusRequested, ActorType: venues.PartyCustomer, ActorUserID: &user, ActorName: name, Start: &st, End: &en, At: created}
		return VenueSample{Quote: q, Booking: b, Events: []venues.Event{e}}
	}
	confirmed := sample(TenantConfirmedBookingID, id("b511"), TenantID, "Tariro Tenant", TenantEmail, at(week, 10), at(week, 14), "VB-SAMPLE01")
	confirmed.Booking.Message = "Product launch for about forty guests."
	attendees := 40
	confirmed.Booking.Attendees = &attendees
	confirmed.Booking.Status, confirmed.Booking.Held, confirmed.Booking.Confirmations = venues.StatusConfirmed, true, 1
	confirmed.Booking.UpdatedAt = created.Add(2 * time.Hour)
	st, en := confirmed.Booking.Start, confirmed.Booking.End
	owner := ShowroomsOwnerID()
	confirmed.Events = append(confirmed.Events, venues.Event{ID: uuid.NewSHA1(TenantConfirmedBookingID, []byte("confirmed")), BookingID: TenantConfirmedBookingID,
		Action: venues.ActionConfirmed, FromStatus: venues.StatusRequested, ToStatus: venues.StatusConfirmed, ActorType: venues.PartyHost,
		ActorUserID: &owner, ActorName: "Sample Showrooms Owner", Start: &st, End: &en, At: confirmed.Booking.UpdatedAt})

	overlap := sample(ConsumerOverlapBookingID, id("b513"), identity.SeedConsumerUserID, "Casey Consumer", identity.SeedConsumerEmail,
		at(week, 12), at(week, 15), "VB-SAMPLE03")
	requested := sample(TenantRequestedBookingID, id("b512"), TenantID, "Tariro Tenant", TenantEmail, at(fortnight, 9), at(fortnight, 12), "VB-SAMPLE02")
	return s, []VenueSample{confirmed, requested, overlap}
}

// ShowroomsOwnerID is the user ID of the showrooms host's owner.
func ShowroomsOwnerID() uuid.UUID {
	for _, l := range Listings() {
		if l.ID == ShowroomListingID && l.AgentUserID != nil {
			return *l.AgentUserID
		}
	}
	return uuid.Nil
}

// SeedMemoryVenues stores the showroom's settings and sample bookings.
func SeedMemoryVenues(store *venues.MemoryStore, now time.Time) {
	s, samples := VenueSamplesAt(now)
	_ = store.PutSettings(context.Background(), s)
	for _, v := range samples {
		store.Seed(v.Quote, v.Booking, v.Events...)
	}
}

// seedPostgresVenues replaces the showroom's settings and bookings (needs
// migration 0032).
func seedPostgresVenues(ctx context.Context, tx pgx.Tx, now time.Time) error {
	s, samples := VenueSamplesAt(now)
	if err := venues.PutSettingsTx(ctx, tx, s); err != nil {
		return err
	}
	for _, v := range samples {
		if err := venues.SeedTx(ctx, tx, v.Quote, v.Booking, v.Events...); err != nil {
			return fmt.Errorf("seed venue booking %s: %w", v.Booking.Reference, err)
		}
	}
	return nil
}
