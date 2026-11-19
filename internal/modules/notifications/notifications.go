// Package notifications turns things that happened into emails.
//
// There are two paths, chosen by whether the email carries a secret:
//
//   - Notification emails (a request or enquiry was received, welcome,
//     password changed, administrator sign-in) are driven by the transactional
//     outbox: the module that made the change wrote an event in the same
//     transaction, and Handlers turns that event into one email per recipient.
//     Delivery is at-least-once at the event level and exactly-once at the
//     email level (see mail.Service).
//   - Emails with a secret link (verify your address, reset your password) are
//     sent by AccountMailer straight from the request, because the token must
//     never be written to an outbox payload or a log.
//
// Privacy: an email to an agent, seller or business owner never contains the
// customer's phone number or email address — it carries a reference, a short
// preview with contact details removed, and a button to the inbox. An email
// to a customer never contains the recipient business's contact details.
package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/accounts"
	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/inapp"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/modules/teams"
	"rightproperty365.com/backend/internal/modules/venues"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Site paths the emails link to. Links are always PUBLIC_WEB_URL + one of these.
const (
	PathHome        = "/"
	PathVerifyEmail = "/verify-email"
	PathResetPass   = "/reset-password"
	PathSignIn      = "/sign-in"
	PathAccount     = "/account"
	PathInbox       = "/agent/requests"
	PathAdminSignIn = "/admin/sign-in"
	// Where the appointment emails send the professional and the customer.
	PathAppointmentsInbox = "/agent/appointments"
	PathMyAppointments    = "/account/appointments"
	// Where the listing review emails send the advertiser.
	PathMyListings = "/agent/listings"
	// PathInvitation is followed by the invitation token.
	PathInvitation = "/invitations/"
	// PathSigning is followed by a recipient's signing token;
	// PathMyAgreements by an agreement's ID (the sender's page).
	PathSigning      = "/sign/"
	PathMyAgreements = "/agent/agreements/"
)

// previewLength is the size of the message preview in emails to recipients.
const previewLength = 140

// ErrNotFound is returned by a Source when the subject of an event is gone.
var ErrNotFound = errors.New("notification subject not found")

// Person is someone an email is addressed to.
type Person struct {
	UserID *uuid.UUID
	Name   string
	Email  string
}

// PropertyRequestInfo is what the emails about a property request need.
type PropertyRequestInfo struct {
	Reference   string
	ListingID   uuid.UUID
	RequestType string
	Message     string
	CreatedAt   time.Time
	Customer    Person
	// Stay is the dates, nights and guests of a request to stay; nil otherwise.
	Stay *propertyrequests.StayDetails
	// Recipients are the people the request was routed to: the assigned agent
	// or direct seller, otherwise the organisation's owners and managers.
	// Empty for a routing exception.
	Recipients []Person
}

// BusinessEnquiryInfo is what the emails about a business enquiry need.
type BusinessEnquiryInfo struct {
	Reference   string
	ProfileName string
	Service     string
	Message     string
	CreatedAt   time.Time
	Customer    Person
	// Recipients are the named agent, otherwise the owning organisation's
	// owners and managers. Empty for a routing exception.
	Recipients []Person
}

// UserInfo is what the account emails need.
type UserInfo struct {
	Person
	Roles []mail.Role
	// Administrator is true when the account holds a platform role.
	Administrator bool
}

// Source loads the data behind an event. Outbox payloads carry identifiers
// only, so personal data is read here, at send time, and nowhere else.
type Source interface {
	PropertyRequest(ctx context.Context, id uuid.UUID) (PropertyRequestInfo, error)
	BusinessEnquiry(ctx context.Context, id uuid.UUID) (BusinessEnquiryInfo, error)
	User(ctx context.Context, id uuid.UUID) (UserInfo, error)
	// Recipients resolves a routed user or organisation to the people to
	// notify: the named active user, otherwise the organisation's active
	// owners and managers.
	Recipients(ctx context.Context, userID, organisationID *uuid.UUID) ([]Person, error)
}

