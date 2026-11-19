package ics

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func sampleEvent() Event {
	start := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	return Event{
		UID: "appointment-0b6f3a52-6c1e-4b8e-9d7e-2f1c5a9e8d10@rightproperty365.com", Sequence: 1,
		Stamp: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), Start: start, End: start.Add(30 * time.Minute),
		Summary:  "Property viewing: 3-bedroom house, Borrowdale (RPA-2026-000123)",
		Location: "Meet at the gate; ask for the caretaker, then the blue door",
		Description: "Reference: RPA-2026-000123\nAppointment: Property viewing\nTime: Tuesday 17 November 2026, 10:00 (Central Africa Time)\n" +
			"Manage: https://rightproperty365.com/account/appointments",
		URL: "https://rightproperty365.com/account/appointments", Status: StatusConfirmed, TimeZone: "Africa/Harare",
		Organizer: &Address{Name: "RightProperty365", Email: "no-reply@rightproperty365.com"},
		Attendees: []Address{{Name: "Rudo Moyo", Email: "rudo@example.test"}},
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/platform/ics -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden file:\n%s", name, got)
	}
}

// checkWellFormed: CRLF endings only, no line over 75 octets, no UTF-8
// character split across a fold.
func checkWellFormed(t *testing.T, raw []byte) {
	t.Helper()
	s := string(raw)
	if !strings.HasSuffix(s, "\r\n") || strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\n") || strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\r") {
		t.Fatalf("not CRLF-only:\n%q", s)
	}
	for _, line := range strings.Split(strings.TrimSuffix(s, "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Fatalf("line of %d octets: %q", len(line), line)
		}
		if !utf8.ValidString(line) {
			t.Fatalf("a fold split a character: %q", line)
		}
	}
}

// unfold joins folded lines (RFC 5545 3.1).
func unfold(raw []byte) []string {
	return strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(raw), "\r\n ", ""), "\r\n"), "\r\n")
}

func TestGoldenRequestUpdateCancel(t *testing.T) {
	e := sampleEvent()
	request := Build(MethodRequest, e)

	moved := e
	moved.Sequence = 3
	moved.Stamp = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	moved.Start, moved.End = e.Start.Add(48*time.Hour), e.End.Add(48*time.Hour)
	moved.Description = strings.Replace(e.Description, "Tuesday 17", "Thursday 19", 1)
	update := Build(MethodRequest, moved)

	cancelled := moved
	cancelled.Sequence = 4
	cancelled.Stamp = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	cancelled.Status = StatusCancelled
	cancel := Build(MethodCancel, cancelled)

	golden(t, "request.ics", request)
	golden(t, "update.ics", update)
	golden(t, "cancel.ics", cancel)
	for _, raw := range [][]byte{request, update, cancel} {
		checkWellFormed(t, raw)
	}
	for _, want := range []string{"METHOD:CANCEL", "STATUS:CANCELLED", "SEQUENCE:4", "UID:" + e.UID, "DTSTART:20261119T080000Z"} {
		if !strings.Contains(strings.Join(unfold(cancel), "\n"), want) {
			t.Fatalf("cancel lacks %q", want)
		}
	}
}

func TestEscape(t *testing.T) {
	cases := map[string]string{
		`a,b;c\d`:                `a\,b\;c\\d`,
		"one\ntwo\r\nx":          `one\ntwo\nx`,
		"tab\there\x01!":         "tab here!",
		"Harare, Zimbabwe; café": `Harare\, Zimbabwe\; café`,
	}
	for in, want := range cases {
		if got := Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, want %q", in, got, want)
		}
	}
	// A value cannot break out of its property.
	e := sampleEvent()
	e.Summary = "Viewing\r\nATTENDEE:mailto:evil@example.test"
	for _, line := range unfold(Build(MethodRequest, e)) {
		if strings.HasPrefix(line, "ATTENDEE:mailto:evil") {
			t.Fatal("a newline in a value started a new property")
		}
	}
}

func TestFolding(t *testing.T) {
	e := sampleEvent()
	e.Description = strings.Repeat("ÿ€ multi-byte text, folded at 75 octets; ", 20)
	e.Summary = strings.Repeat("x", 74) // "SUMMARY:" + 74 = 82 octets
	raw := Build(MethodRequest, e)
	checkWellFormed(t, raw)
	found := false
	for _, line := range unfold(raw) {
		if line == "DESCRIPTION:"+Escape(e.Description) {
			found = true
		}
		if strings.HasPrefix(line, "SUMMARY:") && line != "SUMMARY:"+e.Summary {
			t.Fatalf("summary unfolds to %q", line)
		}
	}
	if !found {
		t.Fatal("the description does not unfold to the original")
	}
	// A line of exactly 75 octets is not folded.
	var w writer
	w.line(strings.Repeat("a", 75))
	if w.buf.String() != strings.Repeat("a", 75)+"\r\n" {
		t.Fatalf("75 octets folded: %q", w.buf.String())
	}
}

func TestOrganizer(t *testing.T) {
	if o := Organizer("RightProperty365 <no-reply@rightproperty365.com>"); o == nil || o.Email != "no-reply@rightproperty365.com" || o.Name != "RightProperty365" {
		t.Fatalf("%+v", o)
	}
	if o := Organizer("no-reply@rightproperty365.com"); o == nil || o.Name != DefaultOrganizerName {
		t.Fatalf("%+v", o)
	}
	if Organizer("") != nil || Organizer("not an address") != nil {
		t.Fatal("an unreadable sender must give no organizer")
	}
	// A quote in a name cannot end the CN parameter.
	if got := cn(`Evil" ;X=1`); got != `;CN="Evil ;X=1"` {
		t.Fatalf("cn = %q", got)
	}
	if !strings.HasPrefix(ContentType(MethodCancel), "text/calendar") || !strings.HasSuffix(ContentType(MethodCancel), "method=CANCEL") {
		t.Fatal(ContentType(MethodCancel))
	}
	if FormatTime(time.Date(2026, 11, 17, 10, 0, 0, 0, time.FixedZone("CAT", 2*3600))) != "20261117T080000Z" {
		t.Fatal("times must be written in UTC with Z")
	}
}
