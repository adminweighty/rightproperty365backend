package venues

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v.UTC()
}

// londonSettings: Monday to Saturday 08:00-18:00, Sunday 00:00-04:00 (to
// cross the clock change, which happens at 01:00 GMT on a Sunday), a
// closed Saturday and a short Monday as exceptions, a 30-minute buffer,
// 24 hours' notice and a 7-day horizon.
func londonSettings() Settings {
	s := Settings{Currency: "GBP", RatePerHour: "40.00", IncrementMinutes: 30, MinMinutes: 60, MaxMinutes: 480, CleaningBufferMinutes: 30,
		Capacity: 1, LeadTimeHours: 24, HorizonDays: 7, Enabled: true}
	for wd := 1; wd <= 6; wd++ {
		s.OpeningHours = append(s.OpeningHours, OpeningRange{Weekday: wd, Opens: "08:00", Closes: "18:00"})
	}
	s.OpeningHours = append(s.OpeningHours, OpeningRange{Weekday: 7, Opens: "00:00", Closes: "04:00"})
	note := "Stocktake"
	opens, closes := "10:00", "12:00"
	s.Exceptions = []Exception{{Date: "2026-03-28", Closed: true, Note: &note}, {Date: "2026-03-30", Opens: &opens, Closes: &closes}}
	return s
}

// Europe/London: the opening hours are wall-clock times, so their UTC
// instants move with the clocks, and the night the clocks change is an
// hour shorter (spring) or longer (autumn).
func TestDaySchedulesAcrossTheClockChange(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	s := londonSettings()
	cases := []struct {
		date       string
		closed     bool
		start, end string
	}{
		{"2026-03-27", false, "2026-03-27T08:00:00Z", "2026-03-27T18:00:00Z"}, // Friday, GMT
		{"2026-03-28", true, "", ""},                                          // exception: closed
		{"2026-03-22", false, "2026-03-22T00:00:00Z", "2026-03-22T04:00:00Z"}, // a normal Sunday: 4 hours
		{"2026-03-29", false, "2026-03-29T00:00:00Z", "2026-03-29T03:00:00Z"}, // clocks go forward: 3 hours
		{"2026-03-30", false, "2026-03-30T09:00:00Z", "2026-03-30T11:00:00Z"}, // exception hours, BST
		{"2026-03-31", false, "2026-03-31T07:00:00Z", "2026-03-31T17:00:00Z"}, // Tuesday, BST
		{"2026-10-25", false, "2026-10-24T23:00:00Z", "2026-10-25T04:00:00Z"}, // clocks go back: 5 hours
	}
	for _, c := range cases {
		d := s.Day(c.date, loc)
		if d.Closed != c.closed {
			t.Fatalf("%s closed = %v", c.date, d.Closed)
		}
		if c.closed {
			if d.Note == nil || *d.Note != "Stocktake" {
				t.Fatalf("%s note = %v", c.date, d.Note)
			}
			continue
		}
		if len(d.Ranges) != 1 || !d.Ranges[0].Start.Equal(mustTime(t, c.start)) || !d.Ranges[0].End.Equal(mustTime(t, c.end)) {
			t.Fatalf("%s ranges = %+v", c.date, d.Ranges)
		}
	}
}

func TestSlotShapeOnTheVenueClock(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	s := londonSettings()
	cases := []struct {
		start, end string
		field      string
	}{
		// 00:30 GMT to 03:30 BST is two real hours on the night the clocks go forward.
		{"2026-03-29T00:30:00Z", "2026-03-29T02:30:00Z", ""},
		// 01:15Z is 02:15 BST: off the 30-minute step of the venue's clock.
		{"2026-03-29T01:15:00Z", "2026-03-29T02:15:00Z", "start"},
		{"2026-03-31T07:00:00Z", "2026-03-31T07:45:00Z", "end"}, // not a multiple of 30 minutes
		{"2026-03-31T07:00:00Z", "2026-03-31T07:30:00Z", "end"}, // shorter than the minimum
		{"2026-03-31T07:00:00Z", "2026-03-31T16:00:00Z", "end"}, // longer than the maximum
		{"2026-03-31T07:00:00Z", "2026-03-31T07:00:00Z", "end"}, // empty
	}
	for _, c := range cases {
		f := s.SlotProblem(mustTime(t, c.start), mustTime(t, c.end), loc)
		if (c.field == "") != (len(f) == 0) || (c.field != "" && f[c.field] == "") {
			t.Fatalf("%s-%s: %v", c.start, c.end, f)
		}
	}
}

