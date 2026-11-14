// Package availability is the availability calendar of short-stay listings:
// whoever lists the property marks nights unavailable, and the public sees
// which nights are free.
//
// It is NOT a booking engine. There are no payments, no instant booking and
// no holds: a guest asks to stay through a property request (STAY_REQUEST),
// and the lister then marks the nights BOOKED themselves.
//
// The model:
//
//   - Dates are calendar NIGHTS in the time zone of the listing's country,
//     written YYYY-MM-DD, with no time of day. A stay from check_in to
//     check_out occupies the nights check_in .. check_out-1.
//   - Every night is AVAILABLE unless marked otherwise, inside a booking
//     window from today (in that time zone) to WindowDays ahead. Nights in
//     the past or beyond the window are never available.
//   - Unavailable nights are stored as inclusive ranges [first_night,
//     last_night] with a PRIVATE reason (BLOCKED: the owner's choice; BOOKED:
//     taken) and an optional PRIVATE note. The public only ever learns
//     available / unavailable per night.
package availability

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Statuses of a change, and reasons of a stored range (the two unavailable ones).
const (
	StatusAvailable = "AVAILABLE"
	ReasonBlocked   = "BLOCKED"
	ReasonBooked    = "BOOKED"
)

// Limits.
const (
	// WindowDays is how far ahead a night can be available or changed: the
	// window is today .. today+WindowDays, both inclusive.
	WindowDays = 365
	// DefaultSpanDays is the default length of a calendar read: today .. +90.
	DefaultSpanDays = 90
	// MaxSpanNights bounds a calendar read and one change.
	MaxSpanNights = 366
	// MaxChanges bounds one PUT.
	MaxChanges = 50
	// MaxNoteLength bounds the private note of a range.
	MaxNoteLength = 200
	// MaxStayNights bounds a stay in a search or a stay request.
	MaxStayNights = 90
	// Stay rule bounds.
	MaxMinNights = 365
	MaxGuests    = 50
)

// Date is a calendar date: the number of days since 1970-01-01. It has no
// time of day and no time zone; which zone it is read in is the listing's.
type Date int

const dateLayout = "2006-01-02"

// ParseDate reads a YYYY-MM-DD date.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil || len(s) != len(dateLayout) {
		return 0, errors.New("not a YYYY-MM-DD date")
	}
	return Date(t.Unix() / 86400), nil
}

// String writes the date as YYYY-MM-DD.
func (d Date) String() string {
	return time.Unix(int64(d)*86400, 0).UTC().Format(dateLayout)
}

// Time returns midnight UTC of the date (for formatting weekdays and months).
func (d Date) Time() time.Time { return time.Unix(int64(d)*86400, 0).UTC() }

// MarshalJSON writes the date as a JSON string.
func (d Date) MarshalJSON() ([]byte, error) { return []byte(`"` + d.String() + `"`), nil }

