// Package ics writes RFC 5545 calendar files: one VCALENDAR holding one
// VEVENT, for the calendar invitations of appointments and venue bookings
// (API 0.39.0, FR-021).
//
// Every time is written in UTC with the "Z" suffix, so no VTIMEZONE is
// needed; the market's zone is named in X-WR-TIMEZONE and, in words, in the
// description. Lines end in CRLF and are folded at 75 octets without
// splitting a UTF-8 character; TEXT values are escaped.
package ics

import (
	"bytes"
	netmail "net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Methods (RFC 5546).
const (
	MethodRequest = "REQUEST"
	MethodCancel  = "CANCEL"
	MethodPublish = "PUBLISH"
)

// Event statuses.
const (
	StatusConfirmed = "CONFIRMED"
	StatusTentative = "TENTATIVE"
	StatusCancelled = "CANCELLED"
)

// ProductID is the PRODID of every file.
const ProductID = "-//RightProperty365//Calendar invitations//EN"

// ContentType is the media type of a calendar file with the given method,
// as an email attachment declares it.
func ContentType(method string) string {
	return "text/calendar; charset=utf-8; method=" + method
}

// Address is a calendar user: a display name and an email address.
type Address struct {
	Name  string
	Email string
}

// Event is one VEVENT.
type Event struct {
	UID      string
	Sequence int
	// Stamp is DTSTAMP: when this version of the event was made.
	Stamp      time.Time
	Start, End time.Time
	Summary    string
	// Location and URL are left out when empty.
	Location    string
	Description string
	URL         string
	Status      string
	// TimeZone is the IANA zone written as X-WR-TIMEZONE ("" leaves it out).
	TimeZone string
	// Organizer and Attendees are left out when nil / empty.
	Organizer *Address
	Attendees []Address
}

// Build writes the calendar file.
func Build(method string, e Event) []byte {
	var w writer
	w.line("BEGIN:VCALENDAR")
	w.line("PRODID:" + ProductID)
	w.line("VERSION:2.0")
	w.line("CALSCALE:GREGORIAN")
	w.line("METHOD:" + method)
	if e.TimeZone != "" {
		w.line("X-WR-TIMEZONE:" + Escape(e.TimeZone))
	}
	w.line("BEGIN:VEVENT")
	w.line("UID:" + e.UID)
	w.line("SEQUENCE:" + strconv.Itoa(max(e.Sequence, 0)))
	w.line("DTSTAMP:" + FormatTime(e.Stamp))
	w.line("DTSTART:" + FormatTime(e.Start))
	w.line("DTEND:" + FormatTime(e.End))
	w.line("SUMMARY:" + Escape(e.Summary))
	if e.Location != "" {
		w.line("LOCATION:" + Escape(e.Location))
	}
	if e.Description != "" {
		w.line("DESCRIPTION:" + Escape(e.Description))
	}
	if e.URL != "" {
		w.line("URL:" + e.URL)
	}
	if e.Status != "" {
		w.line("STATUS:" + e.Status)
	}
	w.line("TRANSP:OPAQUE")
	if e.Organizer != nil && e.Organizer.Email != "" {
		w.line("ORGANIZER" + cn(e.Organizer.Name) + ":mailto:" + e.Organizer.Email)
	}
	for _, a := range e.Attendees {
		if a.Email == "" {
			continue
		}
		w.line("ATTENDEE" + cn(a.Name) + ";ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;RSVP=FALSE:mailto:" + a.Email)
	}
	w.line("END:VEVENT")
	w.line("END:VCALENDAR")
	return w.buf.Bytes()
}

// FormatTime writes an instant as a UTC DATE-TIME ("20261117T080000Z").
func FormatTime(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// Escape escapes a TEXT value: backslash, semicolon, comma and line breaks
// (RFC 5545 section 3.3.11). Other control characters are dropped.
func Escape(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == ';':
			b.WriteString(`\;`)
		case r == ',':
			b.WriteString(`\,`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cn writes a CN parameter. A parameter value cannot be escaped, so it is
// quoted, with the characters a quoted value may not hold removed.
func cn(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '"' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	return `;CN="` + name + `"`
}

// maxLineOctets is the longest content line, CRLF excluded (RFC 5545 3.1).
const maxLineOctets = 75

type writer struct{ buf bytes.Buffer }

// line writes one content line, folded: each continuation starts with one
// space and no line is longer than 75 octets; a UTF-8 character is never split.
func (w *writer) line(s string) {
	limit := maxLineOctets
	for len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		w.buf.WriteString(s[:cut])
		w.buf.WriteString("\r\n ")
		s = s[cut:]
		limit = maxLineOctets - 1 // the leading space counts
	}
	w.buf.WriteString(s)
	w.buf.WriteString("\r\n")
}

// DefaultOrganizerName is the ORGANIZER's display name when the sender
// address has none.
const DefaultOrganizerName = "RightProperty365"

// Organizer turns the platform's sender ("Name <address>" or a bare address,
// EMAIL_FROM) into the ORGANIZER of the invitations. It is never a party's
// address: neither side's address is shared with the other. Nil when the
// sender cannot be read.
func Organizer(from string) *Address {
	a, err := netmail.ParseAddress(strings.TrimSpace(from))
	if err != nil || a.Address == "" {
		return nil
	}
	name := a.Name
	if name == "" {
		name = DefaultOrganizerName
	}
	return &Address{Name: name, Email: a.Address}
}