// Opening hours, exceptions, buffers, lead time and horizon, in the order
// of the reasons.
func TestUnavailableReasons(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	s := londonSettings()
	now := mustTime(t, "2026-03-27T12:00:00Z")
	// Confirmed: Tuesday 31 March 10:00-12:00 BST (09:00Z-11:00Z), buffer 30.
	booked := []Occupied{{Slot: Interval{mustTime(t, "2026-03-31T09:00:00Z"), mustTime(t, "2026-03-31T11:00:00Z")},
		Block: Interval{mustTime(t, "2026-03-31T09:00:00Z"), mustTime(t, "2026-03-31T11:30:00Z")}}}
	cases := []struct {
		start, end, want string
	}{
		{"2026-03-28T11:00:00Z", "2026-03-28T13:00:00Z", ReasonLeadTime},       // inside the 24 hours' notice
		{"2026-04-03T11:00:00Z", "2026-04-03T13:00:00Z", ReasonOutsideHorizon}, // ends after now + 7 days
		{"2026-03-28T13:00:00Z", "2026-03-28T15:00:00Z", ReasonClosed},         // the closed exception
		{"2026-03-30T10:00:00Z", "2026-03-30T12:00:00Z", ReasonClosed},         // past the exception's 12:00
		{"2026-03-31T16:00:00Z", "2026-03-31T18:00:00Z", ReasonClosed},         // past 18:00 BST
		{"2026-03-31T10:00:00Z", "2026-03-31T11:00:00Z", ReasonTaken},          // inside the booking
		{"2026-03-31T11:00:00Z", "2026-03-31T12:00:00Z", ReasonBuffer},         // starts in its cleaning time
		{"2026-03-31T08:00:00Z", "2026-03-31T09:00:00Z", ReasonBuffer},         // its own buffer runs into the booking
		{"2026-03-31T11:30:00Z", "2026-03-31T12:30:00Z", ""},                   // right after the buffer
		{"2026-03-31T07:30:00Z", "2026-03-31T08:30:00Z", ""},                   // its buffer ends as the booking starts
		{"2026-03-29T00:30:00Z", "2026-03-29T02:30:00Z", ""},                   // across the clock change, inside the 3-hour night
		{"2026-03-29T01:30:00Z", "2026-03-29T03:30:00Z", ReasonClosed},         // past 04:00 BST
	}
	for _, c := range cases {
		if got := s.Unavailable(mustTime(t, c.start), mustTime(t, c.end), now, loc, booked); got != c.want {
			t.Fatalf("%s-%s: %q, want %q", c.start, c.end, got, c.want)
		}
	}

	// Capacity 2: one booking leaves room, two fill it.
	s.Capacity = 2
	if got := s.Unavailable(mustTime(t, "2026-03-31T10:00:00Z"), mustTime(t, "2026-03-31T11:00:00Z"), now, loc, booked); got != "" {
		t.Fatalf("capacity 2 with one booking: %q", got)
	}
	two := append(booked, booked[0])
	if got := s.Unavailable(mustTime(t, "2026-03-31T10:00:00Z"), mustTime(t, "2026-03-31T11:00:00Z"), now, loc, two); got != ReasonTaken {
		t.Fatalf("capacity 2 with two bookings: %q", got)
	}
}

func TestAvailabilityDays(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	s := londonSettings()
	now := mustTime(t, "2026-03-27T12:10:00Z")
	booked := []Occupied{{Slot: Interval{mustTime(t, "2026-03-31T09:00:00Z"), mustTime(t, "2026-03-31T11:00:00Z")},
		Block: Interval{mustTime(t, "2026-03-31T09:00:00Z"), mustTime(t, "2026-03-31T11:30:00Z")}}}
	from, until := s.Window(now, loc)
	// 24 hours later is 12:10 GMT, rounded up to the 30-minute step.
	if !from.Equal(mustTime(t, "2026-03-28T12:30:00Z")) || !until.Equal(mustTime(t, "2026-04-03T12:10:00Z")) {
		t.Fatalf("window %s .. %s", from, until)
	}
	days := s.Availability("2026-03-27", "2026-04-04", now, loc, booked)
	if len(days) != 9 {
		t.Fatalf("%d days", len(days))
	}
	byDate := map[string]AvailabilityDay{}
	for _, d := range days {
		byDate[d.Date] = d
	}
	if d := byDate["2026-03-27"]; d.Closed || len(d.OpenRanges) != 0 {
		t.Fatalf("today is inside the lead time: %+v", d)
	}
	if d := byDate["2026-03-28"]; !d.Closed || len(d.OpenRanges) != 0 || d.Weekday != 6 {
		t.Fatalf("closed exception: %+v", d)
	}
	if d := byDate["2026-03-29"]; d.Weekday != 7 || len(d.OpenRanges) != 1 || d.OpenRanges[0].End.Sub(d.OpenRanges[0].Start) != 3*time.Hour {
		t.Fatalf("clock change night: %+v", d)
	}
	d := byDate["2026-03-31"]
	if len(d.OpenRanges) != 1 || len(d.Booked) != 1 ||
		!d.Booked[0].Start.Equal(mustTime(t, "2026-03-31T08:30:00Z")) || !d.Booked[0].End.Equal(mustTime(t, "2026-03-31T11:30:00Z")) {
		t.Fatalf("booked widened by the buffers: %+v", d)
	}
	if got := localISO(d.OpenRanges[0].Start, loc); got != "2026-03-31T08:00:00+01:00" {
		t.Fatalf("local start %s", got)
	}
	// Beyond the horizon: open, but nothing bookable.
	if d := byDate["2026-04-04"]; d.Closed || len(d.OpenRanges) != 0 {
		t.Fatalf("beyond the horizon: %+v", d)
	}
	// The horizon cuts the day it falls on.
	if d := byDate["2026-04-03"]; len(d.OpenRanges) != 1 || !d.OpenRanges[0].End.Equal(until) {
		t.Fatalf("horizon day: %+v", d)
	}
}

func TestFullCapacity(t *testing.T) {
	h := func(n int) time.Time { return time.Date(2026, 1, 1, n, 0, 0, 0, time.UTC) }
	blocks := []Interval{{h(1), h(3)}, {h(2), h(4)}, {h(4), h(5)}}
	if got := full(blocks, 2); len(got) != 1 || !got[0].Start.Equal(h(2)) || !got[0].End.Equal(h(3)) {
		t.Fatalf("capacity 2: %+v", got)
	}
	// Back-to-back blocks merge into one full range at capacity 1.
	if got := full(blocks, 1); len(got) != 1 || !got[0].Start.Equal(h(1)) || !got[0].End.Equal(h(5)) {
		t.Fatalf("capacity 1: %+v", got)
	}
	if !fits(blocks, Interval{h(5), h(6)}, 1) || fits(blocks, Interval{h(4), h(6)}, 1) {
		t.Fatal("fits")
	}
}