// NewSender builds the transport selected by the configuration and logs one
// line naming it. The API key is never logged.
func NewSender(cfg config.Config, logger *slog.Logger) (mail.Sender, error) {
	switch cfg.MailDriver {
	case config.MailDriverResend:
		sender, err := mail.NewResendSender(mail.ResendConfig{
			APIKey: cfg.ResendAPIKey.Reveal(), From: cfg.EmailFrom, ReplyTo: cfg.EmailReplyTo,
		})
		if err != nil {
			return nil, err
		}
		logger.Info("mailer selected: resend (real email will be sent through the Resend API)",
			slog.String("mail_driver", config.MailDriverResend), slog.String("email_from", cfg.EmailFrom))
		return sender, nil
	case config.MailDriverLog:
		if cfg.Env != config.EnvDevelopment {
			return nil, errors.New("the log mailer is only allowed in development")
		}
		logger.Info("mailer selected: log (development; NO email is sent, messages are written to this log)",
			slog.String("mail_driver", config.MailDriverLog), slog.Bool("resend_key_present_but_unused", cfg.ResendAPIKey.IsSet()))
		return mail.NewLogSender(logger), nil
	}
	return nil, fmt.Errorf("unknown mail driver %q", cfg.MailDriver)
}

// ---------------------------------------------------------------------------
// Request-path emails with a secret link
// ---------------------------------------------------------------------------

// AccountMailer implements identity.Mailer.
type AccountMailer struct {
	svc      *mail.Service
	renderer *mail.Renderer
	// inbox and userByEmail make an invitation to an existing account an
	// in-app notification too (see SetInbox).
	inbox       *inapp.Service
	userByEmail func(ctx context.Context, email string) (uuid.UUID, bool)
	logger      *slog.Logger
}

// NewAccountMailer builds the mailer for verification and reset emails.
func NewAccountMailer(svc *mail.Service, renderer *mail.Renderer) *AccountMailer {
	return &AccountMailer{svc: svc, renderer: renderer}
}

// send renders and delivers. The notification key is random: it identifies
// this send for retries and for the delivery record, and reveals nothing
// about the token.
func (m *AccountMailer) send(ctx context.Context, template, to string, d mail.Data) error {
	msg, err := m.renderer.Render(template, to, d)
	if err != nil {
		return err
	}
	return m.svc.Deliver(ctx, mail.Notification{Key: template + "/" + uuid.NewString(), Message: msg})
}

// SendVerification implements identity.Mailer.
func (m *AccountMailer) SendVerification(ctx context.Context, to, name, token string) error {
	return m.send(ctx, "verify-email", to, mail.Data{Name: name, Path: PathVerifyEmail, Query: url.Values{"token": {token}}})
}

// SendPasswordReset implements identity.Mailer.
func (m *AccountMailer) SendPasswordReset(ctx context.Context, to, name, token string) error {
	return m.send(ctx, "reset-password", to, mail.Data{Name: name, Path: PathResetPass, Query: url.Values{"token": {token}}})
}

// SendAccountExists implements identity.Mailer.
func (m *AccountMailer) SendAccountExists(ctx context.Context, to, name string) error {
	return m.send(ctx, "account-exists", to, mail.Data{Name: name, Path: PathSignIn})
}

// SendInvitation implements teams.Mailer: the invitation to join an
// organisation's team. The token is the secret part of the link
// (/invitations/{token}); it is only rendered into the message, and the
// notification key is random.
func (m *AccountMailer) SendInvitation(ctx context.Context, to string, inv teams.InvitationMail) error {
	if err := m.send(ctx, "team-invitation", to, mail.Data{
		Path: PathInvitation + url.PathEscape(inv.Token), OrganisationName: inv.OrganisationName, RoleName: inv.RoleLabel,
		JobTitle: inv.JobTitle, ActorName: inv.InviterName, ExpiresOn: inv.ExpiresAt,
	}); err != nil {
		return err
	}
	// The email is the invitation; the in-app item is a pointer to it, so
	// failing to create it never fails the invitation.
	if err := m.notifyInvitation(ctx, to, inv); err != nil && m.logger != nil {
		m.logger.WarnContext(ctx, "could not create the invitation's in-app notification", slog.String("error", err.Error()))
	}
	return nil
}

