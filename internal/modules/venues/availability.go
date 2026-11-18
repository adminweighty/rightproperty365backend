package venues

import (
	"sort"
	"strconv"
	"time"
	_ "time/tzdata" // every venue's zone must load on any machine
)

// Interval is a half-open span of time [Start, End).
type Interval struct{ Start, End time.Time }

// Overlaps reports whether two intervals share any time (touching does not).
func (a Interval) Overlaps(b Interval) bool { return a.Start.Before(b.End) && b.Start.Before(a.End) }

// clockMinutes reads "HH:MM" (00:00 to 24:00) as minutes after midnight.
func clockMinutes(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, false
	}
	return h*60 + m, true
}

// isoWeekday is 1 (Monday) to 7 (Sunday).
func isoWeekday(d time.Time) int {
	if wd := int(d.Weekday()); wd != 0 {
		return wd
	}
	return 7
}

// wallTime is the instant of a local date (y-m-d) at minutes after midnight
// in loc. 24:00 is the next day's midnight. Go resolves a wall time that
// does not exist (the hour skipped when clocks go forward) by moving it
// forward, and an ambiguous one (clocks going back) to its first instance.
func wallTime(y int, m time.Month, d, minutes int, loc *time.Location) time.Time {
	return time.Date(y, m, d, 0, minutes, 0, 0, loc)
}

// DaySchedule is a local date's opening ranges as instants.
type DaySchedule struct {
	Date    string
	Weekday int
	Closed  bool
	Note    *string
	Ranges  []Interval
}

// Day returns the schedule of a local date ("2006-01-02") in loc: the
// exception for that date when there is one, else the weekday's hours.
func (s Settings) Day(date string, loc *time.Location) DaySchedule {
	d, err := time.Parse(dateLayout, date)
	if err != nil {
		return DaySchedule{Date: date, Closed: true}
	}
	out := DaySchedule{Date: date, Weekday: isoWeekday(d), Ranges: []Interval{}}
	y, m, dd := d.Date()
	span := func(opens, closes string) {
		a, ok1 := clockMinutes(opens)
		b, ok2 := clockMinutes(closes)
		if ok1 && ok2 && a < b {
			out.Ranges = append(out.Ranges, Interval{Start: wallTime(y, m, dd, a, loc), End: wallTime(y, m, dd, b, loc)})
		}
	}
	exception := false
	for _, e := range s.Exceptions {
		if e.Date != date {
			continue
		}
		exception, out.Note = true, e.Note
		if !e.Closed && e.Opens != nil && e.Closes != nil {
			span(*e.Opens, *e.Closes)
		}
	}
	if !exception {
		for _, r := range s.OpeningHours {
			if r.Weekday == out.Weekday {
				span(r.Opens, r.Closes)
			}
		}
	}
	sort.Slice(out.Ranges, func(i, j int) bool { return out.Ranges[i].Start.Before(out.Ranges[j].Start) })
	out.Closed = len(out.Ranges) == 0
	return out
}

// full returns the times where at least capacity of the blocks overlap,
// merged and in order.
func full(blocks []Interval, capacity int) []Interval {
	if capacity < 1 {
		capacity = 1
	}
	type edge struct {
		at    time.Time
		delta int
	}
	edges := make([]edge, 0, 2*len(blocks))
	for _, b := range blocks {
		if b.Start.Before(b.End) {
			edges = append(edges, edge{b.Start, +1}, edge{b.End, -1})
		}
	}
	// Ends before starts at the same instant: back-to-back is not overlap.
	sort.Slice(edges, func(i, j int) bool {
		if !edges[i].at.Equal(edges[j].at) {
			return edges[i].at.Before(edges[j].at)
		}
		return edges[i].delta < edges[j].delta
	})
	var out []Interval
	n := 0
	var since time.Time
	for _, e := range edges {
		before := n
		n += e.delta
		switch {
		case before < capacity && n >= capacity:
			since = e.at
		case before >= capacity && n < capacity:
			if len(out) > 0 && !out[len(out)-1].End.Before(since) {
				out[len(out)-1].End = e.at
			} else {
				out = append(out, Interval{Start: since, End: e.at})
			}
		}
	}
	return out
}

// fits reports whether a candidate block can be added to blocks without
// going over capacity anywhere.
func fits(blocks []Interval, candidate Interval, capacity int) bool {
	for _, f := range full(blocks, capacity) {
		if f.Overlaps(candidate) {
			return false
		}
	}
	return true
}

// Occupied is a booking's slot as availability needs it: without the
// buffer (Slot) and with it (Block).
type Occupied struct {
	Slot  Interval
	Block Interval
}

// Window is when a slot may be booked at now: from the lead time, rounded
// up to the increment on the venue's clock, until the horizon.
func (s Settings) Window(now time.Time, loc *time.Location) (from, until time.Time) {
	earliest := now.Add(time.Duration(s.LeadTimeHours) * time.Hour).In(loc)
	inc := max(s.IncrementMinutes, 1)
	minutes := earliest.Hour()*60 + earliest.Minute()
	if earliest.Second() > 0 || earliest.Nanosecond() > 0 {
		minutes++
	}
	rounded := (minutes + inc - 1) / inc * inc
	y, m, d := earliest.Date()
	from = wallTime(y, m, d, rounded, loc)
	if from.Before(earliest) {
		from = earliest.Truncate(time.Minute)
	}
	return from.UTC(), now.Add(time.Duration(s.HorizonDays) * 24 * time.Hour).UTC()
}

