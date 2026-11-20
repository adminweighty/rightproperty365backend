package notifications

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/inapp"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// lastWorkEvent is the newest event of a type the request store recorded
// (the memory store keeps the conversation's events instead of sinking them).
func (f *fixture) lastWorkEvent(eventType string, requestID uuid.UUID) outbox.Event {
	f.t.Helper()
	events := f.requests.Outbox()
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.EventType == eventType && e.AggregateID == requestID {
			return outbox.Event{ID: uuid.NewSHA1(requestID, []byte(eventType+e.CreatedAt.String()+string(e.Payload))), Type: e.EventType,
				AggregateType: propertyrequests.AggregatePropertyRequest, AggregateID: e.AggregateID, Payload: e.Payload, CreatedAt: e.CreatedAt}
		}
	}
	f.t.Fatalf("no %s event for %s", eventType, requestID)
	return outbox.Event{}
}

// The conversation on a request (API 0.46.0): the customer is told of a
// reply (with a preview) and of a change of THEIR status; the receiving side
// of the customer's answer. Message text never reaches the in-app notices.
func TestRequestConversationNotifications(t *testing.T) {
	f := newFixture(t)
	f.handlers.SetRequestMessageSource(f.requests.Messages)
	_, inbox := f.withInbox()
	ctx := context.Background()
	svc := propertyrequests.NewService(f.requests)
	customer := &propertyrequests.Customer{UserID: identity.SeedConsumerUserID, Name: "Casey Consumer", Email: identity.SeedConsumerEmail}

	created := f.propertyRequest(1, customer)
	req, err := f.requests.GetRequest(ctx, created.AggregateID)
	if err != nil {
		t.Fatal(err)
	}
	agent := agentID(t, f)
	scope := propertyrequests.InboxScope{UserID: agent, MemberOrganisationIDs: []uuid.UUID{*req.Recipient.OrganisationID}}
	actor := propertyrequests.Actor{UserID: agent, Name: "Agent"}

	// The first reply: request-update with the preview, to the request page;
	// the stage move it makes sends nothing more.
	if _, _, err := svc.Reply(ctx, scope, actor, req.ID, "The house is still available. Saturday at ten?"); err != nil {
		t.Fatal(err)
	}
	stage := f.lastWorkEvent(propertyrequests.EventStageChanged, req.ID)
	before := len(f.sender.byKind("request-update"))
	if err := f.handle(stage); err != nil {
		t.Fatal(err)
	}
	if n := len(f.sender.byKind("request-update")) - before; n != 0 {
		t.Fatalf("the move a reply makes sent %d emails", n)
	}
	replied := f.lastWorkEvent(propertyrequests.EventReplied, req.ID)
	for range 2 { // a duplicate worker run sends once
		if err := f.handle(replied); err != nil {
			t.Fatal(err)
		}
	}
	sent := f.sender.byKind("request-update")[before:]
	if recipients(sent) != identity.SeedConsumerEmail || !strings.Contains(sent[0].Text, "Saturday at ten?") ||
		!strings.Contains(sent[0].Text, publicURL+PathMyRequests+req.ID.String()) || strings.Contains(sent[0].Text, "without an account") {
		t.Fatalf("reply email: %v %s", recipients(sent), sent[0].Text)
	}
	notes := notificationsOf(t, inbox, identity.SeedConsumerUserID)
	if len(notes) == 0 || notes[0].Type != inapp.TypeRequestUpdate || strings.Contains(notes[0].Title+notes[0].Body, "Saturday") {
		t.Fatalf("customer in-app: %+v", notes)
	}

	// The customer answers: the assigned agent hears of it.
	if _, _, err := svc.CustomerReply(ctx, identity.SeedConsumerUserID, "Casey", req.ID, "Ten is fine. Is there parking?"); err != nil {
		t.Fatal(err)
	}
	before = len(f.sender.byKind("request-customer-replied"))
	if err := f.handle(f.lastWorkEvent(propertyrequests.EventCustomerReplied, req.ID)); err != nil {
		t.Fatal(err)
	}
	sent = f.sender.byKind("request-customer-replied")[before:]
	if recipients(sent) != identity.SeedAgentEmail || !strings.Contains(sent[0].Text, "Is there parking?") {
		t.Fatalf("customer reply email: %v", recipients(sent))
	}
	agentNotes := notificationsOf(t, inbox, agent)
	if len(agentNotes) == 0 || agentNotes[0].Type != inapp.TypeNewLead || strings.Contains(agentNotes[0].Title+agentNotes[0].Body, "parking") {
		t.Fatalf("agent in-app: %+v", agentNotes)
	}

	// A move that keeps the customer's status sends nothing; one that
	// changes it sends the new status.
	if _, err := svc.MoveStage(ctx, scope, actor, req.ID, propertyrequests.StageFollowUpRequired); err != nil {
		t.Fatal(err)
	}
	before = len(f.sender.byKind("request-update"))
	if err := f.handle(f.lastWorkEvent(propertyrequests.EventStageChanged, req.ID)); err != nil {
		t.Fatal(err)
	}
	// CONTACTED (Responded) -> FOLLOW_UP_REQUIRED (In progress): changed.
	sent = f.sender.byKind("request-update")[before:]
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "Status: In progress") {
		t.Fatalf("status email: %d %v", len(sent), sent)
	}
	if _, err := svc.MoveStage(ctx, scope, actor, req.ID, propertyrequests.StageViewingRequested); err != nil {
		t.Fatal(err)
	}
	before = len(f.sender.byKind("request-update"))
	if err := f.handle(f.lastWorkEvent(propertyrequests.EventStageChanged, req.ID)); err != nil {
		t.Fatal(err)
	}
	if n := len(f.sender.byKind("request-update")) - before; n != 0 {
		t.Fatalf("an unchanged customer status sent %d emails", n)
	}
}

// A guest's request: the reply goes by email with the guest wording and a
// link to the home page, never to a request page they cannot open.
func TestRequestReplyToAGuest(t *testing.T) {
	f := newFixture(t)
	f.handlers.SetRequestMessageSource(f.requests.Messages)
	ctx := context.Background()
	created := f.propertyRequest(1, nil)
	req, err := f.requests.GetRequest(ctx, created.AggregateID)
	if err != nil {
		t.Fatal(err)
	}
	agent := agentID(t, f)
	scope := propertyrequests.InboxScope{UserID: agent, MemberOrganisationIDs: []uuid.UUID{*req.Recipient.OrganisationID}}
	if _, _, err := propertyrequests.NewService(f.requests).Reply(ctx, scope, propertyrequests.Actor{UserID: agent, Name: "Agent"}, req.ID, "Yes, it is available."); err != nil {
		t.Fatal(err)
	}
	if err := f.handle(f.lastWorkEvent(propertyrequests.EventReplied, req.ID)); err != nil {
		t.Fatal(err)
	}
	sent := f.sender.byKind("request-update")
	if len(sent) != 1 || sent[0].To != "tariro@example.com" || !strings.Contains(sent[0].Text, "without an account") ||
		strings.Contains(sent[0].Text, "/account/requests/") || !strings.Contains(sent[0].Text, "Visit RightProperty365") {
		t.Fatalf("guest email: %+v", sent)
	}
}