// SendSigning implements agreements.Mailer: a recipient's signing link
// (a request, or a reminder with a new link). The token is only rendered
// into the message.
func (m *AccountMailer) SendSigning(ctx context.Context, to string, s agreements.SigningMail) error {
	template := "agreement-signing-request"
	if s.Reminder {
		template = "agreement-reminder"
	}
	return m.send(ctx, template, to, mail.Data{Name: s.Name, Path: PathSigning + url.PathEscape(s.Token), DocumentName: s.AgreementName,
		Reference: s.Reference, ActorName: s.SenderName, RoleName: s.RoleLabel, Message: mail.SafePreview(s.Message, 400), ExpiresOn: s.ExpiresAt})
}

// SendCode implements agreements.Mailer (the code is a credential).
func (m *AccountMailer) SendCode(ctx context.Context, to string, c agreements.CodeMail) error {
	return m.send(ctx, "agreement-signing-code", to, mail.Data{Name: c.Name, Path: PathHome, Code: c.Code, Reference: c.Reference})
}

// SendCompleted implements agreements.Mailer: a recipient's read-only link,
// or the sender's agreement page.
func (m *AccountMailer) SendCompleted(ctx context.Context, to string, c agreements.CompletedMail) error {
	path := PathMyAgreements + c.AgreementID.String()
	if c.Token != "" {
		path = PathSigning + url.PathEscape(c.Token)
	}
	return m.send(ctx, "agreement-completed", to, mail.Data{Name: c.Name, Path: path, DocumentName: c.AgreementName, Reference: c.Reference,
		When: c.CompletedAt, ExpiresOn: c.AvailableUntil})
}

// SendDeclined implements agreements.Mailer.
func (m *AccountMailer) SendDeclined(ctx context.Context, to string, d agreements.DeclinedMail) error {
	return m.send(ctx, "agreement-declined", to, mail.Data{Name: d.Name, Path: PathMyAgreements + d.AgreementID.String(), DocumentName: d.AgreementName,
		Reference: d.Reference, ActorName: d.RecipientName, Reason: mail.SafePreview(d.Reason, 400)})
}

// SendSignInCode implements identity.Mailer. The code is rendered into the
// message only: the notification key is random and the delivery record holds
// no body.
func (m *AccountMailer) SendSignInCode(ctx context.Context, to, name, code string, admin bool) error {
	path := PathSignIn
	if admin {
		path = PathAdminSignIn
	}
	return m.send(ctx, "sign-in-code", to, mail.Data{Name: name, Path: path, Code: code})
}

// ---------------------------------------------------------------------------
// Outbox-driven notification emails
// ---------------------------------------------------------------------------

// Handlers sends the notification emails for outbox events.
type Handlers struct {
	svc      *mail.Service
	renderer *mail.Renderer
	source   Source
	logger   *slog.Logger
	// listing returns the title and reference of a listing, for the property
	// request emails. Nil (or not found) leaves them out.
	listing func(ctx context.Context, id uuid.UUID) (title, reference string, ok bool)
	// roleLabels returns the current reference-data labels of the account
	// roles (code -> label). Nil means the built-in labels.
	roleLabels func(ctx context.Context) (map[string]string, error)
	// onboarding loads the data behind an onboarding event.
	onboarding func(ctx context.Context, applicationID uuid.UUID) (onboarding.NotificationInfo, error)
	// appointment loads the appointment behind an appointment event.
	appointment func(ctx context.Context, id uuid.UUID) (appointments.Appointment, error)
	// listingRecord loads the listing behind a listing review event.
	listingRecord func(ctx context.Context, id uuid.UUID) (listings.Record, error)
	// suspensionReason reads the reason behind a suspension.
	suspensionReason func(ctx context.Context, userID uuid.UUID) (string, error)
	// subscription loads the subscription behind a subscription event.
	subscription func(ctx context.Context, id uuid.UUID) (subscriptions.Subscription, error)
	// application loads the application behind an application event.
	application func(ctx context.Context, id uuid.UUID) (applications.Notice, error)
	// venue loads the hourly booking behind a venue booking event.
	venue func(ctx context.Context, id uuid.UUID) (venues.BookingNotice, error)
	// maintenance loads the request behind a maintenance event.
	maintenance func(ctx context.Context, id uuid.UUID) (maintenance.NotificationInfo, error)
	// inbox is the notification centre (API 0.35.0): the in-app notification
	// of each (event, recipient), its push deliveries, and the preferences
	// the emails consult. Nil: email only, no preference check.
	inbox *inapp.Service
	// place names a listing's place (suburb, else city) for the in-app and
	// push text. Nil leaves the place out.
	place func(ctx context.Context, listingID uuid.UUID) string
}

