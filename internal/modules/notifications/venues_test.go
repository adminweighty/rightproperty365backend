package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/venues"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Venue booking emails go to the party that did not act (and a receipt to
// the customer for a request), never with the customer's contact details
// to the host, always with the no-payment wording.
func TestVenueBookingNotifications(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	_, samples := devseed.VenueSamplesAt(now)
	owner := devseed.ShowroomsOwnerID()
	notices := map[uuid.UUID]venues.BookingNotice{}
	for _, s := range samples {
		notices[s.Booking.ID] = venues.BookingNotice{Booking: s.Booking, OrganisationID: s.Booking.OrganisationID, AgentUserID: &owner}
	}
	f.handlers.SetVenueSource(func(_ context.Context, id uuid.UUID) (venues.BookingNotice, error) {
		n, ok := notices[id]
		if !ok {
			return venues.BookingNotice{}, venues.ErrNotFound
		}
		return n, nil
	})
	b := samples[0].Booking
	event := func(typ string, p venues.EventPayload) outbox.Event {
		p.BookingID, p.Start, p.End, p.OccurredAt = b.ID, b.Start, b.End, now
		raw, _ := json.Marshal(p)
		return outbox.Event{ID: uuid.New(), Type: typ, AggregateType: venues.AggregateVenueBooking, AggregateID: b.ID, Payload: raw, CreatedAt: now}
	}
	host := "sample-showrooms@example.test"
	cases := []struct {
		name      string
		ev        outbox.Event
		templates map[string]string
	}{
		{"requested", event(venues.EventRequested, venues.EventPayload{Action: venues.ActionRequested, To: venues.StatusRequested}),
			map[string]string{"venue-booking-sent": devseed.TenantEmail, "venue-booking-new": host}},
		{"confirmed by the host", event(venues.EventConfirmed, venues.EventPayload{Action: venues.ActionConfirmed, To: venues.StatusConfirmed}),
			map[string]string{"venue-booking-confirmed": devseed.TenantEmail}},
		{"accepted by the customer", event(venues.EventConfirmed, venues.EventPayload{Action: venues.ActionAccepted, To: venues.StatusConfirmed}),
			map[string]string{"venue-booking-accepted": host, "venue-booking-confirmed": devseed.TenantEmail}},
		{"declined", event(venues.EventDeclined, venues.EventPayload{Action: venues.ActionDeclined, To: venues.StatusDeclined}),
			map[string]string{"venue-booking-declined": devseed.TenantEmail}},
		{"cancelled by the customer", event(venues.EventCancelled, venues.EventPayload{Action: venues.ActionCancelled, To: venues.StatusCancelled, CancelledBy: venues.PartyCustomer}),
			map[string]string{"venue-booking-cancelled-by-customer": host}},
		{"cancelled by the host", event(venues.EventCancelled, venues.EventPayload{Action: venues.ActionCancelled, To: venues.StatusCancelled, CancelledBy: venues.PartyHost}),
			map[string]string{"venue-booking-cancelled": devseed.TenantEmail}},
		{"reschedule", event(venues.EventRescheduleRequested, venues.EventPayload{Action: venues.ActionRescheduleRequested, To: venues.StatusRescheduleRequested,
			ProposedStart: &b.Start, ProposedEnd: &b.End}), map[string]string{"venue-booking-reschedule-requested": host}},
		{"proposed", event(venues.EventProposed, venues.EventPayload{Action: venues.ActionProposed, To: venues.StatusRescheduleRequested,
			ProposedStart: &b.Start, ProposedEnd: &b.End}), map[string]string{"venue-booking-time-proposed": devseed.TenantEmail}},
		{"completed sends nothing", event(venues.EventCompleted, venues.EventPayload{To: venues.StatusCompleted}), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := f.sender.count()
			if err := f.handle(tc.ev); err != nil {
				t.Fatal(err)
			}
			total := 0
			for template, to := range tc.templates {
				sent := f.sender.byKind(template)
				if len(sent) == 0 {
					t.Fatalf("no %s", template)
				}
				last := sent[len(sent)-1]
				if last.To != to {
					t.Fatalf("%s to %s, want %s", template, last.To, to)
				}
				if to == host && strings.Contains(last.Text, devseed.TenantEmail) {
					t.Fatalf("%s leaks the customer's email:\n%s", template, last.Text)
				}
				if !strings.Contains(last.Text, b.Reference) || !strings.Contains(last.Text, "(Central Africa Time)") {
					t.Fatalf("%s:\n%s", template, last.Text)
				}
				total++
			}
			if f.sender.count()-before != total {
				t.Fatalf("%d emails, want %d", f.sender.count()-before, total)
			}
		})
	}
	if sent := f.sender.byKind("venue-booking-sent"); len(sent) != 1 || !strings.Contains(sent[0].Text, venues.Notice) {
		t.Fatalf("the request receipt lacks the no-payment notice")
	}
}
