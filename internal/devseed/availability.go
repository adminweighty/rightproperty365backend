package devseed

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/availability"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

// SampleCalendar is the sample availability of one short-stay listing.
type SampleCalendar struct {
	ListingID uuid.UUID
	Ranges    []availability.Range
}

// ShortStayListingNumbers are the sample short stays (see backend_samples.go).
func ShortStayListingNumbers() []int {
	var out []int
	for _, l := range AllListings() {
		if l.Category == listings.CategoryShortStay {
			var n int
			if _, err := fmt.Sscanf(l.ID[len(l.ID)-12:], "%d", &n); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// listerOf returns the account that lists sample listing n.
func listerOf(n int) uuid.UUID {
	for _, a := range Accounts() {
		for _, m := range a.Listings {
			if m == n {
				return a.ID
			}
		}
	}
	return uuid.Nil
}

// AvailabilitySamples returns the sample calendars, relative to the moment of
// seeding so they are always in the future: the Victoria Falls lodge (22) has
// a booked week and a blocked weekend, the London apartment (21) three booked
// nights, the Harare guest house (23) a blocked weekend; the villa in
// Mauritius (24) is free throughout. Every sample short stay gets a calendar,
// so re-seeding resets them all.
func AvailabilitySamples(now time.Time) []SampleCalendar {
	now = now.UTC().Truncate(time.Microsecond)
	today := availability.Today(now, time.UTC)
	// The first Saturday at least three weeks ahead.
	saturday := today + 21
	for saturday.Time().Weekday() != time.Saturday {
		saturday++
	}
	next := 0
	rng := func(n int, first, last availability.Date, reason, note string) availability.Range {
		next++
		return availability.Range{
			ID: id(fmt.Sprintf("ea%02x", next)), First: first, Last: last, Reason: reason, Note: note,
			UpdatedAt: now, UpdatedBy: listerOf(n),
		}
	}
	ranges := map[int][]availability.Range{
		21: {rng(21, today+5, today+7, availability.ReasonBooked, "Sample booking")},
		22: {
			rng(22, today+10, today+16, availability.ReasonBooked, "Sample booking: a week"),
			rng(22, saturday, saturday+1, availability.ReasonBlocked, "Owner's weekend"),
		},
		23: {rng(23, saturday, saturday+1, availability.ReasonBlocked, "Maintenance")},
	}
	var out []SampleCalendar
	for _, n := range ShortStayListingNumbers() {
		out = append(out, SampleCalendar{ListingID: propertyrequests.SeedListingID(n), Ranges: ranges[n]})
	}
	return out
}

// SeedMemoryAvailability puts the sample calendars into the in-memory store.
func SeedMemoryAvailability(store *availability.MemoryStore, now time.Time) {
	for _, c := range AvailabilitySamples(now) {
		if len(c.Ranges) > 0 {
			store.Put(c.ListingID, c.Ranges, now.UTC().Truncate(time.Microsecond))
		}
	}
}