// SetListingSource supplies the listing behind a listing review event (the
// listing store's record, whatever its status). Without it those events are
// dead-lettered.
func (h *Handlers) SetListingSource(source func(ctx context.Context, id uuid.UUID) (listings.Record, error)) {
	h.listingRecord = source
}

// listingReviewTemplates maps a review event to its template and status text.
var listingReviewTemplates = map[string]struct{ template, status string }{
	listings.EventListingApproved:         {"listing-approved", "Published"},
	listings.EventListingChangesRequested: {"listing-changes-requested", "Changes requested"},
	listings.EventListingRejected:         {"listing-rejected", "Rejected"},
}

// listingReviewNotes builds the email of a listing review decision. It goes
// to the advertiser only: the listing's assigned agent, else the
// organisation's owners and managers, or the direct owner. Nobody else.
func (h *Handlers) listingReviewNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	if h.listingRecord == nil {
		return outbox.Permanent(fmt.Errorf("notifications: no listing source for event type %q", ev.Type))
	}
	var payload listings.ReviewEventPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload: %w", ev.Type, err))
	}
	r, err := h.listingRecord(ctx, ev.AggregateID)
	if errors.Is(err, listings.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var people []Person
	switch {
	case r.OrganisationID != nil:
		people, err = h.source.Recipients(ctx, r.AgentUserID, r.OrganisationID)
	case r.SellerUserID != nil:
		people, err = h.source.Recipients(ctx, r.SellerUserID, nil)
	}
	if err != nil {
		return err
	}
	t := listingReviewTemplates[ev.Type]
	d := mail.Data{Path: PathMyListings, ListingTitle: r.Title, ListingRef: r.Reference, StatusText: t.status,
		Reason: mail.SafePreview(payload.Reason, listings.MaxReasonLength), Edit: payload.ReviewKind == listings.ReviewKindEdit, When: occurredAt(ev),
		Place: h.placeOf(ctx, &r.ID)}
	if ev.Type == listings.EventListingApproved && !d.Edit {
		d.ExpiresOn = r.ExpiresAt
	}
	for _, p := range people {
		d := d
		d.Name = p.Name
		if err := add(t.template, recipientKey(p), p, d); err != nil {
			return err
		}
	}
	return nil
}

// SetAppointmentSource supplies the appointment behind an appointment event
// (the appointment store's Get). Without it those events are dead-lettered.
func (h *Handlers) SetAppointmentSource(source func(ctx context.Context, id uuid.UUID) (appointments.Appointment, error)) {
	h.appointment = source
}

