package propertyrequests

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// messageFixture is two requests to one organisation: one sent by a
// signed-in customer (assigned to an agent), one by a guest (in the
// organisation inbox).
type messageFixture struct {
	market, org, agent, other, customer, listing uuid.UUID
	signedIn, guest                              uuid.UUID
}

func newMessageFixture() messageFixture {
	return messageFixture{market: uuid.New(), org: uuid.New(), agent: uuid.New(), other: uuid.New(), customer: uuid.New(), listing: uuid.New(),
		signedIn: uuid.New(), guest: uuid.New()}
}

func (f messageFixture) seeds(created time.Time) []SeedRequest {
	org, agent, customer := f.org, f.agent, f.customer
	mk := func(id uuid.UUID, rec Recipient, customerID *uuid.UUID) SeedRequest {
		req := NewRequest{ID: id, Reference: "MS" + strings.ToUpper(id.String()[:6]), MarketID: f.market, ListingID: f.listing,
			RequestType: TypeEnquiry, Status: StatusReceived, Message: "Is it available?", PreferredContactMethod: ContactEmail,
			IdempotencyKey: "msg-" + id.String(), RequestHash: "fixture", PrivacyVersion: "2026-01", ConsentAcceptedAt: created,
			Recipient: &rec, CreatedAt: created, CustomerUserID: customerID}
		if customerID == nil {
			req.GuestName, req.GuestEmail = "Guest", "guest@example.test"
		}
		return SeedRequest{Request: req}
	}
	return []SeedRequest{
		mk(f.signedIn, Recipient{Type: RecipientAgent, OrganisationID: &org, UserID: &agent}, &customer),
		mk(f.guest, Recipient{Type: RecipientOrganisation, OrganisationID: &org}, nil),
	}
}

// messageContract runs the conversation rules on a store seeded with the
// fixture. outbox counts this fixture's events of a type.
func messageContract(t *testing.T, store Store, f messageFixture, outbox func(eventType string) int) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	svc := NewService(store, WithClock(func() time.Time { return now }))
	agentScope := InboxScope{UserID: f.agent, MemberOrganisationIDs: []uuid.UUID{f.org}}
	otherScope := InboxScope{UserID: f.other}
	agent := Actor{UserID: f.agent, Name: "Agent A"}

	// Nobody outside the request sees or writes.
	if _, _, err := svc.Reply(ctx, otherScope, Actor{UserID: f.other}, f.signedIn, "Hello"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider reply: %v", err)
	}
	if _, _, err := svc.CustomerReply(ctx, f.other, "X", f.signedIn, "Hello"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other customer: %v", err)
	}
	// The customer cannot start the conversation.
	if _, _, err := svc.CustomerReply(ctx, f.customer, "Rudo", f.signedIn, "Hello?"); !errors.Is(err, ErrReplyNotOpen) {
		t.Fatalf("customer first: %v", err)
	}
	var verr *ValidationError
	if _, _, err := svc.Reply(ctx, agentScope, agent, f.signedIn, "   "); !errors.As(err, &verr) {
		t.Fatalf("empty body: %v", err)
	}
	if _, _, err := svc.Reply(ctx, agentScope, agent, f.signedIn, strings.Repeat("a", MaxMessageLength+1)); !errors.As(err, &verr) {
		t.Fatalf("long body: %v", err)
	}

	// The first reply moves NEW_ENQUIRY to CONTACTED, once.
	r, m, err := svc.Reply(ctx, agentScope, agent, f.signedIn, "  Yes, it is available.  ")
	if err != nil || r.Stage != StageContacted || r.MessageCount != 1 || m.Body != "Yes, it is available." || r.AwaitingReply() || !r.UnreadReply() || !r.CanCustomerReply() {
		t.Fatalf("reply: %+v %+v %v", r, m, err)
	}
	if CustomerStatus(r.Stage) != CustomerResponded {
		t.Fatalf("customer status %s", CustomerStatus(r.Stage))
	}
	if n := outbox(EventStageChanged); n != 1 {
		t.Fatalf("%d stage events after the first reply", n)
	}
	now = now.Add(time.Minute)
	if r, _, err = svc.Reply(ctx, agentScope, agent, f.signedIn, "We can show it on Saturday."); err != nil || r.Stage != StageContacted || r.MessageCount != 2 {
		t.Fatalf("second reply: %+v %v", r, err)
	}
	if n := outbox(EventStageChanged); n != 1 {
		t.Fatalf("%d stage events: the second reply must not move the stage", n)
	}

	// The customer reads, then answers.
	now = now.Add(time.Minute)
	if err := svc.MarkRead(ctx, f.signedIn, AuthorCustomer); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetRequest(ctx, f.signedIn); got.UnreadReply() || got.CustomerLastReadAt == nil {
		t.Fatalf("after reading: %+v", got)
	}
	now = now.Add(time.Minute)
	if r, _, err = svc.CustomerReply(ctx, f.customer, "Rudo", f.signedIn, "Saturday at ten works."); err != nil || !r.AwaitingReply() || r.MessageCount != 3 || r.Stage != StageContacted {
		t.Fatalf("customer reply: %+v %v", r, err)
	}
	msgs, err := svc.Messages(ctx, f.signedIn)
	if err != nil || len(msgs) != 3 || msgs[0].AuthorType != AuthorProfessional || msgs[2].AuthorType != AuthorCustomer || msgs[2].Body != "Saturday at ten works." {
		t.Fatalf("messages: %+v %v", msgs, err)
	}
	if outbox(EventReplied) != 2 || outbox(EventCustomerReplied) != 1 {
		t.Fatalf("events: %d replied, %d customer replied", outbox(EventReplied), outbox(EventCustomerReplied))
	}
	// The timeline records the messages, never their text.
	entries, _ := store.RequestTimeline(ctx, f.signedIn)
	replied := 0
	for _, e := range entries {
		if e.EventType == TimelineReplied || e.EventType == TimelineCustomerReplied {
			replied++
		}
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), "Saturday") {
			t.Fatalf("timeline carries a message: %s", raw)
		}
	}
	if replied != 3 {
		t.Fatalf("%d message entries in the timeline", replied)
	}

	// A closed request takes no message from either side.
	if _, err := svc.MoveStage(ctx, agentScope, agent, f.signedIn, StageClosed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Reply(ctx, agentScope, agent, f.signedIn, "One more thing"); !errors.Is(err, ErrRequestClosed) {
		t.Fatalf("closed reply: %v", err)
	}
	if _, _, err := svc.CustomerReply(ctx, f.customer, "Rudo", f.signedIn, "Hello?"); !errors.Is(err, ErrRequestClosed) {
		t.Fatalf("closed customer reply: %v", err)
	}

	// A reply on a request already past CONTACTED leaves the stage alone;
	// the guest's request (organisation inbox) is open to the member.
	if _, err := svc.MoveStage(ctx, agentScope, agent, f.guest, StageViewingRequested); err != nil {
		t.Fatal(err)
	}
	if r, _, err = svc.Reply(ctx, agentScope, agent, f.guest, "Which day suits you?"); err != nil || r.Stage != StageViewingRequested || r.CanCustomerReply() {
		t.Fatalf("guest reply: %+v %v", r, err)
	}
	if err := svc.MarkRead(ctx, uuid.New(), AuthorProfessional); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mark read missing: %v", err)
	}
}

