package appointments

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/venues"
)

func weekdays(opens, closes string) []venues.OpeningRange {
	out := []venues.OpeningRange{}
	for d := 1; d <= 5; d++ {
		out = append(out, venues.OpeningRange{Weekday: d, Opens: opens, Closes: closes})
	}
	return out
}

func starts(day SlotDay, loc *time.Location) []string {
	out := []string{}
	for _, s := range day.Slots {
		out = append(out, s.Start.In(loc).Format("15:04"))
	}
	return out
}

func TestComputeSlots(t *testing.T) {
	loc, _ := time.LoadLocation("Africa/Harare")
	// Monday 23 November 2026, 06:00 local; lead time 0.
	now := time.Date(2026, 11, 23, 6, 0, 0, 0, loc)
	s := Settings{Enabled: true, Timezone: "Africa/Harare", WorkingHours: weekdays("09:00", "11:00"), SlotMinutes: 30, HorizonDays: 30}

	days := ComputeSlots(s, "2026-11-23", 7, 30, nil, now)
	if len(days) != 7 {
		t.Fatalf("%d days", len(days))
	}
	if got := starts(days[0], loc); len(got) != 4 || got[0] != "09:00" || got[3] != "10:30" {
		t.Fatalf("monday: %v", got)
	}
	if len(days[5].Slots) != 0 || len(days[6].Slots) != 0 {
		t.Fatal("the weekend has no hours")
	}

	// A 60-minute service stepped by 30: 09:00, 09:30, 10:00.
	if got := starts(ComputeSlots(s, "2026-11-23", 1, 60, nil, now)[0], loc); len(got) != 3 || got[2] != "10:00" {
		t.Fatalf("60 minutes: %v", got)
	}

	// Busy 09:30-10:00 with a 15-minute buffer takes 09:00 (ends 09:30,
	// within the buffer), 09:30 and 10:00 (starts within the buffer).
	busy := []Busy{{Start: time.Date(2026, 11, 23, 9, 30, 0, 0, loc), End: time.Date(2026, 11, 23, 10, 0, 0, 0, loc)}}
	s.BufferMinutes = 15
	if got := starts(ComputeSlots(s, "2026-11-23", 1, 30, busy, now)[0], loc); len(got) != 1 || got[0] != "10:30" {
		t.Fatalf("buffer: %v", got)
	}
	s.BufferMinutes = 0
	if got := starts(ComputeSlots(s, "2026-11-23", 1, 30, busy, now)[0], loc); len(got) != 3 {
		t.Fatalf("no buffer: %v", got)
	}

	// The lead time hides the morning; the daily maximum closes the day.
	s.LeadTimeHours = 4 // earliest 10:00
	if got := starts(ComputeSlots(s, "2026-11-23", 1, 30, nil, now)[0], loc); len(got) != 2 || got[0] != "10:00" {
		t.Fatalf("lead time: %v", got)
	}
	s.LeadTimeHours, s.MaxPerDay = 0, 1
	if got := ComputeSlots(s, "2026-11-23", 1, 30, busy, now)[0]; len(got.Slots) != 0 {
		t.Fatalf("max per day: %v", got.Slots)
	}

	// An exception closes a date, another gives it its own hours.
	s.MaxPerDay = 0
	afternoon, until := "14:00", "15:00"
	s.Exceptions = []venues.Exception{{Date: "2026-11-24", Closed: true}, {Date: "2026-11-25", Opens: &afternoon, Closes: &until}}
	days = ComputeSlots(s, "2026-11-23", 3, 30, nil, now)
	if len(days[1].Slots) != 0 {
		t.Fatal("closed exception")
	}
	if got := starts(days[2], loc); len(got) != 2 || got[0] != "14:00" {
		t.Fatalf("own hours: %v", got)
	}

	// The horizon ends the offer.
	s.HorizonDays = 1
	if days := ComputeSlots(s, "2026-11-23", 3, 30, nil, now); len(days[1].Slots)+len(days[2].Slots) != 0 || len(days[0].Slots) == 0 {
		t.Fatal("horizon")
	}
}

// Clocks go forward in London on Sunday 29 March 2026 at 01:00: the slots
// are wall times of that day, an hour apart in UTC from the day before.
func TestComputeSlotsAcrossDST(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	now := time.Date(2026, 3, 20, 8, 0, 0, 0, loc)
	all := []venues.OpeningRange{}
	for d := 1; d <= 7; d++ {
		all = append(all, venues.OpeningRange{Weekday: d, Opens: "09:00", Closes: "10:00"})
	}
	s := Settings{Enabled: true, Timezone: "Europe/London", WorkingHours: all, SlotMinutes: 60, HorizonDays: 30}
	days := ComputeSlots(s, "2026-03-28", 2, 60, nil, now)
	if days[0].Slots[0].Start.Format(time.RFC3339) != "2026-03-28T09:00:00Z" || days[1].Slots[0].Start.Format(time.RFC3339) != "2026-03-29T08:00:00Z" {
		t.Fatalf("DST: %v %v", days[0].Slots, days[1].Slots)
	}
}

func TestSettingsValidation(t *testing.T) {
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	user := uuid.New()
	n := func(v int) *int { return &v }
	_, err := SettingsInput{Enabled: true, Timezone: "Mars/Olympus", SlotMinutes: n(20), BufferMinutes: n(500),
		WorkingHours: []venues.OpeningRange{{Weekday: 1, Opens: "10:00", Closes: "09:00"}, {Weekday: 8, Opens: "09:00", Closes: "10:00"}},
		Exceptions:   []venues.Exception{{Date: "2026-02-30"}, {Date: "2030-01-01", Closed: true}}}.apply(user, now)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("want a validation error: %v", err)
	}
	for _, f := range []string{"timezone", "slot_minutes", "buffer_minutes", "working_hours[0].closes", "working_hours[1].weekday",
		"exceptions[0].date", "exceptions[1].date"} {
		if verr.Fields[f] == "" {
			t.Errorf("no message for %s: %v", f, verr.Fields)
		}
	}
	_, err = SettingsInput{Enabled: true, Timezone: "Africa/Harare"}.apply(user, now)
	if !errors.As(err, &verr) || verr.Fields["working_hours"] == "" {
		t.Fatalf("enabling with no hours: %v", err)
	}
	overlap := []venues.OpeningRange{{Weekday: 2, Opens: "09:00", Closes: "12:00"}, {Weekday: 2, Opens: "11:00", Closes: "13:00"}}
	if _, err := (SettingsInput{Timezone: "Africa/Harare", WorkingHours: overlap}).apply(user, now); !errors.As(err, &verr) {
		t.Fatal("overlapping ranges")
	}
	s, err := SettingsInput{Enabled: true, Timezone: "Africa/Harare", WorkingHours: weekdays("08:00", "17:00"), SlotMinutes: n(45)}.apply(user, now)
	if err != nil || s.SlotMinutes != 45 || s.LeadTimeHours != 24 || len(s.WorkingHours) != 5 {
		t.Fatalf("valid: %+v %v", s, err)
	}
	price := "10.00"
	if err := (ServiceInput{Price: &price}).apply(&ConsultationService{}, true); !errors.As(err, &verr) || verr.Fields["price"] == "" || verr.Fields["name"] == "" {
		t.Fatalf("service: %v", err)
	}
}
