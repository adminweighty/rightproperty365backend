package appointments

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/ics"
	"rightproperty365.com/backend/internal/platform/mail"
)

// Calendar invitations (API 0.39.0, FR-021).
//
// One appointment is one calendar entry: its UID never changes, so the
// confirmation, a confirmed new time and the cancellation all update the same
// entry in the reader's calendar app. SEQUENCE is derived from the immutable
// timeline (see CalendarSequence), so nothing is stored for it.

// Pages the calendar entry links to (the same pages the emails open).
const (
	CalendarPathCustomer     = "/account/appointments"
	CalendarPathProfessional = "/agent/appointments"
	CalendarPathGuest        = "/"
)

// calendarLocationLength is the longest location note in a calendar entry,
// the length the "appointment confirmed" email shows.
const calendarLocationLength = 200

// calendarReasonLength is the longest cancellation reason, as in the emails.
const calendarReasonLength = 300

// CalendarUID is the appointment's calendar UID.
func CalendarUID(id uuid.UUID) string { return "appointment-" + id.String() + "@rightproperty365.com" }

// CalendarSequence is the SEQUENCE of the calendar entry as it was at `at`:
// the number of timeline entries made at or before it, minus one. The
// timeline is append-only, so the value never goes down and every later
// change has a higher one. A nil at counts every entry (a download).
func CalendarSequence(entries []TimelineEntry, at *time.Time) int {
	n := 0
	for _, e := range entries {
		if at == nil || !e.At.After(*at) {
			n++
		}
	}
	return max(n-1, 0)
}

// CalendarStamp is the time of the latest timeline entry (a download's
// DTSTAMP), or fallback when there is none.
func CalendarStamp(entries []TimelineEntry, fallback time.Time) time.Time {
	stamp := time.Time{}
	for _, e := range entries {
		if e.At.After(stamp) {
			stamp = e.At
		}
	}
	if stamp.IsZero() {
		return fallback
	}
	return stamp
}

// EverConfirmed reports whether the appointment was confirmed at or before
// at: only then can a calendar entry exist that a cancellation must remove.
func EverConfirmed(entries []TimelineEntry, at time.Time) bool {
	for _, e := range entries {
		if e.Event == TimelineConfirmed && !e.At.After(at) {
			return true
		}
	}
	return false
}

// CalendarFilename is the name of the calendar file.
func CalendarFilename(a Appointment) string {
	return "appointment-" + safeFileRef(a.Reference) + ".ics"
}

func safeFileRef(ref string) string {
	out := strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return -1
	}, ref)
	if out == "" {
		return "invitation"
	}
	return out
}

// CalendarInput says which version of the entry to write and for whom.
type CalendarInput struct {
	// Method is ics.MethodRequest, MethodCancel or MethodPublish.
	Method string
	// ForCustomer: the reader is the customer (the entry names the
	// professional's public profile); otherwise the professional.
	ForCustomer bool
	// Start is the slot's start (the event's time); zero means the
	// appointment's current start.
	Start    time.Time
	Stamp    time.Time
	Sequence int
	// PublicWebURL is the web app's origin (PUBLIC_WEB_URL).
	PublicWebURL string
	Organizer    *ics.Address
	// Attendee is the email's own recipient (never the other party); nil
	// for a download.
	Attendee *ics.Address
}

// CalendarFile writes the appointment's calendar entry. It carries only what
// the matching email (or the reader's own view) already shows: no contact
// details of the other party, and the customer's name never goes to the
// professional.
func CalendarFile(a Appointment, in CalendarInput) []byte {
	start := in.Start
	if start.IsZero() {
		start = a.StartUTC
	}
	end := start.Add(time.Duration(a.DurationMinutes) * time.Minute)
	typeName := mail.AppointmentTypeName(a.Type)

	summary := typeName
	switch {
	case a.ListingTitle != "":
		summary += ": " + a.ListingTitle
	case in.ForCustomer && a.WithName != "":
		summary += " with " + a.WithName
	}
	summary += " (" + a.Reference + ")"

	path := CalendarPathProfessional
	if in.ForCustomer {
		path = CalendarPathCustomer
		if a.CustomerUserID == nil {
			path = CalendarPathGuest
		}
	}
	link := strings.TrimRight(in.PublicWebURL, "/") + path

	location := mail.SafePreview(a.LocationNote, calendarLocationLength)
	lines := []string{"Reference: " + a.Reference, "Appointment: " + typeName}
	if a.ListingTitle != "" {
		lines = append(lines, "Property: "+a.ListingTitle)
	}
	if a.ListingReference != "" {
		lines = append(lines, "Listing reference: "+a.ListingReference)
	}
	if in.ForCustomer && a.WithName != "" {
		lines = append(lines, "With: "+a.WithName)
	}
	lines = append(lines, "Time: "+mail.LocalDateTime(start, a.Timezone))
	if location != "" {
		lines = append(lines, "Location: "+location)
	}
	status := ics.StatusConfirmed
	switch in.Method {
	case ics.MethodCancel:
		status = ics.StatusCancelled
		lines = append(lines, "This appointment was cancelled.")
		if reason := mail.SafePreview(a.CancelReason, calendarReasonLength); reason != "" {
			lines = append(lines, "Reason: "+reason)
		}
	case ics.MethodPublish:
		status = CalendarStatus(a.Status)
	}
	lines = append(lines, "Manage: "+link)

	e := ics.Event{
		UID: CalendarUID(a.ID), Sequence: in.Sequence, Stamp: in.Stamp, Start: start, End: end,
		Summary: summary, Location: location, Description: strings.Join(lines, "\n"), URL: link,
		Status: status, TimeZone: a.Timezone, Organizer: in.Organizer,
	}
	if in.Attendee != nil && in.Organizer != nil {
		e.Attendees = []ics.Address{*in.Attendee}
	}
	return ics.Build(in.Method, e)
}

// CalendarStatus is the ICS STATUS of an appointment status (a download).
func CalendarStatus(status string) string {
	switch status {
	case StatusCancelled:
		return ics.StatusCancelled
	case StatusRequested, StatusRescheduleRequested:
		return ics.StatusTentative
	}
	return ics.StatusConfirmed
}

// CalendarAttachment wraps a calendar file as an email attachment.
func CalendarAttachment(a Appointment, method string, content []byte) mail.Attachment {
	return mail.Attachment{Filename: CalendarFilename(a), ContentType: ics.ContentType(method), Content: content}
}
