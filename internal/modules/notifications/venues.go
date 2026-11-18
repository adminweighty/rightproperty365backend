package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/venues"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Where the venue booking emails send the customer and the host.
const (
	PathMyHourlyBookings  = "/account/hourly-bookings"
	PathHostVenueBookings = "/agent/venue-bookings"
)

// SetVenueSource supplies the data behind venue booking events (the venues
// service's Notice). Without it those events are dead-lettered.
func (h *Handlers) SetVenueSource(source func(ctx context.Context, id uuid.UUID) (venues.BookingNotice, error)) {
	h.venue = source
}

// venueNotes builds the emails of one venue booking event, to the party
// that did not act (and a receipt to the customer for a request):
//
//   - REQUESTED: the customer (sent, with the estimate and the no-payment
//     notice) and the host (new request);
//   - CONFIRMED by the host: the customer; ACCEPTED by the customer (the
//     host's proposed time): the host, and the customer's confirmation;
//   - DECLINED: the customer; CANCELLED: the other party;
//   - RESCHEDULE_REQUESTED: the host; PROPOSED: the customer;
//   - COMPLETED and NO_SHOW: nobody.
//
// The host is the listing's assigned agent, else the organisation's owners
// and managers, or the direct owner. No email carries the customer's
// contact details or any payment instruction.
func (h *Handlers) venueNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	if ev.Type == venues.EventCompleted || ev.Type == venues.EventNoShow {
		return nil
	}
	if h.venue == nil {
		return outbox.Permanent(fmt.Errorf("notifications: no venue booking source for event type %q", ev.Type))
	}
	var payload venues.EventPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload: %w", ev.Type, err))
	}
	n, err := h.venue(ctx, ev.AggregateID)
	if errors.Is(err, venues.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	b := n.Booking
	start, end := payload.Start, payload.End
	if start.IsZero() {
		start, end = b.Start, b.End
	}
	d := mail.Data{Reference: b.Reference, ListingTitle: b.ListingTitle, ListingRef: b.ListingReference,
		AppointmentTime: mail.LocalSlot(start, end, b.TimeZone), StatusText: venues.StatusLabel(payload.To), When: occurredAt(ev)}
	total := b.Pricing
	if payload.ProposedStart != nil && payload.ProposedEnd != nil {
		d.ProposedTime = mail.LocalSlot(*payload.ProposedStart, *payload.ProposedEnd, b.TimeZone)
		if b.Proposal != nil {
			total = b.Proposal.Pricing
		}
	}
	if total.TotalExcludingDeposit != "" {
		d.Amount = total.Currency + " " + total.TotalExcludingDeposit
	}
	customer := func(template string) error {
		d := d
		d.Name, d.Path = b.CustomerName, PathMyHourlyBookings
		user := b.CustomerUserID
		return add(template, "customer", Person{UserID: &user, Name: b.CustomerName, Email: b.CustomerEmail}, d)
	}
	host := func(template string) error {
		var people []Person
		var err error
		switch {
		case n.OrganisationID != nil:
			people, err = h.source.Recipients(ctx, n.AgentUserID, n.OrganisationID)
		case n.OwnerUserID != nil:
			people, err = h.source.Recipients(ctx, n.OwnerUserID, nil)
		}
		if err != nil {
			return err
		}
		for _, r := range people {
			d := d
			d.Name, d.Path = r.Name, PathHostVenueBookings
			if err := add(template, recipientKey(r), r, d); err != nil {
				return err
			}
		}
		return nil
	}
	switch ev.Type {
	case venues.EventRequested:
		if err := customer("venue-booking-sent"); err != nil {
			return err
		}
		return host("venue-booking-new")
	case venues.EventConfirmed:
		if payload.Action == venues.ActionAccepted {
			if err := host("venue-booking-accepted"); err != nil {
				return err
			}
		}
		return customer("venue-booking-confirmed")
	case venues.EventDeclined:
		d.Reason, d.Amount = mail.SafePreview(b.DeclineReason, 300), ""
		return customer("venue-booking-declined")
	case venues.EventCancelled:
		d.Reason, d.Amount = mail.SafePreview(b.CancelReason, 300), ""
		if payload.CancelledBy == venues.PartyCustomer {
			return host("venue-booking-cancelled-by-customer")
		}
		return customer("venue-booking-cancelled")
	case venues.EventRescheduleRequested:
		return host("venue-booking-reschedule-requested")
	case venues.EventProposed:
		return customer("venue-booking-time-proposed")
	}
	return nil
}
