package notifications

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

const (
	publicURL    = "https://rightproperty365.com"
	seedPassword = "seed-password-for-tests"
	// A made-up key: never sent anywhere.
	fakeKey = "test-key-FAKE-not-real-0123456789"
)

// recordingSender captures what would be sent and can be told to fail.
type recordingSender struct {
	mu   sync.Mutex
	sent []sentMail
	// fail returns the error for a message, or nil to accept it.
	fail func(m mail.Message, attempt int) error
	seen map[string]int
}

type sentMail struct {
	Key string
	mail.Message
}

func (s *recordingSender) Name() string { return "fake" }

func (s *recordingSender) Send(_ context.Context, key string, m mail.Message) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]int{}
	}
	s.seen[key]++
	if s.fail != nil {
		if err := s.fail(m, s.seen[key]); err != nil {
			return "", err
		}
	}
	s.sent = append(s.sent, sentMail{Key: key, Message: m})
	return "provider-" + uuid.NewString(), nil
}

func (s *recordingSender) byKind(kind string) []sentMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sentMail
	for _, m := range s.sent {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

type fixture struct {
	t         *testing.T
	identity  *identity.MemoryStore
	requests  *propertyrequests.MemoryStore
	enquiries *businessenquiries.MemoryStore
	sender    *recordingSender
	log       *mail.MemoryDeliveryLog
	handlers  *Handlers
	mailer    *AccountMailer
	logs      *bytes.Buffer
	events    []outbox.Event
	seq       int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t: t, identity: identity.NewMemoryStore(), requests: propertyrequests.NewMemoryStore(),
		enquiries: businessenquiries.NewMemoryStore(), sender: &recordingSender{}, log: mail.NewMemoryDeliveryLog(), logs: &bytes.Buffer{},
	}
	devseed.SeedMemoryListings(f.requests)
	devseed.SeedMemoryProfiles(f.enquiries)
	now := time.Date(2026, 11, 14, 9, 5, 0, 0, time.UTC)
	if err := devseed.SeedMemoryUsers(context.Background(), f.identity, identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1}, seedPassword, now); err != nil {
		t.Fatal(err)
	}
	// Events are collected instead of handled, so each test decides when (and
	// how many times) the "worker" runs.
	sink := func(ev outbox.Event) { f.events = append(f.events, ev) }
	f.identity.SetOutboxSink(sink)
	f.requests.SetOutboxSink(sink)
	f.enquiries.SetOutboxSink(sink)

	logger := slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	renderer, err := mail.NewRenderer(publicURL)
	if err != nil {
		t.Fatal(err)
	}
	svc := mail.NewService(f.sender, f.log, logger)
	f.handlers = NewHandlers(svc, renderer, NewMemorySource(f.identity, f.requests, f.enquiries), logger)
	// The property request emails name the listing by title and reference.
	catalogue := listings.NewMemoryStore(noLabels{})
	devseed.SeedMemoryContent(catalogue, directory.NewMemoryStore(), now)
	f.handlers.SetListingLookup(func(ctx context.Context, id uuid.UUID) (string, string, bool) {
		records, err := catalogue.ByIDs(ctx, []uuid.UUID{id})
		if err != nil || len(records) == 0 {
			return "", "", false
		}
		return records[0].Title, records[0].Reference, true
	})
	f.mailer = NewAccountMailer(svc, renderer)
	return f
}

const customerMessage = "Is this house still available? Call me on +263 77 123 4567 or write to tariro@example.com. Budget USD 250,000."

