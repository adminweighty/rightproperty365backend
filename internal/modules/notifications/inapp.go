package notifications

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/inapp"
	"rightproperty365.com/backend/internal/modules/teams"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// In-app notifications and preferences (API 0.35.0).
//
// The recipients of an in-app notification are exactly the people the
// email path already writes to (the same plans), limited to known
// accounts. Its text is SAFE TEXT ONLY, built here from a fixed title per
// template plus the reference and the listing's place: never a person's
// name, phone or email, a message, a reason or an answer (lock screens
// show it). TestInAppTextHasNoPersonalData renders every template against
// the seed.

// SetInbox connects the notification centre: in-app notifications, their
// push deliveries, and the preferences the emails consult.
func (h *Handlers) SetInbox(inbox *inapp.Service) { h.inbox = inbox }

// SetPlaceLookup names a listing's place (suburb, else city) for the
// in-app and push text.
func (h *Handlers) SetPlaceLookup(place func(ctx context.Context, listingID uuid.UUID) string) {
	h.place = place
}

func (h *Handlers) placeOf(ctx context.Context, listingID *uuid.UUID) string {
	if h.place == nil || listingID == nil || *listingID == uuid.Nil {
		return ""
	}
	return h.place(ctx, *listingID)
}

// inAppTemplate is how one email template becomes an in-app notification.
type inAppTemplate struct {
	typ string
	// title is fixed text, or built from safe labels only.
	title func(d mail.Data) string
	// path is the web page; one ending in "/" is followed by the event's
	// aggregate ID.
	path string
	// noRef leaves the reference out (account and security notices).
	noRef bool
	body  func(d mail.Data) string
}

func fixed(s string) func(mail.Data) string { return func(mail.Data) string { return s } }

// kind is the appointment type in lower case ("viewing"), or "appointment".
func kind(d mail.Data) string {
	if d.AppointmentType == "" {
		return "appointment"
	}
	return strings.ToLower(d.AppointmentType)
}