// appointmentNotes builds the emails for one appointment event: one email per
// logical transition, to the party that did NOT make it. The times come from
// the event (what was true when it happened), everything else from the
// appointment. No email carries the other party's contact details: the
// customer's are never read here, and the professional is named only by the
// public profile name.
func (h *Handlers) appointmentNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	if h.appointment == nil {
		return outbox.Permanent(fmt.Errorf("notifications: no appointment source for event type %q", ev.Type))
	}
	var payload appointments.EventPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload: %w", ev.Type, err))
	}
	a, err := h.appointment(ctx, ev.AggregateID)
	if errors.Is(err, appointments.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	start := payload.StartUTC
	if start.IsZero() {
		start = a.StartUTC
	}
	d := mail.Data{
		Reference: a.Reference, AppointmentType: mail.AppointmentTypeName(a.Type),
		ListingTitle: a.ListingTitle, ListingRef: a.ListingReference,
		AppointmentTime: mail.LocalDateTime(start, a.Timezone), StatusText: appointments.StatusLabel(payload.To),
		When: occurredAt(ev), Place: h.placeOf(ctx, a.ListingID),
	}
	if payload.ProposedStartUTC != nil {
		d.ProposedTime = mail.LocalDateTime(*payload.ProposedStartUTC, a.Timezone)
	}

	toCustomer := func(template string) error {
		d := d
		d.Name, d.Path, d.WithName = a.CustomerName, PathMyAppointments, a.WithName
		if a.CustomerUserID == nil {
			// A guest has no account page to open.
			d.Path = PathHome
		}
		return add(template, "customer", Person{UserID: a.CustomerUserID, Name: a.CustomerName, Email: a.CustomerEmail}, d)
	}
	toProfessional := func(template string) error {
		people, err := h.source.Recipients(ctx, a.AssignedUserID, a.OrganisationID)
		if err != nil {
			return err
		}
		for _, r := range people {
			d := d
			d.Name, d.Path = r.Name, PathAppointmentsInbox
			if err := add(template, recipientKey(r), r, d); err != nil {
				return err
			}
		}
		return nil
	}

	switch ev.Type {
	case appointments.EventRequested:
		return toProfessional("appointment-requested")
	case appointments.EventConfirmed:
		d.Location = mail.SafePreview(a.LocationNote, 200)
		return toCustomer("appointment-confirmed")
	case appointments.EventRescheduleRequested:
		if payload.ProposedBy == appointments.PartyCustomer {
			return toProfessional("appointment-reschedule-proposed")
		}
		return toCustomer("appointment-new-time-proposed")
	case appointments.EventCancelled:
		d.Reason = mail.SafePreview(a.CancelReason, 300)
		if payload.CancelledBy == appointments.PartyCustomer {
			return toProfessional("appointment-cancelled-by-customer")
		}
		return toCustomer("appointment-cancelled")
	}
	// Completed, no-show and assigned: recorded, nobody is emailed.
	return nil
}

// SetListingLookup makes the property request emails name the listing by its
// title and reference instead of its ID.
func (h *Handlers) SetListingLookup(lookup func(ctx context.Context, id uuid.UUID) (title, reference string, ok bool)) {
	h.listing = lookup
}

// SetRoleLabels makes the welcome email name roles exactly as the reference
// data (and so the sign-up form) does.
func (h *Handlers) SetRoleLabels(labels func(ctx context.Context) (map[string]string, error)) {
	h.roleLabels = labels
}

// labelled replaces each role's name with its reference-data label, when one exists.
func (h *Handlers) labelled(ctx context.Context, roles []mail.Role) []mail.Role {
	if h.roleLabels == nil {
		return roles
	}
	labels, err := h.roleLabels(ctx)
	if err != nil {
		h.logger.WarnContext(ctx, "could not load role labels; using the built-in ones", slog.String("error", err.Error()))
		return roles
	}
	out := make([]mail.Role, len(roles))
	for i, r := range roles {
		if label, ok := labels[r.Code]; ok {
			r.Name = label
		}
		out[i] = r
	}
	return out
}

// NewHandlers builds the outbox handlers.
func NewHandlers(svc *mail.Service, renderer *mail.Renderer, source Source, logger *slog.Logger) *Handlers {
	return &Handlers{svc: svc, renderer: renderer, source: source, logger: logger}
}

// EventTypes lists the outbox events this module handles.
func EventTypes() []string {
	return append(append([]string{
		propertyrequests.EventPropertyRequestCreated,
		propertyrequests.EventRequestAssigned,
		businessenquiries.EventBusinessEnquiryCreated,
		identity.EventUserEmailVerified,
		identity.EventPasswordChanged,
		identity.EventAdminSignedIn,
	}, onboarding.EventTypes()...), append(append(append(appointments.EventTypes(), listings.ReviewEventTypes()...),
		accounts.EventTypes()...), append(append(subscriptions.EventTypes(), applications.OutboxEventTypes()...), append(venues.EventTypes(), maintenance.OutboxEventTypes()...)...)...)...)
}

