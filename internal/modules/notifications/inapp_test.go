package notifications

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/accounts"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/inapp"
	"rightproperty365.com/backend/internal/modules/teams"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// withInbox connects a memory notification centre (push queued as with a
// pusher) and a place lookup to the fixture's handlers.
func (f *fixture) withInbox() (*inapp.MemoryStore, *inapp.Service) {
	f.t.Helper()
	store := inapp.NewMemoryStore()
	svc := inapp.NewService(store, "log", slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	f.handlers.SetInbox(svc)
	f.handlers.SetPlaceLookup(func(context.Context, uuid.UUID) string { return "Borrowdale" })
	return store, svc
}

func agentID(t *testing.T, f *fixture) uuid.UUID {
	t.Helper()
	u, err := f.identity.UserByEmail(context.Background(), identity.SeedAgentEmail)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func notificationsOf(t *testing.T, svc *inapp.Service, user uuid.UUID) []inapp.Notification {
	t.Helper()
	page, err := svc.List(context.Background(), user, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// FR-062 acceptance: the receiving agent gets ONE in-app notification (and
// one push per opted-in device) however often the worker runs the event.
func TestOneInAppNotificationPerEventAndRecipientUnderRedelivery(t *testing.T) {
	f := newFixture(t)
	store, svc := f.withInbox()
	agent := agentID(t, f)
	consent := true
	if _, _, err := svc.RegisterDevice(context.Background(), agent, nil, inapp.DeviceInput{Token: "tok-agent-phone", Platform: inapp.PlatformAndroid,
		AppVersion: "1.0.0", PushConsent: &consent}); err != nil {
		t.Fatal(err)
	}
	ev := f.propertyRequest(1, nil)
	for run := range 3 {
		if err := f.handle(ev); err != nil {
			t.Fatalf("run %d: %v", run+1, err)
		}
	}
	// And through the dispatcher, the queue handing the event out twice.
	queue := &twiceQueue{event: ev}
	d := outbox.NewDispatcher(queue, slog.New(slog.NewJSONHandler(io.Discard, nil)), outbox.Config{})
	f.handlers.Register(d)
	for range 2 {
		if _, err := d.ProcessBatch(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	items := notificationsOf(t, svc, agent)
	if len(items) != 1 {
		t.Fatalf("%d notifications for the agent, want 1", len(items))
	}
	n := items[0]
	if n.Type != inapp.TypeNewLead || n.Title != "New property enquiry" || !strings.HasPrefix(n.Link, "/agent/requests/"+ev.AggregateID.String()) ||
		n.Reference == "" || n.Body != n.Reference+" · Borrowdale" {
		t.Fatalf("notification = %+v", n)
	}
	pushes := 0
	for _, r := range store.Deliveries(n.ID) {
		if r.Channel == inapp.ChannelPush {
			pushes++
		}
	}
	if pushes != 1 {
		t.Fatalf("%d push deliveries, want 1", pushes)
	}
	// The email delivery is recorded on the same notification.
	var email *inapp.DeliveryRecord
	for _, r := range store.Deliveries(n.ID) {
		if r.Channel == inapp.ChannelEmail {
			r := r
			email = &r
		}
	}
	if email == nil || email.Status != inapp.StatusProviderAccepted {
		t.Fatalf("email delivery record = %+v", email)
	}
	// The guest customer has no inbox; their receipt email is still sent once.
	if mails := f.sender.byKind("property-request-received"); len(mails) != 1 {
		t.Fatalf("customer receipts: %d", len(mails))
	}
}

// FR-003 / FR-044: a non-essential type's email follows the preference
// from the next send; the in-app item is always created; an essential
// type's email cannot be switched off.
func TestEmailPathConsultsPreferences(t *testing.T) {
	f := newFixture(t)
	_, svc := f.withInbox()
	agent := agentID(t, f)
	off := false
	if _, err := svc.SetPreferences(context.Background(), agent, []inapp.PreferenceItem{{Type: inapp.TypeNewLead, Email: &off}}, inapp.SourceWeb); err != nil {
		t.Fatal(err)
	}
	if err := f.handle(f.propertyRequest(1, nil)); err != nil {
		t.Fatal(err)
	}
	if mails := f.sender.byKind("property-request-new"); len(mails) != 0 {
		t.Fatalf("the agent switched NEW_LEAD email off but got %d", len(mails))
	}
	if mails := f.sender.byKind("property-request-received"); len(mails) != 1 {
		t.Fatalf("the customer's receipt is not the agent's preference: %d", len(mails))
	}
	if items := notificationsOf(t, svc, agent); len(items) != 1 {
		t.Fatalf("in-app is always on: %d", len(items))
	}

	// Essential: refused, and the email still goes.
	_, err := svc.SetPreferences(context.Background(), agent, []inapp.PreferenceItem{{Type: inapp.TypeAccount, Email: &off}}, inapp.SourceWeb)
	var v *inapp.ValidationError
	if !errors.As(err, &v) || !strings.HasPrefix(v.Fields["items[0].email"], "PREFERENCE_ESSENTIAL") {
		t.Fatalf("essential switch-off: %v", err)
	}
	f.handlers.SetSuspensionReasons(func(context.Context, uuid.UUID) (string, error) { return "Reported", nil })
	if err := f.handle(userEvent(accounts.EventAccountSuspended, agent)); err != nil {
		t.Fatal(err)
	}
	if mails := f.sender.byKind("account-suspended"); len(mails) != 1 {
		t.Fatalf("essential email: %d", len(mails))
	}

	// Switched back on: the next send goes out.
	on := true
	if _, err := svc.SetPreferences(context.Background(), agent, []inapp.PreferenceItem{{Type: inapp.TypeNewLead, Email: &on}}, inapp.SourceApp); err != nil {
		t.Fatal(err)
	}
	if err := f.handle(f.propertyRequest(1, nil)); err != nil {
		t.Fatal(err)
	}
	if mails := f.sender.byKind("property-request-new"); len(mails) != 1 {
		t.Fatalf("after switching back on: %d", len(mails))
	}
}

// seededPersonalData is every seeded person's name and email, and the
// sample customers' contact details.
func seededPersonalData() []string {
	out := []string{"Tariro Moyo", "tariro@example.com", "+263771234567", "+263 77 123 4567", "123 4567", "tapiwa.chirwa@example.com",
		"+263712345678", "t.marufu@example.com", "+263773555444", "+263770000001", "+263770000002"}
	for _, a := range devseed.Accounts() {
		out = append(out, a.Name, a.Email)
	}
	return out
}

// Lock-screen safety: every template's in-app text, built from data in
// which every personal field holds a seeded name, email or phone, contains
// none of them. Then the same end to end for a request and an enquiry.
func TestInAppTextHasNoPersonalData(t *testing.T) {
	people := devseed.Accounts()
	pii := seededPersonalData()
	ev := outbox.Event{ID: uuid.New(), AggregateID: uuid.New()}
	for template := range inAppTemplates {
		for i, p := range people {
			d := mail.Data{
				Name: p.Name, ActorName: people[(i+1)%len(people)].Name, WithName: p.Name, ProfileName: p.Name,
				Preview: customerMessage, Message: customerMessage + " " + p.Email, Reason: "Call " + p.Name + " on +263771234567",
				Location: "Ask for " + p.Name, OrganisationName: p.Name, JobTitle: p.Name, DocumentName: p.Name,
				ListingTitle: "House of " + p.Name + " " + p.Email,
				Reference:    "PR-ZW-10001", ListingRef: "RP-ZW-10001", Place: "Borrowdale", InvoiceNumber: "INV-2026-0001", PlanName: "Agency Pro",
				StatusText: "Shortlisted", AppointmentType: "Viewing", RequestType: "Viewing request",
			}
			draft, ok := InAppDraft(ev, template, d)
			if !ok {
				t.Fatalf("%s: no draft", template)
			}
			text := strings.Join([]string{draft.Title, draft.Body, draft.Link, draft.Reference}, "\n")
			for _, s := range pii {
				if s != "" && strings.Contains(text, s) {
					t.Fatalf("%s: in-app text contains %q:\n%s", template, s, text)
				}
			}
			if draft.Title == "" || !strings.HasPrefix(draft.Link, "/") {
				t.Fatalf("%s: draft %+v", template, draft)
			}
		}
	}

	// End to end: a request and an enquiry with the sample contact details.
	f := newFixture(t)
	_, svc := f.withInbox()
	for _, ev := range []outbox.Event{f.propertyRequest(1, nil), f.enquiry("service-providers", "sample-plumbers"), f.enquiry("agents", "tendai-ncube")} {
		if err := f.handle(ev); err != nil {
			t.Fatal(err)
		}
	}
	found := 0
	for _, a := range people {
		for _, n := range notificationsOf(t, svc, a.ID) {
			found++
			text := n.Title + "\n" + n.Body + "\n" + n.Link + "\n" + n.Reference
			for _, s := range pii {
				if strings.Contains(text, s) {
					t.Fatalf("notification %s contains %q: %s", n.Type, s, text)
				}
			}
		}
	}
	if found < 3 {
		t.Fatalf("%d notifications created, want at least 3", found)
	}
}

// Every type of the preference catalogue (but the summary) has a source.
func TestEveryTypeHasASource(t *testing.T) {
	sources := map[string]bool{inapp.TypeTeamInvitation: true}
	for _, tpl := range inAppTemplates {
		sources[tpl.typ] = true
	}
	for _, c := range inapp.Catalogue() {
		if !sources[c.Type] {
			t.Fatalf("type %s has no event mapped to it", c.Type)
		}
	}
}

// An invitation to an existing account is also an in-app notification,
// without the invitation token.
func TestInvitationInApp(t *testing.T) {
	f := newFixture(t)
	_, svc := f.withInbox()
	agent := agentID(t, f)
	f.mailer.SetInbox(svc, func(context.Context, string) (uuid.UUID, bool) { return agent, true }, nil)
	if err := f.mailer.SendInvitation(context.Background(), identity.SeedAgentEmail, teams.InvitationMail{Token: "secret-invitation-token",
		OrganisationName: "Harare Homes", RoleLabel: "Agent", InviterName: "Blake Owner", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	items := notificationsOf(t, svc, agent)
	if len(items) != 1 || items[0].Type != inapp.TypeTeamInvitation || items[0].Link != "/account" ||
		strings.Contains(items[0].Body+items[0].Link, "secret-invitation-token") || strings.Contains(items[0].Body, "Blake") {
		t.Fatalf("invitation notification = %+v", items)
	}
}
