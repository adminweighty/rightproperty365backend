package venues

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Actions a party may take (the `actions` of a booking view, and the
// action names of INVALID_BOOKING_TRANSITION).
const (
	DoCancel     = "cancel"
	DoReschedule = "reschedule"
	DoAccept     = "accept"
	DoConfirm    = "confirm"
	DoDecline    = "decline"
	DoPropose    = "propose"
	DoComplete   = "complete"
	DoNoShow     = "no_show"
)

// Allowed returns the actions party may take on b at now: the state
// machine, as one table.
//
//	REQUESTED            -> CONFIRMED (host confirm), DECLINED (host),
//	                        CANCELLED (either), RESCHEDULE_REQUESTED (host propose)
//	CONFIRMED            -> RESCHEDULE_REQUESTED (customer reschedule, host propose),
//	                        CANCELLED (either), COMPLETED / NO_SHOW (host, after the end)
//	RESCHEDULE_REQUESTED -> CONFIRMED (host confirm of the customer's time,
//	                        customer accept of the host's; both re-lock), CANCELLED (either),
//	                        RESCHEDULE_REQUESTED (a counter-proposal by the other party)
//
// A customer may cancel until the start; a host until the end. Nothing is
// confirmed once its start has passed.
func Allowed(b Booking, party string, now time.Time) []string {
	out := []string{}
	beforeStart, afterEnd := now.Before(b.Start), !now.Before(b.End)
	proposedBy := ""
	proposalAhead := false
	if b.Proposal != nil {
		proposedBy, proposalAhead = b.Proposal.By, now.Before(b.Proposal.Start)
	}
	open := b.Status == StatusRequested || b.Status == StatusConfirmed || b.Status == StatusRescheduleRequested
	switch party {
	case PartyCustomer:
		if open && beforeStart {
			out = append(out, DoCancel)
		}
		if (b.Status == StatusConfirmed && beforeStart) || (b.Status == StatusRescheduleRequested && proposedBy == PartyHost) {
			out = append(out, DoReschedule)
		}
		if b.Status == StatusRescheduleRequested && proposedBy == PartyHost && proposalAhead {
			out = append(out, DoAccept)
		}
	case PartyHost:
		if (b.Status == StatusRequested && beforeStart) || (b.Status == StatusRescheduleRequested && proposedBy == PartyCustomer && proposalAhead) {
			out = append(out, DoConfirm)
		}
		if b.Status == StatusRequested {
			out = append(out, DoDecline)
		}
		if (b.Status == StatusRequested || b.Status == StatusConfirmed) && !afterEnd ||
			(b.Status == StatusRescheduleRequested && proposedBy == PartyCustomer) {
			out = append(out, DoPropose)
		}
		if open && !afterEnd {
			out = append(out, DoCancel)
		}
		if b.Status == StatusConfirmed && afterEnd {
			out = append(out, DoComplete, DoNoShow)
		}
	}
	return out
}

func allow(b Booking, party, action string, now time.Time) error {
	allowed := Allowed(b, party, now)
	if slices.Contains(allowed, action) {
		return nil
	}
	return &TransitionError{From: b.Status, Action: action, Allowed: allowed}
}

// ---------------------------------------------------------------------------
// Requests (/v1/me/venue-bookings)
// ---------------------------------------------------------------------------

// Consent records acceptance of a privacy notice version.
type Consent struct {
	PrivacyVersion string `json:"privacy_version"`
	Accepted       bool   `json:"accepted"`
}

// RequestInput is the body of POST /v1/me/venue-bookings.
type RequestInput struct {
	QuoteID   string   `json:"quote_id"`
	Message   *string  `json:"message"`
	Attendees *int     `json:"attendees"`
	Consent   *Consent `json:"consent"`
}

// Request turns a quote into a booking request (REQUESTED). It re-checks the
// slot but holds nothing: other requests may overlap it, and only the
// host's confirmation locks it. Idempotent per quote: the same customer
// sending the same quote again gets the same booking (replayed=true).
func (s *Service) Request(ctx context.Context, p *auth.Principal, in RequestInput) (Booking, bool, error) {
	if p == nil {
		return Booking{}, false, ErrUnauthenticated
	}
	fields := map[string]string{}
	quoteID, err := uuid.Parse(strings.TrimSpace(in.QuoteID))
	if err != nil {
		fields["quote_id"] = "is required, the quote_id of a quote"
	}
	message := ""
	if in.Message != nil {
		var msg string
		if message, msg = plainText(*in.Message, maxMessageRunes, true); msg != "" {
			fields["message"] = msg
		}
	}
	if in.Attendees != nil && (*in.Attendees < 1 || *in.Attendees > maxAttendees) {
		fields["attendees"] = fmt.Sprintf("must be between 1 and %d", maxAttendees)
	}
	privacy := ""
	if in.Consent == nil {
		fields["consent"] = "is required"
	} else {
		privacy = strings.TrimSpace(in.Consent.PrivacyVersion)
		if privacy == "" || len(privacy) > maxPrivacyVersion {
			fields["consent.privacy_version"] = "is required"
		}
		if !in.Consent.Accepted {
			fields["consent.accepted"] = "must be true"
		}
	}
	if len(fields) > 0 {
		return Booking{}, false, &ValidationError{Fields: fields}
	}
	// A replay first: it succeeds even after the quote has expired.
	existing, err := s.d.Store.BookingByQuote(ctx, quoteID)
	switch {
	case err == nil && existing.CustomerUserID == p.UserID:
		return existing, true, nil
	case err == nil:
		return Booking{}, false, ErrQuoteUsed
	case !errors.Is(err, ErrNotFound):
		return Booking{}, false, err
	}
	q, err := s.d.Store.Quote(ctx, quoteID)
	if err != nil {
		return Booking{}, false, err
	}
	v, err := s.bookable(ctx, q.ListingID)
	if err != nil {
		return Booking{}, false, err
	}
	if q, err = s.usableQuote(ctx, quoteID.String(), v.listing.ID, nil); err != nil {
		return Booking{}, false, err
	}
	if err := s.check(ctx, v, q.Start, q.End, nil); err != nil {
		return Booking{}, false, err
	}
	now := s.now()
	l := v.listing
	b := Booking{ID: newID(), ListingID: l.ID, ListingSlug: l.Slug, ListingTitle: l.Title, ListingReference: l.Reference,
		OrganisationID: l.OrganisationID, CustomerUserID: p.UserID, CustomerName: p.Name, CustomerEmail: strings.ToLower(p.Email),
		Status: StatusRequested, Start: q.Start, End: q.End, BufferMinutes: q.BufferMinutes, TimeZone: q.TimeZone,
		Attendees: in.Attendees, Message: message, QuoteID: q.ID, Pricing: q.Pricing,
		PolicyText: v.settings.CancellationPolicy, FreeCancellationHours: v.settings.FreeCancellationHours,
		PrivacyVersion: privacy, ConsentAcceptedAt: now, CreatedAt: now, UpdatedAt: now}
	if l.OrganisationID == nil {
		b.SellerUserID = l.SellerUserID
	}
	start, end := b.Start, b.End
	user := p.UserID
	e := Event{ID: newID(), BookingID: b.ID, Action: ActionRequested, ToStatus: StatusRequested, ActorType: PartyCustomer,
		ActorUserID: &user, ActorName: p.Name, Start: &start, End: &end, At: now}
	for range referenceAttempts {
		if b.Reference, err = s.newRef(); err != nil {
			return Booking{}, false, err
		}
		stored, created, err := s.d.Store.CreateBooking(ctx, b, e)
		switch {
		case errors.Is(err, ErrReferenceCollision):
			continue
		case err != nil:
			return Booking{}, false, err
		case !created && stored.CustomerUserID != p.UserID:
			return Booking{}, false, ErrQuoteUsed
		}
		return stored, !created, nil
	}
	return Booking{}, false, errors.New("could not allocate a unique reference")
}

// Mine returns the customer's own booking (ErrNotFound for anybody else's).
func (s *Service) Mine(ctx context.Context, p *auth.Principal, id uuid.UUID) (Booking, error) {
	if p == nil {
		return Booking{}, ErrUnauthenticated
	}
	b, err := s.d.Store.Booking(ctx, id)
	if err != nil {
		return Booking{}, err
	}
	if b.CustomerUserID != p.UserID {
		return Booking{}, ErrNotFound
	}
	return b, nil
}

// MineList lists the customer's own bookings.
func (s *Service) MineList(ctx context.Context, p *auth.Principal, f Filter) ([]Booking, int, error) {
	if p == nil {
		return nil, 0, ErrUnauthenticated
	}
	user := p.UserID
	f.CustomerUserID, f.OrganisationID, f.SellerUserID, f.ListingIDs = &user, nil, nil, nil
	return s.d.Store.List(ctx, f)
}

// Events returns a booking's history.
func (s *Service) Events(ctx context.Context, id uuid.UUID) ([]Event, error) {
	return s.d.Store.Events(ctx, id)
}

// Now is the service's clock (the views' actions).
func (s *Service) Now() time.Time { return s.now() }

// ---------------------------------------------------------------------------
// Hosts
// ---------------------------------------------------------------------------

// Host is who a host request speaks for: an organisation, or (nil) the
// caller as the direct owner (/v1/me/hosted-venue-bookings).
type Host struct{ OrganisationID *uuid.UUID }

// hostAccess is what the caller may see under a Host.
type hostAccess struct {
	p   *auth.Principal
	org *uuid.UUID
	// agent: an AGENT, who sees the bookings of their own listings only.
	agent bool
}

func (s *Service) access(p *auth.Principal, h Host) (hostAccess, error) {
	switch {
	case p == nil:
		return hostAccess{}, ErrUnauthenticated
	case h.OrganisationID == nil:
		if !p.IsDirectOwner() {
			return hostAccess{}, ErrForbidden
		}
		return hostAccess{p: p}, nil
	case p.MaySeeAllVenueBookings(*h.OrganisationID):
		return hostAccess{p: p, org: h.OrganisationID}, nil
	case p.MembershipRole(*h.OrganisationID) == auth.RoleAgent && p.MayManageListings(*h.OrganisationID):
		return hostAccess{p: p, org: h.OrganisationID, agent: true}, nil
	}
	return hostAccess{}, ErrOrganisationUnknown
}

// sees reports whether the access covers b.
func (s *Service) sees(ctx context.Context, a hostAccess, b Booking) (bool, error) {
	if a.org == nil {
		return b.OrganisationID == nil && b.SellerUserID != nil && *b.SellerUserID == a.p.UserID, nil
	}
	if b.OrganisationID == nil || *b.OrganisationID != *a.org {
		return false, nil
	}
	if !a.agent {
		return true, nil
	}
	l, err := s.d.Listings.Listing(ctx, b.ListingID)
	if errors.Is(err, ErrListingNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return a.p.MayHandleVenueBooking(*a.org, l.AgentUserID), nil
}

// HostList lists the bookings a host handles.
func (s *Service) HostList(ctx context.Context, p *auth.Principal, h Host, f Filter) ([]Booking, int, error) {
	a, err := s.access(p, h)
	if err != nil {
		return nil, 0, err
	}
	f.CustomerUserID, f.OrganisationID, f.SellerUserID, f.ListingIDs = nil, nil, nil, nil
	if a.org == nil {
		user := p.UserID
		f.SellerUserID = &user
	} else {
		f.OrganisationID = a.org
		if a.agent {
			ids, err := s.d.Listings.AgentListingIDs(ctx, *a.org, p.UserID)
			if err != nil {
				return nil, 0, err
			}
			f.ListingIDs = append([]uuid.UUID{}, ids...)
		}
	}
	return s.d.Store.List(ctx, f)
}

// HostGet returns one booking the host handles (ErrNotFound otherwise).
func (s *Service) HostGet(ctx context.Context, p *auth.Principal, h Host, id uuid.UUID) (Booking, error) {
	a, err := s.access(p, h)
	if errors.Is(err, ErrOrganisationUnknown) || errors.Is(err, ErrForbidden) {
		return Booking{}, ErrNotFound
	}
	if err != nil {
		return Booking{}, err
	}
	b, err := s.d.Store.Booking(ctx, id)
	if err != nil {
		return Booking{}, err
	}
	ok, err := s.sees(ctx, a, b)
	if err != nil {
		return Booking{}, err
	}
	if !ok {
		return Booking{}, ErrNotFound
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// Transitions
// ---------------------------------------------------------------------------

// actor performs a transition.
type actor struct {
	party string
	p     *auth.Principal
}

// change runs one transition: who may act was checked by the caller (the
// booking is visible to them); change decides the next state inside the
// store's critical section and records the event, the outbox event and,
// for a host acting on an organisation's booking, the audit record.
func (s *Service) change(ctx context.Context, who actor, id uuid.UUID, action string,
	decide func(cur Booking, set Settings, now time.Time) (next Booking, e Event, outboxType string, lock bool, useQuote *uuid.UUID, err error)) (Booking, error) {
	now := s.now()
	return s.d.Store.Update(ctx, id, func(cur Booking, set Settings) (Mutation, error) {
		if err := allow(cur, who.party, action, now); err != nil {
			return Mutation{}, err
		}
		next, e, ob, lock, quote, err := decide(cur, set, now)
		if err != nil {
			return Mutation{}, err
		}
		next.UpdatedAt = now
		user := who.p.UserID
		e.ID, e.BookingID, e.At, e.ActorType, e.ActorUserID, e.ActorName = newID(), cur.ID, now, who.party, &user, who.p.Name
		e.FromStatus, e.ToStatus = cur.Status, next.Status
		m := Mutation{Next: next, Event: e, Outbox: ob, Lock: lock, UseQuote: quote}
		if who.party == PartyHost && cur.OrganisationID != nil {
			after := map[string]any{"status": next.Status}
			if e.Reason != "" {
				after["reason"] = e.Reason
			}
			ev := orgaudit.New(*cur.OrganisationID, user, who.p.Name, auditActions[e.Action], orgaudit.TargetVenueBooking, cur.ID, cur.Reference,
				map[string]any{"status": cur.Status}, after, now)
			m.Audit = &ev
		}
		return m, nil
	})
}

var auditActions = map[string]string{
	ActionConfirmed: orgaudit.ActionVenueBookingConfirmed,
	ActionDeclined:  orgaudit.ActionVenueBookingDeclined,
	ActionCancelled: orgaudit.ActionVenueBookingCancelled,
	ActionProposed:  orgaudit.ActionVenueBookingTimeProposed,
	ActionCompleted: orgaudit.ActionVenueBookingCompleted,
	ActionNoShow:    orgaudit.ActionVenueBookingNoShow,
}

func slotEvent(b Booking) Event {
	start, end := b.Start, b.End
	return Event{Start: &start, End: &end}
}

// confirmAt moves b to CONFIRMED at its proposal (when there is one) and
// asks the store to lock the slot.
func confirmAt(cur Booking) Booking {
	next := cur
	if cur.Proposal != nil {
		p := cur.Proposal
		next.Start, next.End, next.BufferMinutes, next.Pricing, next.QuoteID = p.Start, p.End, p.BufferMinutes, p.Pricing, p.QuoteID
	}
	next.Status, next.Held, next.Proposal = StatusConfirmed, true, nil
	next.Confirmations++
	return next
}

func reasonText(raw string, required bool) (string, error) {
	v, msg := plainText(raw, maxReasonRunes, true)
	if msg == "" && required && v == "" {
		msg = "is required"
	}
	if msg != "" {
		return "", &ValidationError{Fields: map[string]string{"reason": msg}}
	}
	return v, nil
}

// Confirm confirms a request, or the new time the customer asked for. The
// slot is locked in the same critical section that re-checks the CONFIRMED
// bookings and their buffers: a second overlapping confirmation for the
// same capacity gets ErrSlotTaken.
func (s *Service) Confirm(ctx context.Context, p *auth.Principal, h Host, id uuid.UUID) (Booking, error) {
	if _, err := s.HostGet(ctx, p, h, id); err != nil {
		return Booking{}, err
	}
	b, err := s.change(ctx, actor{PartyHost, p}, id, DoConfirm, func(cur Booking, _ Settings, _ time.Time) (Booking, Event, string, bool, *uuid.UUID, error) {
		next := confirmAt(cur)
		return next, withAction(slotEvent(next), ActionConfirmed, ""), EventConfirmed, true, nil, nil
	})
	if err == nil {
		s.audit(ctx, "venue_booking.confirmed", slog.String("booking_id", id.String()), slog.String("actor_user_id", p.UserID.String()))
	}
	return b, err
}

func withAction(e Event, action, reason string) Event {
	e.Action, e.Reason = action, reason
	return e
}

// Decline declines a request; a reason is required.
func (s *Service) Decline(ctx context.Context, p *auth.Principal, h Host, id uuid.UUID, rawReason string) (Booking, error) {
	reason, err := reasonText(rawReason, true)
	if err != nil {
		return Booking{}, err
	}
	if _, err := s.HostGet(ctx, p, h, id); err != nil {
		return Booking{}, err
	}
	return s.change(ctx, actor{PartyHost, p}, id, DoDecline, func(cur Booking, _ Settings, _ time.Time) (Booking, Event, string, bool, *uuid.UUID, error) {
		next := cur
		next.Status, next.DeclineReason, next.Held, next.Proposal = StatusDeclined, reason, false, nil
		return next, withAction(Event{}, ActionDeclined, reason), EventDeclined, false, nil, nil
	})
}

func cancelled(cur Booking, by, reason string, free bool, now time.Time) Booking {
	next := cur
	at := now
	next.Status, next.CancelledBy, next.CancelReason, next.CancelledAt, next.CancelFree = StatusCancelled, by, reason, &at, &free
	next.Held, next.Proposal = false, nil
	return next
}

// HostCancel cancels on behalf of the host; a reason is required. A host
// cancellation is always free for the customer.
func (s *Service) HostCancel(ctx context.Context, p *auth.Principal, h Host, id uuid.UUID, rawReason string) (Booking, error) {
	reason, err := reasonText(rawReason, true)
	if err != nil {
		return Booking{}, err
	}
	if _, err := s.HostGet(ctx, p, h, id); err != nil {
		return Booking{}, err
	}
	return s.change(ctx, actor{PartyHost, p}, id, DoCancel, func(cur Booking, _ Settings, now time.Time) (Booking, Event, string, bool, *uuid.UUID, error) {
		return cancelled(cur, PartyHost, reason, true, now), withAction(Event{}, ActionCancelled, reason), EventCancelled, false, nil, nil
	})
}

// CancellationFree reports whether a customer cancelling at now is inside
// the free window: up to free_cancellation_hours before the start. After
// that the policy text applies (the platform takes no payment either way).
func CancellationFree(b Booking, now time.Time) bool { return !now.After(b.FreeUntil()) }

// CustomerCancel cancels the customer's own booking, until its start.
func (s *Service) CustomerCancel(ctx context.Context, p *auth.Principal, id uuid.UUID, rawReason string) (Booking, error) {
	reason, err := reasonText(rawReason, false)
	if err != nil {
		return Booking{}, err
	}
	if _, err := s.Mine(ctx, p, id); err != nil {
		return Booking{}, err
	}
	return s.change(ctx, actor{PartyCustomer, p}, id, DoCancel, func(cur Booking, _ Settings, now time.Time) (Booking, Event, string, bool, *uuid.UUID, error) {
		return cancelled(cur, PartyCustomer, reason, CancellationFree(cur, now), now), withAction(Event{}, ActionCancelled, reason), EventCancelled, false, nil, nil
	})
}

// finish closes a CONFIRMED booking once it has ended.
func (s *Service) finish(ctx context.Context, p *auth.Principal, h Host, id uuid.UUID, do, status, action, event string) (Booking, error) {
	if _, err := s.HostGet(ctx, p, h, id); err != nil {
		return Booking{}, err
	}
	return s.change(ctx, actor{PartyHost, p}, id, do, func(cur Booking, _ Settings, _ time.Time) (Booking, Event, string, bool, *uuid.UUID, error) {
		next := cur
		next.Status = status
		return next, withAction(Event{}, action, ""), event, false, nil, nil
	})
}

// Complete records that a confirmed booking took place.
func (s *Service) Complete(ctx context.Context, p *auth.Principal, h Host, id uuid.UUID) (Booking, error) {
	return s.finish(ctx, p, h, id, DoComplete, StatusCompleted, ActionCompleted, EventCompleted)
}

// NoShow records that the customer did not come.
func (s *Service) NoShow(ctx context.Context, p *auth.Principal, h Host, id uuid.UUID) (Booking, error) {
	return s.finish(ctx, p, h, id, DoNoShow, StatusNoShow, ActionNoShow, EventNoShow)
}

// QuoteRef is the body of reschedule and propose.
type QuoteRef struct {
	QuoteID string `json:"quote_id"`
}

// propose puts a new time on the table, from a quote of the same listing
// that is still valid and available (leaving out the booking's own slot).
func (s *Service) propose(ctx context.Context, who actor, b Booking, do, action, event, rawQuote string) (Booking, error) {
	if err := allow(b, who.party, do, s.now()); err != nil {
		return Booking{}, err
	}
	self := b.ID
	q, err := s.usableQuote(ctx, rawQuote, b.ListingID, &self)
	if err != nil {
		return Booking{}, err
	}
	v, err := s.load(ctx, b.ListingID)
	if err != nil {
		return Booking{}, err
	}
	if err := s.check(ctx, v, q.Start, q.End, &self); err != nil {
		return Booking{}, err
	}
	return s.change(ctx, who, b.ID, do, func(cur Booking, _ Settings, _ time.Time) (Booking, Event, string, bool, *uuid.UUID, error) {
		next := cur
		// A confirmed slot stays held until the new time is confirmed.
		next.Held = cur.Status == StatusConfirmed || (cur.Status == StatusRescheduleRequested && cur.Held)
		next.Status = StatusRescheduleRequested
		next.Proposal = &Proposal{By: who.party, QuoteID: q.ID, Start: q.Start, End: q.End, BufferMinutes: q.BufferMinutes, Pricing: q.Pricing}
		start, end := q.Start, q.End
		qid := q.ID
		return next, Event{Action: action, Start: &start, End: &end}, event, false, &qid, nil
	})
}

// CustomerReschedule asks the host for a new time (a quote).
func (s *Service) CustomerReschedule(ctx context.Context, p *auth.Principal, id uuid.UUID, in QuoteRef) (Booking, error) {
	b, err := s.Mine(ctx, p, id)
	if err != nil {
		return Booking{}, err
	}
	return s.propose(ctx, actor{PartyCustomer, p}, b, DoReschedule, ActionRescheduleRequested, EventRescheduleRequested, in.QuoteID)
}

// Propose offers the customer a new time (a quote).
func (s *Service) Propose(ctx context.Context, p *auth.Principal, h Host, id uuid.UUID, in QuoteRef) (Booking, error) {
	b, err := s.HostGet(ctx, p, h, id)
	if err != nil {
		return Booking{}, err
	}
	return s.propose(ctx, actor{PartyHost, p}, b, DoPropose, ActionProposed, EventProposed, in.QuoteID)
}

// CustomerAccept accepts the host's proposed time: confirmed and locked
// like a host confirmation (ErrSlotTaken when the time went meanwhile).
func (s *Service) CustomerAccept(ctx context.Context, p *auth.Principal, id uuid.UUID) (Booking, error) {
	if _, err := s.Mine(ctx, p, id); err != nil {
		return Booking{}, err
	}
	return s.change(ctx, actor{PartyCustomer, p}, id, DoAccept, func(cur Booking, _ Settings, _ time.Time) (Booking, Event, string, bool, *uuid.UUID, error) {
		next := confirmAt(cur)
		return next, withAction(slotEvent(next), ActionAccepted, ""), EventConfirmed, true, nil, nil
	})
}