// PathOnboarding is the web page where an applicant fills in an application.
const PathOnboarding = "/onboarding"

// SetOnboardingSource supplies the data behind onboarding events. Without it
// those events are dead-lettered.
func (h *Handlers) SetOnboardingSource(source func(ctx context.Context, applicationID uuid.UUID) (onboarding.NotificationInfo, error)) {
	h.onboarding = source
}

// onboardingTemplates maps an onboarding event to its template and the page
// its button opens.
var onboardingTemplates = map[string]struct{ template, path, status string }{
	onboarding.EventSubmitted:        {"onboarding-submitted", PathOnboarding, onboarding.StatusSubmitted},
	onboarding.EventApproved:         {"onboarding-approved", PathAccount, onboarding.StatusApproved},
	onboarding.EventChangesRequested: {"onboarding-changes-requested", PathOnboarding, onboarding.StatusChangesRequested},
	onboarding.EventRejected:         {"onboarding-rejected", PathAccount, onboarding.StatusRejected},
}

// Register attaches the handlers to a dispatcher.
func (h *Handlers) Register(d *outbox.Dispatcher) {
	for _, t := range EventTypes() {
		d.Register(t, outbox.HandlerFunc(h.Handle))
	}
}

// Handle sends every email an event calls for. It is idempotent: running it
// again for the same event sends nothing that the provider already accepted.
//
// A transient failure is returned as is, so the outbox retries the event with
// backoff. When every remaining failure is terminal (the provider rejected
// the message), the error is marked permanent and the event is dead-lettered.
func (h *Handlers) Handle(ctx context.Context, ev outbox.Event) error {
	plans, err := h.build(ctx, ev)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return outbox.Permanent(fmt.Errorf("event %s: %w", ev.Type, err))
		}
		return err
	}
	// The in-app notifications first: they are the durable record, and a
	// failure here is retried with the event (the unique key makes the
	// retry create nothing twice).
	if err := h.notifyInApp(ctx, ev, plans); err != nil {
		return err
	}
	var transient, terminal error
	for _, p := range plans {
		if p.to.Email == "" {
			continue // nobody to write to
		}
		allowed, err := h.emailAllowed(ctx, p)
		if err != nil {
			return err
		}
		if !allowed {
			continue // the recipient switched this type's email off (FR-003, FR-044)
		}
		n, err := h.note(ev, p.template, p.recipient, p.to, p.data)
		if err != nil {
			return err
		}
		err = h.svc.Deliver(ctx, n)
		h.recordEmail(ctx, ev, p, err)
		switch {
		case err == nil:
		case errors.Is(err, mail.ErrTerminal):
			terminal = err
		default:
			transient = err
		}
	}
	switch {
	case transient != nil:
		return transient
	case terminal != nil:
		return outbox.Permanent(terminal)
	}
	return nil
}

// key builds the notification key: one per (template, event, recipient).
func key(template string, ev outbox.Event, recipient string) string {
	return template + "/" + ev.ID.String() + "/" + recipient
}

func recipientKey(p Person) string {
	if p.UserID != nil {
		return p.UserID.String()
	}
	return mail.RecipientHash(p.Email)[:16]
}

func (h *Handlers) note(ev outbox.Event, template, recipient string, to Person, d mail.Data) (mail.Notification, error) {
	msg, err := h.renderer.Render(template, to.Email, d)
	if err != nil {
		return mail.Notification{}, err
	}
	return mail.Notification{Key: key(template, ev, recipient), RecipientUserID: to.UserID, Message: msg}, nil
}