func capitalised(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Links are web pages that exist (the web's own routes). A path ending in
// "/" is followed by the event's aggregate ID (a detail page); the others
// are list pages. Every page checks sign-in and ownership itself.
const (
	pathAgentRequests     = "/agent/requests/"
	pathAgentEnquiries    = "/agent/requests#business-enquiries"
	pathAgentAppointments = "/agent/appointments"
	pathMyAppointments    = "/account/appointments"
	pathAgentApplications = "/agent/applications/"
	pathMyApplications    = "/account/applications/"
	pathHostVenueBookings = "/agent/venue-bookings"
	pathMyVenueBookings   = "/account/venue-bookings"
	pathAgentListings     = "/agent/listings/"
	pathMySubscriptions   = "/account/subscription"
	pathAccountPage       = "/account"
)

// inAppTemplates maps every email template that has an in-app
// counterpart. Receipts of a person's own action (request received,
// application submitted, booking sent), the welcome email and the
// onboarding emails have none.
var inAppTemplates = map[string]inAppTemplate{
	"property-request-new":      {typ: inapp.TypeNewLead, title: fixed("New property enquiry"), path: pathAgentRequests},
	"property-request-assigned": {typ: inapp.TypeNewLead, title: fixed("Property enquiry assigned to you"), path: pathAgentRequests},
	"business-enquiry-new":      {typ: inapp.TypeNewEnquiry, title: fixed("New business enquiry"), path: pathAgentEnquiries},

	"appointment-requested": {typ: inapp.TypeAppointmentRequested, path: pathAgentAppointments,
		title: func(d mail.Data) string { return "New " + kind(d) + " request" }},
	"appointment-reschedule-proposed": {typ: inapp.TypeAppointmentRescheduled, path: pathAgentAppointments,
		title: func(d mail.Data) string { return "New time proposed for a " + kind(d) }},
	"appointment-cancelled-by-customer": {typ: inapp.TypeAppointmentCancelled, path: pathAgentAppointments,
		title: func(d mail.Data) string { return capitalised(kind(d)) + " cancelled by the customer" }},
	"appointment-confirmed": {typ: inapp.TypeAppointmentConfirmed, path: pathMyAppointments,
		title: func(d mail.Data) string { return "Your " + kind(d) + " is confirmed" }},
	"appointment-new-time-proposed": {typ: inapp.TypeAppointmentRescheduled, path: pathMyAppointments,
		title: func(d mail.Data) string { return "A new time was proposed for your " + kind(d) }},
	"appointment-cancelled": {typ: inapp.TypeAppointmentCancelled, path: pathMyAppointments,
		title: func(d mail.Data) string { return "Your " + kind(d) + " was cancelled" }},

	"application-new":       {typ: inapp.TypeApplicationReceived, title: fixed("New application"), path: pathAgentApplications},
	"application-more-info": {typ: inapp.TypeApplicationUpdated, title: fixed("More information requested on your application"), path: pathMyApplications},
	"application-status-changed": {typ: inapp.TypeApplicationUpdated, path: pathMyApplications,
		title: func(d mail.Data) string {
			if d.StatusText == "" {
				return "Your application was updated"
			}
			return "Your application: " + d.StatusText
		}},

	"venue-booking-new":                   {typ: inapp.TypeVenueBookingRequested, title: fixed("New venue booking request"), path: pathHostVenueBookings},
	"venue-booking-accepted":              {typ: inapp.TypeVenueBookingConfirmed, title: fixed("Proposed booking time accepted"), path: pathHostVenueBookings},
	"venue-booking-cancelled-by-customer": {typ: inapp.TypeVenueBookingCancelled, title: fixed("Venue booking cancelled by the customer"), path: pathHostVenueBookings},
	"venue-booking-reschedule-requested":  {typ: inapp.TypeVenueBookingRescheduled, title: fixed("New time requested for a venue booking"), path: pathHostVenueBookings},
	"venue-booking-confirmed":             {typ: inapp.TypeVenueBookingConfirmed, title: fixed("Your venue booking is confirmed"), path: pathMyVenueBookings},
	"venue-booking-declined":              {typ: inapp.TypeVenueBookingDeclined, title: fixed("Your venue booking request was declined"), path: pathMyVenueBookings},
	"venue-booking-cancelled":             {typ: inapp.TypeVenueBookingCancelled, title: fixed("Your venue booking was cancelled"), path: pathMyVenueBookings},
	"venue-booking-time-proposed":         {typ: inapp.TypeVenueBookingRescheduled, title: fixed("A new time was proposed for your venue booking"), path: pathMyVenueBookings},

	// Maintenance (API 0.36.2): the link is the recipient's own page (the
	// request for a manager, the tenancy for a tenant), so path is empty.
	"maintenance-request-new": {typ: inapp.TypeMaintenance, title: fixed("New maintenance request"), body: maintenanceBody},
	"maintenance-status-changed": {typ: inapp.TypeMaintenance, body: maintenanceBody, title: func(d mail.Data) string {
		if d.StatusText == "" {
			return "Maintenance request updated"
		}
		return "Maintenance request: " + d.StatusText
	}},

	// Inspections (API 0.36.3): to the tenants; the link is their tenancy's page.
	"inspection-scheduled": {typ: inapp.TypeInspection, body: inspectionBody, title: func(d mail.Data) string {
		if d.StatusText == "Rescheduled" {
			return "Inspection rescheduled"
		}
		return "Inspection scheduled"
	}},
	"inspection-completed": {typ: inapp.TypeInspection, title: fixed("Inspection report ready"), body: inspectionBody},

	"listing-approved": {typ: inapp.TypeListingReviewed, path: pathAgentListings, title: func(d mail.Data) string {
		if d.Edit {
			return "Listing changes approved"
		}
		return "Listing approved"
	}},
	"listing-changes-requested": {typ: inapp.TypeListingReviewed, title: fixed("Changes requested on your listing"), path: pathAgentListings},
	"listing-rejected":          {typ: inapp.TypeListingReviewed, title: fixed("Listing not approved"), path: pathAgentListings},

	"subscription-invoice":   {typ: inapp.TypeSubscriptionInvoice, title: fixed("Subscription invoice issued"), path: pathMySubscriptions, body: subscriptionBody},
	"subscription-activated": {typ: inapp.TypeSubscriptionActivated, title: fixed("Subscription active"), path: pathMySubscriptions, body: subscriptionBody},
	"subscription-expired":   {typ: inapp.TypeSubscriptionExpired, title: fixed("Subscription ended"), path: pathMySubscriptions, body: subscriptionBody},
	"subscription-rejected":  {typ: inapp.TypeSubscriptionUpdated, title: fixed("Subscription request not approved"), path: pathMySubscriptions, body: subscriptionBody},
	"subscription-cancelled": {typ: inapp.TypeSubscriptionUpdated, title: fixed("Subscription cancelled"), path: pathMySubscriptions, body: subscriptionBody},

	"account-suspended": {typ: inapp.TypeAccount, title: fixed("Your account has been suspended"), path: pathAccountPage, noRef: true,
		body: fixed("Contact support if you think this is a mistake.")},
	"account-reactivated": {typ: inapp.TypeAccount, title: fixed("Your account has been reactivated"), path: pathAccountPage, noRef: true,
		body: fixed("You can sign in again.")},
	"password-changed": {typ: inapp.TypeSecurity, title: fixed("Your password was changed"), path: pathAccountPage, noRef: true,
		body: fixed("If this was not you, reset your password now.")},
	"admin-sign-in": {typ: inapp.TypeSecurity, title: fixed("New administrator sign-in"), path: pathAccountPage, noRef: true,
		body: fixed("If this was not you, contact a super administrator.")},
}

func subscriptionBody(d mail.Data) string { return joinSafe(d.InvoiceNumber, d.PlanName) }

// maintenanceBody is the reference and the category label (safe text).
func maintenanceBody(d mail.Data) string { return joinSafe(d.Reference, d.Service) }

// inspectionBody is the reference, the type and the scheduled time (safe text).
func inspectionBody(d mail.Data) string {
	return joinSafe(d.Reference, d.RequestType, d.AppointmentTime)
}

func joinSafe(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

// reference is the reference an in-app notification shows.
func (t inAppTemplate) reference(d mail.Data) string {
	switch {
	case t.noRef:
		return ""
	case t.typ == inapp.TypeSubscriptionInvoice || strings.HasPrefix(t.typ, "SUBSCRIPTION_"):
		return d.InvoiceNumber
	case d.Reference != "":
		return d.Reference
	}
	return d.ListingRef
}

// InAppDraft builds the in-app notification of one email template for an
// event (false when the template has none). Exported for the tests.
func InAppDraft(ev outbox.Event, template string, d mail.Data) (inapp.Draft, bool) {
	t, ok := inAppTemplates[template]
	if !ok {
		return inapp.Draft{}, false
	}
	ref := t.reference(d)
	body := joinSafe(ref, d.Place)
	if t.body != nil {
		body = t.body(d)
	}
	link := t.path
	if link == "" {
		// Per recipient: the page the email's button opens (maintenance).
		link = d.Path
		if !strings.HasPrefix(link, "/") || strings.HasPrefix(link, "//") {
			link = pathAccountPage
		}
	} else if strings.HasSuffix(link, "/") {
		link += ev.AggregateID.String()
	}
	return inapp.Draft{SourceEventID: ev.ID, Type: t.typ, Title: t.title(d), Body: body, Link: link, Reference: ref}, true
}

// emailType is the preference type that governs a template's email ("" for
// none: receipts, welcome and onboarding are always sent).
func emailType(template string) string { return inAppTemplates[template].typ }

func (h *Handlers) notifyInApp(ctx context.Context, ev outbox.Event, plans []plan) error {
	if h.inbox == nil {
		return nil
	}
	for _, p := range plans {
		if p.to.UserID == nil {
			continue // a guest has no inbox
		}
		d, ok := InAppDraft(ev, p.template, p.data)
		if !ok {
			continue
		}
		d.UserID = *p.to.UserID
		if _, _, err := h.inbox.Notify(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handlers) emailAllowed(ctx context.Context, p plan) (bool, error) {
	typ := emailType(p.template)
	if h.inbox == nil || p.to.UserID == nil || typ == "" || inapp.IsEssential(typ) {
		return true, nil
	}
	return h.inbox.EmailAllowed(ctx, *p.to.UserID, typ)
}

// recordEmail keeps the EMAIL delivery record of the in-app notification of
// the same (event, recipient). Best effort: the email log is the record of
// the email itself.
func (h *Handlers) recordEmail(ctx context.Context, ev outbox.Event, p plan, err error) {
	if h.inbox == nil || p.to.UserID == nil {
		return
	}
	if _, ok := inAppTemplates[p.template]; !ok {
		return
	}
	status, last := inapp.StatusProviderAccepted, ""
	switch {
	case err == nil:
	case errors.Is(err, mail.ErrTerminal):
		status, last = inapp.StatusDead, err.Error()
	default:
		status, last = inapp.StatusFailed, err.Error()
	}
	if rErr := h.inbox.RecordEmail(ctx, ev.ID, *p.to.UserID, status, max(ev.Attempts, 1), last); rErr != nil {
		h.logger.WarnContext(ctx, "could not record the email delivery", slog.String("error", rErr.Error()))
	}
}

// ---------------------------------------------------------------------------
// Team invitations (request path)
// ---------------------------------------------------------------------------

// SetInbox makes the invitation email also create an in-app notification
// when the invited address belongs to an account (userByEmail). The
// invitation link is a credential: it is only in the email, and the in-app
// item links to the account page.
func (m *AccountMailer) SetInbox(inbox *inapp.Service, userByEmail func(ctx context.Context, email string) (uuid.UUID, bool), logger *slog.Logger) {
	m.inbox, m.userByEmail, m.logger = inbox, userByEmail, logger
}

func (m *AccountMailer) notifyInvitation(ctx context.Context, to string, inv teams.InvitationMail) error {
	if m.inbox == nil || m.userByEmail == nil {
		return nil
	}
	userID, ok := m.userByEmail(ctx, to)
	if !ok {
		return nil
	}
	body := "Open the invitation email to accept."
	if inv.RoleLabel != "" {
		body = "Role: " + inv.RoleLabel + ". Open the invitation email to accept."
	}
	_, _, err := m.inbox.Notify(ctx, inapp.Draft{SourceEventID: uuid.New(), UserID: userID, Type: inapp.TypeTeamInvitation,
		Title: "You have been invited to join a team", Body: body, Link: pathAccountPage})
	return err
}