// SlotProblem checks a slot's shape against the settings (a 400): whole
// minutes, the start on the increment of the venue's clock, the length a
// multiple of the increment between the minimum and the maximum. It
// returns per-field messages, empty when the shape is right.
func (s Settings) SlotProblem(start, end time.Time, loc *time.Location) map[string]string {
	fields := map[string]string{}
	if !end.After(start) {
		fields["end"] = "must be after start"
		return fields
	}
	inc := max(s.IncrementMinutes, 1)
	local := start.In(loc)
	if start.Second() != 0 || start.Nanosecond() != 0 || (local.Hour()*60+local.Minute())%inc != 0 {
		fields["start"] = "must be on a " + strconv.Itoa(inc) + "-minute boundary of the venue's local time"
	}
	d := end.Sub(start)
	minutes := int(d / time.Minute)
	switch {
	case d%time.Minute != 0 || minutes%inc != 0:
		fields["end"] = "the length must be a multiple of " + strconv.Itoa(inc) + " minutes"
	case minutes < s.MinMinutes:
		fields["end"] = "the booking must last at least " + humanMinutes(s.MinMinutes)
	case minutes > s.MaxMinutes:
		fields["end"] = "the booking may last at most " + humanMinutes(s.MaxMinutes)
	}
	return fields
}

func humanMinutes(m int) string {
	if m%60 == 0 {
		if m == 60 {
			return "1 hour"
		}
		return strconv.Itoa(m/60) + " hours"
	}
	return strconv.Itoa(m) + " minutes"
}

// Unavailable says why a well-shaped slot [start, end) cannot be booked at
// now, or "" when it can. occupied are the slots that take capacity (other
// bookings only), bufferMinutes the cleaning buffer the new booking would
// carry. The order of the checks is the order of the reasons: lead time,
// horizon, opening hours, then capacity (TAKEN when the bookings themselves
// overlap, BUFFER when only their cleaning buffers do).
func (s Settings) Unavailable(start, end, now time.Time, loc *time.Location, occupied []Occupied) string {
	from, until := s.Window(now, loc)
	switch {
	case start.Before(now.Add(time.Duration(s.LeadTimeHours) * time.Hour)), start.Before(from):
		return ReasonLeadTime
	case end.After(until):
		return ReasonOutsideHorizon
	}
	slot := Interval{Start: start, End: end}
	inside := false
	for _, r := range s.Day(start.In(loc).Format(dateLayout), loc).Ranges {
		if !start.Before(r.Start) && !end.After(r.End) {
			inside = true
		}
	}
	if !inside {
		return ReasonClosed
	}
	slots, blocks := make([]Interval, 0, len(occupied)), make([]Interval, 0, len(occupied))
	for _, o := range occupied {
		slots, blocks = append(slots, o.Slot), append(blocks, o.Block)
	}
	if !fits(slots, slot, s.Capacity) {
		return ReasonTaken
	}
	if !fits(blocks, Interval{Start: start, End: end.Add(time.Duration(s.CleaningBufferMinutes) * time.Minute)}, s.Capacity) {
		return ReasonBuffer
	}
	return ""
}

// Booked returns the times a new slot may not overlap: where the blocks
// fill the capacity, widened before by the current cleaning buffer (a new
// booking needs its own buffer before the next one starts).
func (s Settings) Booked(occupied []Occupied) []Interval {
	blocks := make([]Interval, 0, len(occupied))
	for _, o := range occupied {
		blocks = append(blocks, o.Block)
	}
	out := full(blocks, s.Capacity)
	buffer := time.Duration(s.CleaningBufferMinutes) * time.Minute
	for i := range out {
		out[i].Start = out[i].Start.Add(-buffer)
	}
	// Widening can make neighbours touch or overlap: merge again.
	merged := []Interval{}
	for _, iv := range out {
		if n := len(merged); n > 0 && !iv.Start.After(merged[n-1].End) {
			if iv.End.After(merged[n-1].End) {
				merged[n-1].End = iv.End
			}
			continue
		}
		merged = append(merged, iv)
	}
	return merged
}

// AvailabilityDay is one day of GET .../availability.
type AvailabilityDay struct {
	Date       string
	Weekday    int
	Closed     bool
	Note       *string
	OpenRanges []Interval
	Booked     []Interval
}

// Availability lays out the local dates from..to (inclusive) at now.
func (s Settings) Availability(from, to string, now time.Time, loc *time.Location, occupied []Occupied) []AvailabilityDay {
	start, err1 := time.Parse(dateLayout, from)
	end, err2 := time.Parse(dateLayout, to)
	if err1 != nil || err2 != nil {
		return nil
	}
	windowFrom, windowUntil := s.Window(now, loc)
	booked := s.Booked(occupied)
	var out []AvailabilityDay
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		date := d.Format(dateLayout)
		day := s.Day(date, loc)
		y, m, dd := d.Date()
		dayStart, dayEnd := wallTime(y, m, dd, 0, loc), wallTime(y, m, dd, 24*60, loc)
		item := AvailabilityDay{Date: date, Weekday: day.Weekday, Closed: day.Closed, Note: day.Note, OpenRanges: []Interval{}, Booked: []Interval{}}
		for _, r := range day.Ranges {
			if r.Start.Before(windowFrom) {
				r.Start = windowFrom
			}
			if r.End.After(windowUntil) {
				r.End = windowUntil
			}
			if r.Start.Before(r.End) {
				item.OpenRanges = append(item.OpenRanges, r)
			}
		}
		for _, b := range booked {
			if b.Start.Before(dayStart) {
				b.Start = dayStart
			}
			if b.End.After(dayEnd) {
				b.End = dayEnd
			}
			if b.Start.Before(b.End) {
				item.Booked = append(item.Booked, b)
			}
		}
		out = append(out, item)
	}
	return out
}

// localISO writes an instant in loc with its offset.
func localISO(t time.Time, loc *time.Location) string { return t.In(loc).Format(time.RFC3339) }