func occurredAt(ev outbox.Event) time.Time {
	var payload struct {
		OccurredAt time.Time `json:"occurred_at"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err == nil && !payload.OccurredAt.IsZero() {
		return payload.OccurredAt
	}
	return ev.CreatedAt
}

// plan is one message an event calls for: an email (when the person has an
// address and allows it) and, for a known account, an in-app notification.
type plan struct {
	template, recipient string
	to                  Person
	data                mail.Data
}

func (h *Handlers) build(ctx context.Context, ev outbox.Event) ([]plan, error) {
	var notes []plan
	add := func(template, recipient string, to Person, d mail.Data) error {
		if to.Email == "" {
			return nil // nobody to write to
		}
		notes = append(notes, plan{template: template, recipient: recipient, to: to, data: d})
		return nil
	}

	switch ev.Type {
	case propertyrequests.EventPropertyRequestCreated:
		info, err := h.source.PropertyRequest(ctx, ev.AggregateID)
		if err != nil {
			return nil, err
		}
		requestType := mail.RequestTypeName(info.RequestType)
		// A request to stay says which nights and how many guests.
		stay := ""
		if info.Stay != nil {
			stay = mail.StayText(info.Stay.CheckIn, info.Stay.CheckOut, info.Stay.Nights, info.Stay.Guests)
		}
		var listingTitle, listingRef string
		if h.listing != nil {
			listingTitle, listingRef, _ = h.listing(ctx, info.ListingID)
		}
		if err := add("property-request-received", "customer", info.Customer, mail.Data{
			Name: info.Customer.Name, Path: PathHome, Reference: info.Reference, RequestType: requestType, When: info.CreatedAt,
			ListingTitle: listingTitle, ListingRef: listingRef, Stay: stay,
		}); err != nil {
			return nil, err
		}
		for _, r := range info.Recipients {
			if err := add("property-request-new", recipientKey(r), r, mail.Data{
				Name: r.Name, Path: PathInbox, Reference: info.Reference, RequestType: requestType, Place: h.placeOf(ctx, &info.ListingID),
				ListingTitle: listingTitle, ListingRef: listingRef, Stay: stay,
				Preview: mail.SafePreview(info.Message, previewLength), When: info.CreatedAt,
			}); err != nil {
				return nil, err
			}
		}

	case propertyrequests.EventRequestAssigned:
		// To the person the request was given to only.
		var payload struct {
			Assignee *uuid.UUID `json:"assignee_user_id"`
			Actor    *uuid.UUID `json:"actor_user_id"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil || payload.Assignee == nil {
			return nil, outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload", ev.Type))
		}
		info, err := h.source.PropertyRequest(ctx, ev.AggregateID)
		if err != nil {
			return nil, err
		}
		people, err := h.source.Recipients(ctx, payload.Assignee, nil)
		if err != nil {
			return nil, err
		}
		actorName := ""
		if payload.Actor != nil {
			if actor, err := h.source.User(ctx, *payload.Actor); err == nil {
				actorName = actor.Name
			}
		}
		var listingTitle, listingRef string
		if h.listing != nil {
			listingTitle, listingRef, _ = h.listing(ctx, info.ListingID)
		}
		for _, r := range people {
			if err := add("property-request-assigned", recipientKey(r), r, mail.Data{
				Name: r.Name, Path: PathInbox, Reference: info.Reference, RequestType: mail.RequestTypeName(info.RequestType),
				Place:        h.placeOf(ctx, &info.ListingID),
				ListingTitle: listingTitle, ListingRef: listingRef, ActorName: actorName,
				Preview: mail.SafePreview(info.Message, previewLength), When: occurredAt(ev),
			}); err != nil {
				return nil, err
			}
		}

	case businessenquiries.EventBusinessEnquiryCreated:
		info, err := h.source.BusinessEnquiry(ctx, ev.AggregateID)
		if err != nil {
			return nil, err
		}
		if err := add("business-enquiry-received", "customer", info.Customer, mail.Data{
			Name: info.Customer.Name, Path: PathHome, Reference: info.Reference, ProfileName: info.ProfileName,
			Service: info.Service, When: info.CreatedAt,
		}); err != nil {
			return nil, err
		}
		for _, r := range info.Recipients {
			if err := add("business-enquiry-new", recipientKey(r), r, mail.Data{
				Name: r.Name, Path: PathInbox, Reference: info.Reference, ProfileName: info.ProfileName,
				Service: info.Service, Preview: mail.SafePreview(info.Message, previewLength), When: info.CreatedAt,
			}); err != nil {
				return nil, err
			}
		}

	case identity.EventUserEmailVerified, identity.EventPasswordChanged, identity.EventAdminSignedIn:
		user, err := h.source.User(ctx, ev.AggregateID)
		if err != nil {
			return nil, err
		}
		signIn := PathSignIn
		if user.Administrator {
			signIn = PathAdminSignIn
		}
		var template string
		d := mail.Data{Name: user.Name, When: occurredAt(ev)}
		switch ev.Type {
		case identity.EventUserEmailVerified:
			template, d.Path, d.Roles = "welcome", PathAccount, h.labelled(ctx, user.Roles)
		case identity.EventPasswordChanged:
			template, d.Path = "password-changed", signIn
		case identity.EventAdminSignedIn:
			template, d.Path = "admin-sign-in", PathAdminSignIn
		}
		if err := add(template, "account", user.Person, d); err != nil {
			return nil, err
		}

	case onboarding.EventSubmitted, onboarding.EventApproved, onboarding.EventChangesRequested, onboarding.EventRejected:
		if h.onboarding == nil {
			return nil, outbox.Permanent(fmt.Errorf("notifications: no onboarding source for event type %q", ev.Type))
		}
		info, err := h.onboarding(ctx, ev.AggregateID)
		if errors.Is(err, onboarding.ErrNotFound) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		// The applicant only: no document names, no private details, and the
		// reason only in the two emails that exist to pass it on.
		t := onboardingTemplates[ev.Type]
		d := mail.Data{Name: info.Name, Path: t.path, RoleName: info.RoleLabel, StatusText: onboarding.StatusLabel(t.status), When: occurredAt(ev)}
		switch ev.Type {
		case onboarding.EventChangesRequested, onboarding.EventRejected:
			d.Reason = info.Reason
		case onboarding.EventApproved:
			if info.Profile != nil {
				d.Path, d.ProfileName = "/directory/"+info.Profile.Kind+"/"+info.Profile.Slug, info.ProfileName
			}
		}
		user := info.UserID
		if err := add(t.template, "applicant", Person{UserID: &user, Name: info.Name, Email: info.Email}, d); err != nil {
			return nil, err
		}

	case appointments.EventRequested, appointments.EventConfirmed, appointments.EventRescheduleRequested,
		appointments.EventCancelled, appointments.EventCompleted, appointments.EventNoShow, appointments.EventAssigned:
		if err := h.appointmentNotes(ctx, ev, add); err != nil {
			return nil, err
		}

	case listings.EventListingApproved, listings.EventListingChangesRequested, listings.EventListingRejected:
		if err := h.listingReviewNotes(ctx, ev, add); err != nil {
			return nil, err
		}

	case accounts.EventAccountSuspended, accounts.EventAccountReactivated:
		if err := h.accountNotes(ctx, ev, add); err != nil {
			return nil, err
		}

	case subscriptions.EventRequested, subscriptions.EventActivated, subscriptions.EventRejected, subscriptions.EventCancelled, subscriptions.EventExpired:
		if err := h.subscriptionNotes(ctx, ev, add); err != nil {
			return nil, err
		}

	case applications.EventTypeSubmitted, applications.EventTypeInfoRequested, applications.EventTypeStatusChanged, applications.EventTypeWithdrawn:
		if err := h.applicationNotes(ctx, ev, add); err != nil {
			return nil, err
		}

	case venues.EventRequested, venues.EventConfirmed, venues.EventDeclined, venues.EventCancelled, venues.EventRescheduleRequested,
		venues.EventProposed, venues.EventCompleted, venues.EventNoShow:
		if err := h.venueNotes(ctx, ev, add); err != nil {
			return nil, err
		}

	case maintenance.OutboxRequestCreated, maintenance.OutboxStatusChanged:
		if err := h.maintenanceNotes(ctx, ev, add); err != nil {
			return nil, err
		}

	default:
		return nil, outbox.Permanent(fmt.Errorf("notifications: no handler for event type %q", ev.Type))
	}
	return notes, nil
}
