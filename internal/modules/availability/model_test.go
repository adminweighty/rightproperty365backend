package availability

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func d(s string) Date {
	date, err := ParseDate(s)
	if err != nil {
		panic(s)
	}
	return date
}

var (
	actor = uuid.MustParse("0195c0de-0000-7000-8000-00000000d001")
	at    = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
)

// show writes ranges as "2026-12-01..2026-12-05 BOOKED(note)".
func show(ranges []Range) string {
	var out []string
	for _, r := range ranges {
		s := fmt.Sprintf("%s..%s %s", r.First, r.Last, r.Reason)
		if r.Note != "" {
			s += "(" + r.Note + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

// calendar applies changes in order with predictable IDs (1, 2, 3, ...).
type calendar struct {
	ranges []Range
	next   int
}

func (c *calendar) apply(from, to, status, note string) string {
	c.ranges = Apply(c.ranges, Change{From: d(from), To: d(to), Status: status, Note: note}, actor, at, func() uuid.UUID {
		c.next++
		return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", c.next))
	})
	return show(c.ranges)
}

func (c *calendar) ids() string {
	var out []string
	for _, r := range c.ranges {
		out = append(out, strings.TrimLeft(r.ID.String()[24:], "0"))
	}
	return strings.Join(out, ",")
}

func TestDates(t *testing.T) {
	for _, s := range []string{"2026-11-01", "2024-02-29", "1999-12-31", "2027-01-01"} {
		if got := d(s).String(); got != s {
			t.Fatalf("%s round-trips to %s", s, got)
		}
	}
	if d("2026-03-01")-d("2026-02-28") != 1 || d("2024-03-01")-d("2024-02-28") != 2 || Nights(d("2026-12-24"), d("2026-12-27")) != 3 {
		t.Fatal("date arithmetic")
	}
	for _, bad := range []string{"", "2026-2-1", "2026-02-30", "01/02/2026", "2026-11-01T00:00:00Z", "tomorrow", " 2026-11-01"} {
		if _, err := ParseDate(bad); err == nil {
			t.Fatalf("%q was accepted as a date", bad)
		}
	}
	if raw, _ := d("2026-11-01").MarshalJSON(); string(raw) != `"2026-11-01"` {
		t.Fatalf("JSON = %s", raw)
	}
}

// The range logic, as pure functions: block, book, re-open the middle,
// adjacent merge, a change of reason and a change of note.
func TestApplyMergesAndSplits(t *testing.T) {
	var c calendar
	// Block a run of nights.
	if got := c.apply("2026-12-10", "2026-12-14", ReasonBlocked, ""); got != "2026-12-10..2026-12-14 BLOCKED" {
		t.Fatal(got)
	}
	// Book other nights: a separate range.
	if got := c.apply("2026-12-20", "2026-12-26", ReasonBooked, "The Moyos"); got != "2026-12-10..2026-12-14 BLOCKED; 2026-12-20..2026-12-26 BOOKED(The Moyos)" {
		t.Fatal(got)
	}
	// Re-open the middle of the booking: it splits in two; the left half keeps its ID.
	if got := c.apply("2026-12-22", "2026-12-23", StatusAvailable, ""); got != "2026-12-10..2026-12-14 BLOCKED; 2026-12-20..2026-12-21 BOOKED(The Moyos); 2026-12-24..2026-12-26 BOOKED(The Moyos)" {
		t.Fatal(got)
	}
	if got := c.ids(); got != "1,2,3" {
		t.Fatalf("ids = %s", got)
	}
	// Adjacent nights with the same reason and note merge into one range...
	if got := c.apply("2026-12-15", "2026-12-16", ReasonBlocked, ""); got != "2026-12-10..2026-12-16 BLOCKED; 2026-12-20..2026-12-21 BOOKED(The Moyos); 2026-12-24..2026-12-26 BOOKED(The Moyos)" {
		t.Fatal(got)
	}
	// ...which keeps the ID of the range that was there first.
	if got := c.ids(); got != "1,2,3" {
		t.Fatalf("ids after a merge = %s", got)
	}
	// Filling the gap with the same reason and note joins the two halves again.
	if got := c.apply("2026-12-22", "2026-12-23", ReasonBooked, "The Moyos"); got != "2026-12-10..2026-12-16 BLOCKED; 2026-12-20..2026-12-26 BOOKED(The Moyos)" {
		t.Fatal(got)
	}
	// Adjacent nights with ANOTHER reason stay apart.
	if got := c.apply("2026-12-17", "2026-12-19", ReasonBooked, ""); got != "2026-12-10..2026-12-16 BLOCKED; 2026-12-17..2026-12-19 BOOKED; 2026-12-20..2026-12-26 BOOKED(The Moyos)" {
		t.Fatal(got)
	}
	// A change of reason over part of a range replaces those nights only.
	if got := c.apply("2026-12-14", "2026-12-16", ReasonBooked, ""); got != "2026-12-10..2026-12-13 BLOCKED; 2026-12-14..2026-12-19 BOOKED; 2026-12-20..2026-12-26 BOOKED(The Moyos)" {
		t.Fatal(got)
	}
	// A change of note: the same reason with another note is a different range...
	if got := c.apply("2026-12-10", "2026-12-11", ReasonBlocked, "Painting"); got != "2026-12-10..2026-12-11 BLOCKED(Painting); 2026-12-12..2026-12-13 BLOCKED; 2026-12-14..2026-12-19 BOOKED; 2026-12-20..2026-12-26 BOOKED(The Moyos)" {
		t.Fatal(got)
	}
	// ...and giving the neighbour the same note merges them.
	if got := c.apply("2026-12-12", "2026-12-13", ReasonBlocked, "Painting"); got != "2026-12-10..2026-12-13 BLOCKED(Painting); 2026-12-14..2026-12-19 BOOKED; 2026-12-20..2026-12-26 BOOKED(The Moyos)" {
		t.Fatal(got)
	}
	// A change that covers several ranges swallows them.
	if got := c.apply("2026-12-12", "2026-12-22", ReasonBlocked, "Closed"); got != "2026-12-10..2026-12-11 BLOCKED(Painting); 2026-12-12..2026-12-22 BLOCKED(Closed); 2026-12-23..2026-12-26 BOOKED(The Moyos)" {
		t.Fatal(got)
	}
	// Making nights available that were never unavailable changes nothing.
	before := show(c.ranges)
	if got := c.apply("2027-01-01", "2027-01-31", StatusAvailable, ""); got != before {
		t.Fatal(got)
	}
	// Re-opening everything empties the calendar.
	if got := c.apply("2026-12-01", "2026-12-31", StatusAvailable, ""); got != "" {
		t.Fatal(got)
	}
	// One night.
	if got := c.apply("2026-12-24", "2026-12-24", ReasonBooked, ""); got != "2026-12-24..2026-12-24 BOOKED" {
		t.Fatal(got)
	}
	// Ranges never overlap and stay sorted, whatever is applied.
	c = calendar{}
	for i, ch := range [][3]string{
		{"2026-12-05", "2026-12-09", ReasonBooked}, {"2026-12-01", "2026-12-06", ReasonBlocked}, {"2026-12-08", "2026-12-20", ReasonBlocked},
		{"2026-12-03", "2026-12-03", StatusAvailable}, {"2026-12-10", "2026-12-12", ReasonBooked}, {"2026-12-02", "2026-12-18", ReasonBooked},
	} {
		c.apply(ch[0], ch[1], ch[2], "")
		for j := 1; j < len(c.ranges); j++ {
			if c.ranges[j-1].Last >= c.ranges[j].First || c.ranges[j].First > c.ranges[j].Last {
				t.Fatalf("after change %d the ranges overlap or are unsorted: %s", i, show(c.ranges))
			}
		}
	}
	if got := show(c.ranges); got != "2026-12-01..2026-12-01 BLOCKED; 2026-12-02..2026-12-18 BOOKED; 2026-12-19..2026-12-20 BLOCKED" {
		t.Fatal(got)
	}
}

func TestAvailableNightsAndTheWindow(t *testing.T) {
	today := d("2026-11-01")
	ranges := []Range{
		{First: d("2026-11-01"), Last: d("2026-11-03"), Reason: ReasonBooked},
		{First: d("2026-11-10"), Last: d("2026-11-12"), Reason: ReasonBlocked},
	}
	for night, want := range map[string]bool{
		"2026-10-31": false, // the past is never available
		"2026-11-01": false, "2026-11-03": false, "2026-11-04": true, "2026-11-09": true, "2026-11-10": false, "2026-11-13": true,
		"2027-11-01": true,  // the last night of the window (today + 365)
		"2027-11-02": false, // beyond the window
	} {
		if got := Available(ranges, d(night), today); got != want {
			t.Errorf("%s available = %v, want %v", night, got, want)
		}
	}
	if night, ok := NextAvailable(ranges, today); !ok || night != d("2026-11-04") {
		t.Fatalf("next available = %s, %v", night, ok)
	}
	if night, ok := NextAvailable(nil, today); !ok || night != today {
		t.Fatalf("next available on an empty calendar = %s, %v", night, ok)
	}
	full := []Range{{First: today, Last: today + WindowDays, Reason: ReasonBlocked}}
	if _, ok := NextAvailable(full, today); ok {
		t.Fatal("a fully blocked window has no next available night")
	}
	if night, taken := FirstUnavailable(ranges, d("2026-11-05"), d("2026-11-11"), today); !taken || night != d("2026-11-10") {
		t.Fatalf("first unavailable = %s, %v", night, taken)
	}
	if _, taken := FirstUnavailable(ranges, d("2026-11-04"), d("2026-11-09"), today); taken {
		t.Fatal("free nights were reported as taken")
	}
	if !Overlaps(ranges, d("2026-11-03"), d("2026-11-05")) || Overlaps(ranges, d("2026-11-04"), d("2026-11-09")) {
		t.Fatal("overlap")
	}
}

// "Today" is a calendar date in the listing's time zone: at one instant it
// is three different dates around the world.
func TestTodayByTimeZone(t *testing.T) {
	load := func(name string) *time.Location {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	// 22:30 UTC on 31 December.
	now := time.Date(2026, 12, 31, 22, 30, 0, 0, time.UTC)
	for zone, want := range map[string]string{
		"Europe/London":      "2026-12-31", // 22:30
		"Africa/Harare":      "2027-01-01", // 00:30 the next day
		"Pacific/Kiritimati": "2027-01-01", // UTC+14: 12:30 the next day
		"Pacific/Pago_Pago":  "2026-12-31", // UTC-11: 11:30 the same day
		"Asia/Kolkata":       "2027-01-01", // 04:00
		"America/New_York":   "2026-12-31", // 17:30
	} {
		if got := Today(now, load(zone)).String(); got != want {
			t.Errorf("today in %s = %s, want %s", zone, got, want)
		}
	}
	// 10:30 UTC: still the previous day far behind, already the next day far ahead.
	now = time.Date(2026, 6, 15, 10, 30, 0, 0, time.UTC)
	if a, b, c := Today(now, load("Pacific/Pago_Pago")), Today(now, load("Europe/London")), Today(now, load("Pacific/Kiritimati")); a.String() != "2026-06-14" || b.String() != "2026-06-15" || c.String() != "2026-06-16" {
		t.Fatalf("today = %s / %s / %s", a, b, c)
	}
}

// Validation of a PUT: the window, the past, order, sizes, statuses, notes.
func TestParseChanges(t *testing.T) {
	today := d("2026-11-01")
	ok := ChangeInput{From: "2026-11-05", To: "2026-11-07", Status: ReasonBlocked}
	note := func(s string) *string { return &s }
	changes, fields := parseChanges([]ChangeInput{ok, {From: "2026-11-01", To: "2027-11-01", Status: StatusAvailable}, {From: "2026-12-01", To: "2026-12-01", Status: ReasonBooked, Note: note("  The Moyos ")}}, today)
	if len(fields) != 0 || len(changes) != 3 || changes[2].Note != "The Moyos" || changes[1].To != today+WindowDays {
		t.Fatalf("valid changes: %v %+v", fields, changes)
	}
	for name, tt := range map[string]struct {
		in    []ChangeInput
		field string
	}{
		"no changes":              {nil, "changes"},
		"too many changes":        {make([]ChangeInput, MaxChanges+1), "changes"},
		"in the past":             {[]ChangeInput{ok, {From: "2026-10-31", To: "2026-11-02", Status: ReasonBlocked}}, "changes[1].from"},
		"beyond the window":       {[]ChangeInput{{From: "2027-10-30", To: "2027-11-02", Status: ReasonBlocked}}, "changes[0].to"},
		"to before from":          {[]ChangeInput{ok, ok, {From: "2026-11-09", To: "2026-11-08", Status: ReasonBooked}}, "changes[2].to"},
		"not a date":              {[]ChangeInput{{From: "next week", To: "2026-11-08", Status: ReasonBooked}}, "changes[0].from"},
		"a time":                  {[]ChangeInput{{From: "2026-11-08", To: "2026-11-09T10:00:00Z", Status: ReasonBooked}}, "changes[0].to"},
		"an unknown status":       {[]ChangeInput{{From: "2026-11-08", To: "2026-11-09", Status: "HELD"}}, "changes[0].status"},
		"no status":               {[]ChangeInput{{From: "2026-11-08", To: "2026-11-09"}}, "changes[0].status"},
		"a note that is too long": {[]ChangeInput{{From: "2026-11-08", To: "2026-11-09", Status: ReasonBooked, Note: note(strings.Repeat("a", MaxNoteLength+1))}}, "changes[0].note"},
		"markup in a note":        {[]ChangeInput{{From: "2026-11-08", To: "2026-11-09", Status: ReasonBooked, Note: note("<b>x</b>")}}, "changes[0].note"},
		"a note on free nights":   {[]ChangeInput{{From: "2026-11-08", To: "2026-11-09", Status: StatusAvailable, Note: note("why")}}, "changes[0].note"},
	} {
		_, fields := parseChanges(tt.in, today)
		if fields[tt.field] == "" || len(fields) != 1 {
			t.Errorf("%s: fields = %v, want only %s", name, fields, tt.field)
		}
	}
	// A note of exactly the maximum length is fine; a change of exactly the maximum span too.
	if _, fields := parseChanges([]ChangeInput{{From: "2026-11-01", To: "2027-11-01", Status: ReasonBlocked, Note: note(strings.Repeat("é", MaxNoteLength))}}, today); len(fields) != 0 {
		t.Fatalf("limits: %v", fields)
	}
}

func TestSpan(t *testing.T) {
	today := d("2026-11-01")
	from, to, fields := span("", "", today)
	if len(fields) != 0 || from != today || to != today+DefaultSpanDays {
		t.Fatalf("default span = %s..%s %v", from, to, fields)
	}
	if from, to, _ := span("2026-12-01", "", today); from != d("2026-12-01") || to != d("2026-12-01")+DefaultSpanDays {
		t.Fatalf("from alone = %s..%s", from, to)
	}
	if _, _, fields := span("2026-11-01", "2027-11-01", today); len(fields) != 0 { // 366 nights
		t.Fatalf("366 nights: %v", fields)
	}
	for name, tt := range map[string][3]string{
		"too long":       {"2026-11-01", "2027-11-02", "to"},
		"to before from": {"2026-12-01", "2026-11-30", "to"},
		"bad from":       {"1 Dec", "", "from"},
		"bad to":         {"", "2026-13-01", "to"},
	} {
		if _, _, fields := span(tt[0], tt[1], today); fields[tt[2]] == "" {
			t.Errorf("%s: %v", name, fields)
		}
	}
}

func TestStayDates(t *testing.T) {
	in, out, fields := stayDates("2026-12-24", "2026-12-27")
	if len(fields) != 0 || Nights(in, out) != 3 {
		t.Fatalf("%v", fields)
	}
	for name, tt := range map[string][3]string{
		"same day":      {"2026-12-24", "2026-12-24", "check_out"},
		"out before in": {"2026-12-24", "2026-12-20", "check_out"},
		"91 nights":     {"2026-12-01", "2027-03-02", "check_out"},
		"bad check_in":  {"24/12/2026", "2026-12-27", "check_in"},
		"bad check_out": {"2026-12-24", "", "check_out"},
	} {
		if _, _, fields := stayDates(tt[0], tt[1]); fields[tt[2]] == "" {
			t.Errorf("%s: %v", name, fields)
		}
	}
	if _, _, fields := stayDates("2026-12-01", "2027-03-01"); len(fields) != 0 { // exactly 90 nights
		t.Fatalf("90 nights: %v", fields)
	}
}