func (f *fixture) propertyRequest(listing int, customer *propertyrequests.Customer) outbox.Event {
	f.t.Helper()
	f.seq++
	in := propertyrequests.Input{
		ListingID: propertyrequests.SeedListingID(listing).String(), RequestType: propertyrequests.TypeViewingRequest,
		Message: customerMessage, PreferredContactMethod: propertyrequests.ContactPhone,
		IdempotencyKey: fmt.Sprintf("notify-pr-%04d", f.seq),
		GuestContact:   &propertyrequests.GuestContact{Name: "Tariro Moyo", Email: "tariro@example.com", Phone: "+263771234567"},
		Consent:        &propertyrequests.Consent{PrivacyVersion: "2026-01", Accepted: true},
		Customer:       customer,
	}
	before := len(f.events)
	if _, err := propertyrequests.NewService(f.requests).Create(context.Background(), in, ""); err != nil {
		f.t.Fatal(err)
	}
	if len(f.events) != before+1 {
		f.t.Fatalf("expected one outbox event, got %d", len(f.events)-before)
	}
	return f.events[len(f.events)-1]
}

func (f *fixture) enquiry(kind, slug string) outbox.Event {
	f.t.Helper()
	f.seq++
	service := "Burst pipes and leaks"
	in := businessenquiries.Input{
		Recipient: &businessenquiries.RecipientRef{Kind: kind, Slug: slug}, Service: &service,
		Message: customerMessage, PreferredContactMethod: businessenquiries.ContactPhone,
		IdempotencyKey: fmt.Sprintf("notify-be-%04d", f.seq),
		GuestContact:   &businessenquiries.GuestContact{Name: "Tariro Moyo", Email: "tariro@example.com", Phone: "+263771234567"},
		Consent:        &businessenquiries.Consent{PrivacyVersion: "2026-01", Accepted: true},
	}
	before := len(f.events)
	if _, err := businessenquiries.NewService(f.enquiries).Create(context.Background(), in, ""); err != nil {
		f.t.Fatal(err)
	}
	if len(f.events) != before+1 {
		f.t.Fatalf("expected one outbox event, got %d", len(f.events)-before)
	}
	return f.events[len(f.events)-1]
}

func (f *fixture) handle(ev outbox.Event) error {
	return f.handlers.Handle(context.Background(), ev)
}

func recipients(mails []sentMail) string {
	var to []string
	for _, m := range mails {
		to = append(to, m.To)
	}
	return strings.Join(to, ",")
}