func TestMemoryMessageContract(t *testing.T) {
	store := NewMemoryStore()
	f := newMessageFixture()
	if err := store.SeedRequests(context.Background(), f.seeds(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}
	messageContract(t, store, f, func(eventType string) int {
		n := 0
		for _, e := range store.Outbox() {
			if e.EventType == eventType {
				n++
			}
		}
		return n
	})
}

func TestPostgresMessageContract(t *testing.T) {
	pool := openFunnelDatabase(t)
	ctx := context.Background()
	f := newMessageFixture()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM markets WHERE code = 'ZW'`).Scan(&f.market); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO organisations (id, market_id, name, kind, status) VALUES ($1, $2, $3, 'AGENCY', 'ACTIVE')`,
		f.org, f.market, "Message Test "+f.org.String()[:8])
	for _, u := range []uuid.UUID{f.agent, f.other, f.customer} {
		exec(`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Message Tester', 'ACTIVE', now())`,
			u, "msg."+u.String()[:8]+"@example.test")
	}
	exec(`INSERT INTO listings (id, market_id, title, status, price_amount, currency_code, organisation_id)
		VALUES ($1, $2, 'Message listing', 'PUBLISHED', 100, 'USD', $3)`, f.listing, f.market, f.org)
	store := NewPostgresStore(pool)
	if err := store.SeedRequests(ctx, f.seeds(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}
	messageContract(t, store, f, func(eventType string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type = $1 AND aggregate_id = ANY($2::uuid[])`,
			eventType, []uuid.UUID{f.signedIn, f.guest}).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
}

func TestCustomerViewOfMessages(t *testing.T) {
	agent := uuid.New()
	org := uuid.New()
	r := StoredRequest{Recipient: &Recipient{Type: RecipientAgent, OrganisationID: &org, UserID: &agent}}
	msgs := []Message{
		{ID: uuid.New(), AuthorType: AuthorProfessional, AuthorUserID: &agent, AuthorName: "Tendai Ncube", Body: "Hello"},
		{ID: uuid.New(), AuthorType: AuthorCustomer, AuthorName: "Rudo Moyo", Body: "Hi"},
	}
	got := customerMessages(r, msgs)
	if got[0].AuthorName != "The agent" || got[0].Mine || got[1].AuthorName != "You" || !got[1].Mine {
		t.Fatalf("customer view: %+v", got)
	}
	r.Recipient = &Recipient{Type: RecipientSeller, UserID: &agent}
	if customerMessages(r, msgs)[0].AuthorName != "The seller" {
		t.Fatal("seller name")
	}
	pro := professionalMessages(msgs, agent)
	if pro[0].AuthorName != "Tendai Ncube" || !pro[0].Mine || pro[1].AuthorName != "Rudo Moyo" || pro[1].Mine {
		t.Fatalf("professional view: %+v", pro)
	}
}
