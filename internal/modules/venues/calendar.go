package venues

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/ics"
	"rightproperty365.com/backend/internal/platform/mail"
)

// Calendar invitations (API 0.39.0, FR-021): one booking is one calendar
// entry with a fixed UID; SEQUENCE is derived from the booking's immutable
// history (venue_booking_events), so nothing is stored for it.

// Pages the calendar entry links to (the pages the emails open).
const (
	CalendarPathCustomer = "/account/venue-bookings"
	CalendarPathHost     = "/agent/venue-bookings"
)

// calendarReasonLength is the longest cancellation reason, as in the emails.
const calendarReasonLength = 300

// CalendarUID is the booking's calendar UID.
func CalendarUID(id uuid.UUID) string {
	return "venue-booking-" + id.String() + "@rightproperty365.com"
}

// CalendarSequence is the SEQUENCE of the entry as it was at `at`: the
// number of history events at or before it, minus one (nil: all of them).
func CalendarSequence(events []Event, at *time.Time) int {
	n := 0
	for _, e := range events {
		if at == nil || !e.At.After(*at) {
			n++
		}
	}
	return max(n-1, 0)
}

// CalendarStamp is the time of the latest history event, or fallback.
func CalendarStamp(events []Event, fallback time.Time) time.Time {
	stamp := time.Time{}
	for _, e := range events {
		if e.At.After(stamp) {
			stamp = e.At
		}
	}
	if stamp.IsZero() {
		return fallback
	}
	return stamp
}

// EverConfirmed reports whether the booking was confirmed at or before at.
func EverConfirmed(events []Event, at time.Time) bool {
	for _, e := range events {
		if e.ToStatus == StatusConfirmed && !e.At.After(at) {
			return true
		}
	}
	return false
}

// CalendarFilename is the name of the calendar file.
func CalendarFilename(b Booking) string {
	ref := strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return -1
	}, b.Reference)
	if ref == "" {
		ref = "invitation"
	}
	return "venue-booking-" + ref + ".ics"
}

// CalendarInput says which version of the entry to write and for whom.
type CalendarInput struct {
	Method string
	// ForCustomer: the reader is the customer; otherwise the host.
	ForCustomer bool
	// Start and End are the slot (the event's); zero means the booking's.
	Start, End   time.Time
	Stamp        time.Time
	Sequence     int
	PublicWebURL string
	Organizer    *ics.Address
	// Attendee is the email's own recipient (never the other party).
	Attendee *ics.Address
}

// CalendarFile writes the booking's calendar entry. It carries what the
// booking emails show and nothing more: no address (no email shows one), no
// contact details, and the customer's name never goes to the host.
func CalendarFile(b Booking, in CalendarInput) []byte {
	start, end := in.Start, in.End
	if start.IsZero() || end.IsZero() {
		start, end = b.Start, b.End
	}
	path := CalendarPathHost
	if in.ForCustomer {
		path = CalendarPathCustomer
	}
	link := strings.TrimRight(in.PublicWebURL, "/") + path

	summary := "Venue booking"
	if b.ListingTitle != "" {
		summary += ": " + b.ListingTitle
	}
	summary += " (" + b.Reference + ")"

	lines := []string{"Reference: " + b.Reference}
	if b.ListingTitle != "" {
		lines = append(lines, "Venue: "+b.ListingTitle)
	}
	if b.ListingReference != "" {
		lines = append(lines, "Listing reference: "+b.ListingReference)
	}
	lines = append(lines, "Time: "+mail.LocalSlot(start, end, b.TimeZone))
	status := ics.StatusConfirmed
	switch in.Method {
	case ics.MethodCancel:
		status = ics.StatusCancelled
		lines = append(lines, "This booking was cancelled.")
		if reason := mail.SafePreview(b.CancelReason, calendarReasonLength); reason != "" {
			lines = append(lines, "Reason: "+reason)
		}
	case ics.MethodPublish:
		status = CalendarStatus(b.Status)
	}
	lines = append(lines, "No payment is taken on RightProperty365.", "Manage: "+link)

	e := ics.Event{
		UID: CalendarUID(b.ID), Sequence: in.Sequence, Stamp: in.Stamp, Start: start, End: end,
		Summary: summary, Description: strings.Join(lines, "\n"), URL: link, Status: status, TimeZone: b.TimeZone,
		Organizer: in.Organizer,
	}
	if in.Attendee != nil && in.Organizer != nil {
		e.Attendees = []ics.Address{*in.Attendee}
	}
	return ics.Build(in.Method, e)
}

// CalendarStatus is the ICS STATUS of a booking status (a download).
func CalendarStatus(status string) string {
	switch status {
	case StatusCancelled, StatusDeclined:
		return ics.StatusCancelled
	case StatusRequested, StatusRescheduleRequested:
		return ics.StatusTentative
	}
	return ics.StatusConfirmed
}

// CalendarAttachment wraps a calendar file as an email attachment.
func CalendarAttachment(b Booking, method string, content []byte) mail.Attachment {
	return mail.Attachment{Filename: CalendarFilename(b), ContentType: ics.ContentType(method), Content: content}
}

// ---------------------------------------------------------------------------
// Downloads
// ---------------------------------------------------------------------------

// CalendarConfig is what the calendar downloads need.
type CalendarConfig struct {
	PublicWebURL string
	Organizer    *ics.Address
}

// SetCalendar configures the calendar downloads (API 0.39.0).
func (h *Handler) SetCalendar(c CalendarConfig) { h.calendar = c }

// CalendarMine serves GET /v1/me/venue-bookings/{bookingId}/calendar.ics
// (404 for anybody else's booking).
func (h *Handler) CalendarMine(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "bookingId")
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	b, err := h.svc.Mine(r.Context(), auth.FromContext(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeCalendar(w, r, b, true)
}

// Calendar serves GET .../venue-bookings/{bookingId}/calendar.ics for a host
// (404 for a booking the host does not handle).
func (x HostRoutes) Calendar(w http.ResponseWriter, r *http.Request) {
	hst, ok := host(r, x.orgParam)
	id, okID := pathID(r, "bookingId")
	if !ok || !okID {
		x.h.fail(w, r, ErrNotFound)
		return
	}
	b, err := x.h.svc.HostGet(r.Context(), auth.FromContext(r.Context()), hst, id)
	if err != nil {
		x.h.fail(w, r, err)
		return
	}
	x.h.writeCalendar(w, r, b, false)
}

func (h *Handler) writeCalendar(w http.ResponseWriter, r *http.Request, b Booking, forCustomer bool) {
	events, err := h.svc.Events(r.Context(), b.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body := CalendarFile(b, CalendarInput{
		Method: ics.MethodPublish, ForCustomer: forCustomer, Stamp: CalendarStamp(events, b.UpdatedAt),
		Sequence: CalendarSequence(events, nil), PublicWebURL: h.calendar.PublicWebURL, Organizer: h.calendar.Organizer,
	})
	private(w)
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+CalendarFilename(b)+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