// One logical send per notification, even when the worker runs the same
// event twice (at-least-once delivery).
func TestOneSendDespiteADuplicateWorkerRun(t *testing.T) {
	f := newFixture(t)
	ev := f.propertyRequest(1, nil)

	for run := range 3 {
		if err := f.handle(ev); err != nil {
			t.Fatalf("run %d: %v", run+1, err)
		}
	}
	if f.sender.count() != 2 {
		t.Fatalf("%d emails sent, want 2 (one to the customer, one to the agent)", f.sender.count())
	}
	if got := recipients(f.sender.byKind("property-request-received")); got != "tariro@example.com" {
		t.Fatalf("customer email went to %q", got)
	}
	if got := recipients(f.sender.byKind("property-request-new")); got != identity.SeedAgentEmail {
		t.Fatalf("recipient email went to %q, want the listing's assigned agent", got)
	}

	recs := f.log.Records()
	if len(recs) != 2 {
		t.Fatalf("%d delivery records, want 2", len(recs))
	}
	for _, r := range recs {
		if r.Status != mail.StatusProviderAccepted || r.Attempts != 1 || r.ProviderMessageID == "" || r.Provider != "fake" {
			t.Fatalf("unexpected record: %+v", r)
		}
		if !strings.Contains(r.Key, ev.ID.String()) || len(r.Key) > mail.MaxKeyLength {
			t.Fatalf("the notification key must be derived from the event: %q", r.Key)
		}
	}
	// The provider idempotency key is the notification key.
	for _, m := range f.sender.sent {
		if !strings.HasPrefix(m.Key, m.Kind+"/"+ev.ID.String()+"/") {
			t.Fatalf("idempotency key = %q", m.Key)
		}
	}

	// The same thing through the real dispatcher: the queue hands the event
	// out twice (a lease that expired before it was marked processed).
	ev2 := f.propertyRequest(2, nil)
	queue := &twiceQueue{event: ev2}
	d := outbox.NewDispatcher(queue, slog.New(slog.NewJSONHandler(io.Discard, nil)), outbox.Config{})
	f.handlers.Register(d)
	for range 2 {
		if _, err := d.ProcessBatch(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if queue.processed != 2 || f.sender.count() != 4 {
		t.Fatalf("processed=%d emails=%d, want the event handled twice but only 2 more emails", queue.processed, f.sender.count())
	}
}

// twiceQueue leases the same event on every poll.
type twiceQueue struct {
	event     outbox.Event
	processed int
	failed    int
}

func (q *twiceQueue) Lease(context.Context, string, int, time.Duration) ([]outbox.Event, error) {
	return []outbox.Event{q.event}, nil
}
func (q *twiceQueue) MarkProcessed(context.Context, uuid.UUID, string) error {
	q.processed++
	return nil
}
func (q *twiceQueue) MarkRetry(context.Context, uuid.UUID, string, string, time.Time) error {
	q.failed++
	return nil
}
func (q *twiceQueue) MarkDeadLetter(context.Context, uuid.UUID, string, string) error {
	q.failed++
	return nil
}

// Emails to agents, sellers and owners carry a reference, a safe preview and
// a button to the inbox, never the customer's phone number or email address.
func TestRecipientEmailsHoldNoCustomerContactDetails(t *testing.T) {
	f := newFixture(t)
	if err := f.handle(f.propertyRequest(1, nil)); err != nil {
		t.Fatal(err)
	}
	if err := f.handle(f.enquiry("service-providers", "sample-plumbers")); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{"property-request-new", "business-enquiry-new"} {
		mails := f.sender.byKind(kind)
		if len(mails) != 1 {
			t.Fatalf("%s: %d emails", kind, len(mails))
		}
		m := mails[0]
		for _, part := range []string{m.HTML, m.Text, m.Subject} {
			for _, private := range []string{"tariro@example.com", "263771234567", "77 123 4567", "+263", "Tariro", "Moyo"} {
				if strings.Contains(part, private) {
					t.Fatalf("%s contains the customer's contact detail %q", kind, private)
				}
			}
		}
		if !strings.Contains(m.Text, "Is this house still available? Call me on [number hidden] or write to [email hidden]") {
			t.Fatalf("%s: the preview must be present with contact details removed:\n%s", kind, m.Text)
		}
		if !strings.Contains(m.Text, publicURL+"/agent/requests") || m.DevLink != publicURL+"/agent/requests" {
			t.Fatalf("%s: the button must open the inbox:\n%s", kind, m.Text)
		}
		if !strings.Contains(m.Text, "contact details are in your inbox") {
			t.Fatalf("%s must say where the contact details are", kind)
		}
	}
	pr := f.sender.byKind("property-request-new")[0]
	if !strings.HasPrefix(pr.Subject, "New property request: RP-") || !strings.Contains(pr.Text, "Property: Four-bedroom family home with garden\nListing reference: RP-ZW-10001") ||
		strings.Contains(pr.Text, propertyrequests.SeedListingID(1).String()) ||
		!strings.Contains(pr.Text, "Request: Viewing request") || !strings.Contains(pr.Text, "Hello Tendai Ncube,") {
		t.Fatalf("unexpected agent email:\n%s\n%s", pr.Subject, pr.Text)
	}
	be := f.sender.byKind("business-enquiry-new")[0]
	if !strings.HasPrefix(be.Subject, "New enquiry: BE-") || be.To != "sample-plumbers@example.test" || !strings.Contains(be.Text, "Profile: Sample Plumbers") {
		t.Fatalf("unexpected owner email:\n%s\n%s", be.Subject, be.Text)
	}

	// Emails to the customer: their reference, no contact details of the business.
	for _, kind := range []string{"property-request-received", "business-enquiry-received"} {
		m := f.sender.byKind(kind)[0]
		if kind == "property-request-received" && !strings.Contains(m.Text, "Property: Four-bedroom family home with garden\nListing reference: RP-ZW-10001") {
			t.Fatalf("the customer's email must name the property:\n%s", m.Text)
		}
		if m.To != "tariro@example.com" || !strings.Contains(m.Text, "Hello Tariro Moyo,") || !strings.Contains(m.Text, "Reference: ") {
			t.Fatalf("unexpected customer email:\n%s", m.Text)
		}
		for _, private := range []string{"@example.test", "enquiries@", identity.SeedAgentEmail, "Tendai", "/agent/requests"} {
			if strings.Contains(m.Text, private) || strings.Contains(m.HTML, private) {
				t.Fatalf("%s to the customer contains %q", kind, private)
			}
		}
		if strings.Contains(strings.ToLower(m.Text), "delivered") || !strings.Contains(m.Text, "received") {
			t.Fatalf("%s must say \"received\" and never \"delivered\":\n%s", kind, m.Text)
		}
	}
	// Nothing personal is logged either.
	for _, private := range []string{"tariro@example.com", "263771234567", "Is this house"} {
		if strings.Contains(f.logs.String(), private) {
			t.Fatalf("logs contain %q", private)
		}
	}
}

func TestWhoIsNotified(t *testing.T) {
	f := newFixture(t)

	// A second owner and a manager for Sample Estates, plus staff who must not be notified.
	addMember := func(email, role string) {
		id, now := uuid.New(), time.Now()
		if err := f.identity.CreateUser(context.Background(), identity.NewUser{ID: id, Name: email, Email: email, PasswordHash: "x", EmailVerifiedAt: &now, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := f.identity.AddMembership(id, devseed.SampleEstates.ID, "ZW", role); err != nil {
			t.Fatal(err)
		}
	}
	addMember("estates-manager@example.com", auth.RoleManager)
	addMember("estates-staff@example.com", auth.RoleStaff)

	tests := []struct {
		name     string
		event    func() outbox.Event
		kind     string
		wantTo   string
		wantMail int // total emails for the event, including the customer's
	}{
		{"listing with an assigned agent: that agent only", func() outbox.Event { return f.propertyRequest(7, nil) }, "property-request-new", devseed.EstatesAgent2Email, 2},
		{"direct seller", func() outbox.Event { return f.propertyRequest(6, nil) }, "property-request-new", devseed.SellerEmail, 2},
		{"agent profile: the named agent only", func() outbox.Event { return f.enquiry("agents", "tendai-ncube") }, "business-enquiry-new", identity.SeedAgentEmail, 2},
		{"agency profile: its owners and managers, not agents or staff", func() outbox.Event { return f.enquiry("agencies", "sample-estates") }, "business-enquiry-new",
			// The seeded agency administrator (a MANAGER) is notified too.
			devseed.AgencyAdminEmail + ",estates-manager@example.com," + devseed.EstatesOwnerEmail, 4},
		{"business profile: its owner", func() outbox.Event { return f.enquiry("hosts", "sample-showrooms") }, "business-enquiry-new", "sample-showrooms@example.test", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := f.sender.count()
			kindBefore := len(f.sender.byKind(tt.kind))
			if err := f.handle(tt.event()); err != nil {
				t.Fatal(err)
			}
			if got := recipients(f.sender.byKind(tt.kind)[kindBefore:]); got != tt.wantTo {
				t.Fatalf("notified %q, want %q", got, tt.wantTo)
			}
			if got := f.sender.count() - before; got != tt.wantMail {
				t.Fatalf("%d emails, want %d", got, tt.wantMail)
			}
		})
	}

	t.Run("routing exception: only the customer is written to", func(t *testing.T) {
		p, err := f.enquiries.GetProfile(context.Background(), "hosts", "sample-showrooms")
		if err != nil {
			t.Fatal(err)
		}
		p.ID, p.Slug, p.OrganisationEligible = uuid.New(), "orphaned-host", false
		f.enquiries.PutProfile(p)
		before := f.sender.count()
		if err := f.handle(f.enquiry("hosts", "orphaned-host")); err != nil {
			t.Fatal(err)
		}
		if f.sender.count() != before+1 || f.sender.sent[len(f.sender.sent)-1].Kind != "business-enquiry-received" {
			t.Fatalf("emails after a routing exception: %d", f.sender.count()-before)
		}
	})

	t.Run("a signed-in customer is written to at their account address", func(t *testing.T) {
		before := len(f.sender.byKind("property-request-received"))
		ev := f.propertyRequest(1, &propertyrequests.Customer{UserID: identity.SeedConsumerUserID, Name: "Casey Consumer", Email: identity.SeedConsumerEmail})
		if err := f.handle(ev); err != nil {
			t.Fatal(err)
		}
		m := f.sender.byKind("property-request-received")[before]
		if m.To != identity.SeedConsumerEmail || !strings.Contains(m.Text, "Hello Casey Consumer,") {
			t.Fatalf("customer email: %s\n%s", m.To, m.Text)
		}
		recs := f.log.Records()
		last := recs[len(recs)-2]
		if last.RecipientUserID == nil || *last.RecipientUserID != identity.SeedConsumerUserID {
			t.Fatalf("the delivery record should name the account: %+v", last)
		}
	})
}

func TestAccountNotifications(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	when := time.Date(2026, 11, 14, 9, 5, 0, 0, time.UTC)
	event := func(eventType string, userID uuid.UUID) outbox.Event {
		e := identity.OutboxEvent{Type: eventType, UserID: userID, OccurredAt: when}
		return outbox.Event{ID: uuid.New(), Type: eventType, AggregateType: identity.AggregateUser, AggregateID: userID, Payload: e.Payload(), Attempts: 1, CreatedAt: when}
	}

	t.Run("welcome lists the chosen roles and which are pending", func(t *testing.T) {
		id := uuid.New()
		if err := f.identity.CreateUser(ctx, identity.NewUser{
			ID: id, Name: "Rudo Ncube", Email: "rudo@example.com", PasswordHash: "x", CreatedAt: when,
			AccountRoles: []identity.AccountRole{
				{Role: identity.AccountRoleAgent, Status: identity.AccountRolePendingVerification},
				{Role: identity.AccountRoleConsumer, Status: identity.AccountRoleActive},
				{Role: identity.AccountRoleLandlord, Status: identity.AccountRolePendingVerification},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.handle(event(identity.EventUserEmailVerified, id)); err != nil {
			t.Fatal(err)
		}
		m := f.sender.byKind("welcome")[0]
		for _, want := range []string{
			"Hello Rudo Ncube,", "  - Prospective Buyer: active\n  - Estate Agent: waiting for verification\n  - Landlord: waiting for verification",
			"does not give access to agency or business tools", publicURL + "/account",
		} {
			if !strings.Contains(m.Text, want) {
				t.Fatalf("welcome lacks %q:\n%s", want, m.Text)
			}
		}
		if m.To != "rudo@example.com" || m.Subject != "Your RightProperty365 account is ready" {
			t.Fatalf("to=%s subject=%s", m.To, m.Subject)
		}
	})

	t.Run("password changed: customers go to /sign-in, administrators to /admin/sign-in", func(t *testing.T) {
		if err := f.handle(event(identity.EventPasswordChanged, identity.SeedConsumerUserID)); err != nil {
			t.Fatal(err)
		}
		if err := f.handle(event(identity.EventPasswordChanged, identity.SeedAdminUserID)); err != nil {
			t.Fatal(err)
		}
		mails := f.sender.byKind("password-changed")
		if len(mails) != 2 || mails[0].To != identity.SeedConsumerEmail || mails[0].DevLink != publicURL+"/sign-in" ||
			mails[1].To != identity.SeedAdminEmail || mails[1].DevLink != publicURL+"/admin/sign-in" {
			t.Fatalf("unexpected password-changed emails: %+v", mails)
		}
		if !strings.Contains(mails[0].Text, "Changed: 14 November 2026, 09:05 UTC") {
			t.Fatalf("the notice must say when, with the time zone:\n%s", mails[0].Text)
		}
	})

	t.Run("administrator sign-in notice", func(t *testing.T) {
		if err := f.handle(event(identity.EventAdminSignedIn, identity.SeedAdminUserID)); err != nil {
			t.Fatal(err)
		}
		m := f.sender.byKind("admin-sign-in")[0]
		if m.To != identity.SeedAdminEmail || !strings.Contains(m.Text, "Signed in: 14 November 2026, 09:05 UTC") || m.DevLink != publicURL+"/admin/sign-in" {
			t.Fatalf("unexpected notice:\n%s", m.Text)
		}
	})

	// The administrator MFA security notices (API 0.40.0): no code, no secret.
	t.Run("administrator MFA notices", func(t *testing.T) {
		for typ, kind := range map[string]string{identity.EventAdminMFARecoveryCodeUsed: "admin-mfa-recovery-code-used", identity.EventAdminMFAReset: "admin-mfa-reset"} {
			if err := f.handle(event(typ, identity.SeedAdminUserID)); err != nil {
				t.Fatal(err)
			}
			m := f.sender.byKind(kind)[0]
			if m.To != identity.SeedAdminEmail || !strings.Contains(m.Text, "14 November 2026, 09:05 UTC") || m.DevLink != publicURL+"/admin/sign-in" {
				t.Fatalf("unexpected %s notice:\n%s", kind, m.Text)
			}
		}
	})

	t.Run("an event whose subject is gone is dead-lettered, not retried", func(t *testing.T) {
		for _, ev := range []outbox.Event{
			event(identity.EventPasswordChanged, uuid.New()),
			{ID: uuid.New(), Type: propertyrequests.EventPropertyRequestCreated, AggregateID: uuid.New()},
			{ID: uuid.New(), Type: businessenquiries.EventBusinessEnquiryCreated, AggregateID: uuid.New()},
			{ID: uuid.New(), Type: "SOMETHING_ELSE", AggregateID: uuid.New()},
		} {
			if err := f.handle(ev); !outbox.IsPermanent(err) {
				t.Fatalf("%s: err = %v, want a permanent error", ev.Type, err)
			}
		}
	})

	if got := len(EventTypes()); got != 59 {
		t.Fatalf("%d event types handled, want 59 (the data export, the saved search alert, the rerouted request, the 3 invoice events, 8 identity and request events with the request assignment and the 2 administrator MFA notices, the 3 request conversation events (stage changed, replied, customer replied), the 4 onboarding events, the 8 appointment events (with the reminder), the 4 listing review events (with the warning), the 2 listing expiry events, the 2 account events, the 5 subscription events, the 4 application events, the 8 venue booking events, the 2 maintenance events, the 2 inspection events and the review moderation event)", got)
	}
}

func TestFailuresAreRetriedOrDeadLettered(t *testing.T) {
	t.Run("a transient failure is retried and only the missing email is sent", func(t *testing.T) {
		f := newFixture(t)
		ev := f.propertyRequest(1, nil)
		f.sender.fail = func(m mail.Message, attempt int) error {
			if m.Kind == "property-request-new" && attempt == 1 {
				return &mail.SendError{Status: 503, Name: "service_unavailable", Transient: true}
			}
			return nil
		}
		err := f.handle(ev)
		if err == nil || outbox.IsPermanent(err) {
			t.Fatalf("err = %v, want a retryable error", err)
		}
		if f.sender.count() != 1 {
			t.Fatalf("%d emails accepted after the first run, want 1", f.sender.count())
		}
		if err := f.handle(ev); err != nil {
			t.Fatal(err)
		}
		if f.sender.count() != 2 || len(f.sender.byKind("property-request-received")) != 1 {
			t.Fatalf("after the retry: %d emails; the customer must not be written to twice", f.sender.count())
		}
		statuses := map[string]string{}
		for _, r := range f.log.Records() {
			statuses[r.Kind] = fmt.Sprintf("%s/%d", r.Status, r.Attempts)
		}
		if statuses["property-request-received"] != "PROVIDER_ACCEPTED/1" || statuses["property-request-new"] != "PROVIDER_ACCEPTED/2" {
			t.Fatalf("records: %v", statuses)
		}
	})

	t.Run("a terminal failure dead-letters the event", func(t *testing.T) {
		f := newFixture(t)
		ev := f.propertyRequest(1, nil)
		f.sender.fail = func(m mail.Message, _ int) error {
			if m.Kind == "property-request-new" {
				return &mail.SendError{Status: 422, Name: "missing_required_field"}
			}
			return nil
		}
		for run := range 2 {
			err := f.handle(ev)
			if !outbox.IsPermanent(err) || !errors.Is(err, mail.ErrTerminal) {
				t.Fatalf("run %d: err = %v, want a permanent, terminal error", run+1, err)
			}
		}
		// The customer's email was still accepted once; the rejected one was tried once.
		if f.sender.count() != 1 || f.sender.seen["property-request-new/"+ev.ID.String()+"/"+identity.SeedAgentUserID.String()] != 1 {
			t.Fatalf("emails=%d seen=%v", f.sender.count(), f.sender.seen)
		}
		for _, r := range f.log.Records() {
			if r.Kind == "property-request-new" && (r.Status != mail.StatusFailed || !r.Terminal) {
				t.Fatalf("record: %+v", r)
			}
		}
	})
}

// Verification and reset emails are sent from the request path; their token
// reaches the email and nothing else.
func TestAccountMailerKeepsTheTokenOutOfRecordsAndLogs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const token = "kq3V8wT2mZx0Qe7LrYbN1uJc5aHs9dGfPi4oXvEyR6W"

	if err := f.mailer.SendVerification(ctx, "new@example.com", "Rudo Ncube", token); err != nil {
		t.Fatal(err)
	}
	if err := f.mailer.SendPasswordReset(ctx, "new@example.com", "Rudo Ncube", token); err != nil {
		t.Fatal(err)
	}
	if err := f.mailer.SendAccountExists(ctx, "new@example.com", "Rudo Ncube"); err != nil {
		t.Fatal(err)
	}
	if len(f.events) != 0 {
		t.Fatal("request-path emails must not go through the outbox")
	}

	verify := f.sender.byKind("verify-email")[0]
	if verify.DevLink != publicURL+"/verify-email?token="+token || !strings.Contains(verify.Text, verify.DevLink) || !strings.Contains(verify.HTML, verify.DevLink) {
		t.Fatalf("verification link = %s", verify.DevLink)
	}
	reset := f.sender.byKind("reset-password")[0]
	if reset.DevLink != publicURL+"/reset-password?token="+token || !strings.Contains(reset.Text, "expires 1 hour") {
		t.Fatalf("reset link = %s", reset.DevLink)
	}
	exists := f.sender.byKind("account-exists")[0]
	if exists.DevLink != publicURL+"/sign-in" || strings.Contains(exists.Text, token) {
		t.Fatalf("account-exists email:\n%s", exists.Text)
	}

	// The token is in the email and nowhere else: not in the idempotency key,
	// not in the delivery records, not in the logs.
	for _, m := range f.sender.sent {
		if strings.Contains(m.Key, token) {
			t.Fatalf("the notification key contains the token: %s", m.Key)
		}
	}
	for _, r := range f.log.Records() {
		if s := fmt.Sprintf("%+v", r); strings.Contains(s, token) || strings.Contains(s, "new@example.com") {
			t.Fatalf("the delivery record contains the token or the address: %s", s)
		}
		if r.Status != mail.StatusProviderAccepted {
			t.Fatalf("record: %+v", r)
		}
	}
	if strings.Contains(f.logs.String(), token) || strings.Contains(f.logs.String(), "new@example.com") {
		t.Fatalf("the logs contain the token or the address: %s", f.logs.String())
	}

	// Two sends of the "same" email are two notifications (each has its own token in practice).
	keys := map[string]bool{}
	for range 2 {
		if err := f.mailer.SendVerification(ctx, "new@example.com", "Rudo Ncube", token); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range f.sender.byKind("verify-email") {
		keys[m.Key] = true
	}
	if len(keys) != 3 {
		t.Fatalf("%d distinct keys, want 3", len(keys))
	}

	// A provider failure is reported to the caller (which answers 202 anyway).
	f.sender.fail = func(mail.Message, int) error {
		return &mail.SendError{Status: 500, Name: "application_error", Transient: true}
	}
	if err := f.mailer.SendVerification(ctx, "new@example.com", "Rudo Ncube", token); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v", err)
	}
}

func TestMailerSelection(t *testing.T) {
	load := func(env map[string]string) config.Config {
		t.Helper()
		cfg, err := config.FromLookup(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	t.Run("development without a key: the log mailer", func(t *testing.T) {
		logs.Reset()
		s, err := NewSender(load(nil), logger)
		if err != nil || s.Name() != "log" {
			t.Fatalf("sender = %v, %v", s, err)
		}
		if !strings.Contains(logs.String(), "mailer selected: log") || !strings.Contains(logs.String(), `"mail_driver":"log"`) {
			t.Fatalf("startup line: %s", logs.String())
		}
	})

	t.Run("a key selects Resend, and the startup line never shows it", func(t *testing.T) {
		logs.Reset()
		s, err := NewSender(load(map[string]string{"RESEND_API_KEY": fakeKey}), logger)
		if err != nil || s.Name() != "resend" {
			t.Fatalf("sender = %v, %v", s, err)
		}
		if !strings.Contains(logs.String(), "mailer selected: resend") || strings.Contains(logs.String(), fakeKey) ||
			strings.Contains(fmt.Sprintf("%v %+v", s, s), fakeKey) {
			t.Fatalf("startup line: %s", logs.String())
		}
	})

	t.Run("MAIL_DRIVER=log forces the log mailer although a key is present", func(t *testing.T) {
		logs.Reset()
		s, err := NewSender(load(map[string]string{"RESEND_API_KEY": fakeKey, "MAIL_DRIVER": "log"}), logger)
		if err != nil || s.Name() != "log" {
			t.Fatalf("sender = %v, %v", s, err)
		}
		if !strings.Contains(logs.String(), "mailer selected: log") || !strings.Contains(logs.String(), `"resend_key_present_but_unused":true`) ||
			strings.Contains(logs.String(), fakeKey) {
			t.Fatalf("startup line: %s", logs.String())
		}
	})

	t.Run("the log mailer cannot be built outside development", func(t *testing.T) {
		cfg := load(nil)
		cfg.Env = config.EnvProduction
		if _, err := NewSender(cfg, logger); err == nil {
			t.Fatal("expected an error")
		}
		cfg.MailDriver = "smtp"
		if _, err := NewSender(cfg, logger); err == nil {
			t.Fatal("expected an error for an unknown driver")
		}
	})
}

type noLabels struct{}

func (noLabels) Labels(context.Context, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (noLabels) ActiveCodes(context.Context, string) ([]string, error) { return nil, nil }