// Today returns the calendar date of an instant in a time zone.
func Today(now time.Time, loc *time.Location) Date {
	y, m, d := now.In(loc).Date()
	return Date(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

// Range is a run of unavailable nights, both ends inclusive.
type Range struct {
	ID        uuid.UUID
	First     Date
	Last      Date
	Reason    string // ReasonBlocked or ReasonBooked
	Note      string // private; "" when none
	UpdatedAt time.Time
	UpdatedBy uuid.UUID
}

// Change marks an inclusive run of nights AVAILABLE, BLOCKED or BOOKED.
type Change struct {
	From   Date
	To     Date
	Status string
	Note   string
}

// Audit is one entry of the trail: who changed which nights to what, when.
type Audit struct {
	ID        uuid.UUID
	ListingID uuid.UUID
	ActorID   uuid.UUID
	From      Date
	To        Date
	Status    string
	Note      string
	At        time.Time
}

// State is a listing's calendar as stored.
type State struct {
	// Ranges are sorted by first night and never overlap.
	Ranges []Range
	// UpdatedAt is when the calendar was last changed; nil when it never was.
	UpdatedAt *time.Time
}

// Apply returns the ranges after one change. It is a pure function:
//
//   - the nights of the change are first cut out of every existing range (a
//     range inside them disappears, one that sticks out is trimmed, one that
//     contains them is split in two);
//   - unless the change makes the nights AVAILABLE, a new range is added;
//   - neighbours that touch and carry the same reason and note are merged.
//
// A range that is not touched keeps its ID and timestamps. newID supplies the
// IDs of new ranges (the right half of a split, and the range of the change).
func Apply(ranges []Range, c Change, actor uuid.UUID, at time.Time, newID func() uuid.UUID) []Range {
	out := make([]Range, 0, len(ranges)+2)
	for _, r := range ranges {
		switch {
		case r.Last < c.From || r.First > c.To: // untouched
			out = append(out, r)
		case r.First < c.From && r.Last > c.To: // contains the change: split
			left, right := r, r
			left.Last, left.UpdatedAt, left.UpdatedBy = c.From-1, at, actor
			right.ID, right.First, right.UpdatedAt, right.UpdatedBy = newID(), c.To+1, at, actor
			out = append(out, left, right)
		case r.First < c.From: // sticks out on the left
			r.Last, r.UpdatedAt, r.UpdatedBy = c.From-1, at, actor
			out = append(out, r)
		case r.Last > c.To: // sticks out on the right
			r.First, r.UpdatedAt, r.UpdatedBy = c.To+1, at, actor
			out = append(out, r)
		}
	}
	if c.Status != StatusAvailable {
		out = append(out, Range{ID: newID(), First: c.From, Last: c.To, Reason: c.Status, Note: c.Note, UpdatedAt: at, UpdatedBy: actor})
	}
	return merge(out, actor, at)
}

// merge sorts ranges and joins neighbours that touch (or overlap) and carry
// the same reason and note. The joined range keeps the left one's ID.
func merge(ranges []Range, actor uuid.UUID, at time.Time) []Range {
	sort.SliceStable(ranges, func(i, j int) bool { return ranges[i].First < ranges[j].First })
	out := make([]Range, 0, len(ranges))
	for _, r := range ranges {
		if n := len(out); n > 0 && out[n-1].Last+1 >= r.First && out[n-1].Reason == r.Reason && out[n-1].Note == r.Note {
			if r.Last > out[n-1].Last {
				out[n-1].Last = r.Last
			}
			out[n-1].UpdatedAt, out[n-1].UpdatedBy = at, actor
			continue
		}
		out = append(out, r)
	}
	return out
}

// find returns the range covering a night, if any.
func find(ranges []Range, night Date) (Range, bool) {
	for _, r := range ranges {
		if r.First <= night && night <= r.Last {
			return r, true
		}
	}
	return Range{}, false
}

// Available reports whether a night can be stayed: inside the window that
// starts today and not covered by a range.
func Available(ranges []Range, night, today Date) bool {
	if night < today || night > today+WindowDays {
		return false
	}
	_, taken := find(ranges, night)
	return !taken
}

// FirstUnavailable returns the first night of first..last that cannot be
// stayed, and whether there is one.
func FirstUnavailable(ranges []Range, first, last, today Date) (Date, bool) {
	for night := first; night <= last; night++ {
		if !Available(ranges, night, today) {
			return night, true
		}
	}
	return 0, false
}

// NextAvailable returns the first available night from today on, and whether
// the window has one.
func NextAvailable(ranges []Range, today Date) (Date, bool) {
	for night := today; night <= today+WindowDays; night++ {
		if Available(ranges, night, today) {
			return night, true
		}
	}
	return 0, false
}

// Overlaps reports whether any range touches first..last.
func Overlaps(ranges []Range, first, last Date) bool {
	for _, r := range ranges {
		if r.First <= last && r.Last >= first {
			return true
		}
	}
	return false
}

// Nights is the number of nights of a stay from checkIn to checkOut.
func Nights(checkIn, checkOut Date) int { return int(checkOut - checkIn) }

// Errors.
var (
	// ErrNotFound: no such listing, or one the caller may not manage.
	ErrNotFound = errors.New("listing not found")
	// ErrNotShortStay: the listing is not in the short-stay category.
	ErrNotShortStay = errors.New("listing is not a short stay")
	// ErrForbidden: the caller manages no listings at all.
	ErrForbidden = errors.New("caller manages no listings")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return fmt.Sprintf("validation failed: %v", e.Fields) }

// UnavailableError says a stay cannot be had: Night is the first night of it
// that is not available.
type UnavailableError struct{ Night Date }

func (e *UnavailableError) Error() string { return "dates unavailable from " + e.Night.String() }
