package notifications

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/accounts"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/outbox"
)

func userEvent(eventType string, userID uuid.UUID) outbox.Event {
	return outbox.Event{ID: uuid.New(), Type: eventType, AggregateType: "user", AggregateID: userID,
		Payload: []byte(`{"occurred_at":"2026-11-14T09:05:00Z"}`), CreatedAt: time.Date(2026, 11, 14, 9, 5, 0, 0, time.UTC)}
}

// Suspension and reactivation are emailed to the account, with the reason.
func TestAccountStatusEmails(t *testing.T) {
	f := newFixture(t)
	if err := f.handlers.Handle(context.Background(), userEvent(accounts.EventAccountSuspended, identity.SeedAgentUserID)); err == nil ||
		!outbox.IsPermanent(err) {
		t.Fatalf("without the reasons the event is dead-lettered: %v", err)
	}
	f.handlers.SetSuspensionReasons(func(context.Context, uuid.UUID) (string, error) {
		return "Reported by customers. Call +263 77 123 4567", nil
	})
	if err := f.handle(userEvent(accounts.EventAccountSuspended, identity.SeedAgentUserID)); err != nil {
		t.Fatal(err)
	}
	mails := f.sender.byKind("account-suspended")
	if len(mails) != 1 || mails[0].To != identity.SeedAgentEmail || !strings.Contains(mails[0].Text, "Reason: Reported by customers") ||
		strings.Contains(mails[0].Text, "123 4567") {
		t.Fatalf("suspension email: %q\n%s", recipients(mails), mails[0].Text)
	}
	if err := f.handle(userEvent(accounts.EventAccountReactivated, identity.SeedAgentUserID)); err != nil {
		t.Fatal(err)
	}
	if mails := f.sender.byKind("account-reactivated"); len(mails) != 1 {
		t.Fatalf("reactivation emails: %q", recipients(mails))
	}
	if err := f.mailer.SendAccountDeleted(context.Background(), "gone@example.com", "Gone"); err != nil {
		t.Fatal(err)
	}
	if mails := f.sender.byKind("account-deletion-completed"); len(mails) != 1 || mails[0].To != "gone@example.com" {
		t.Fatalf("deletion emails: %q", recipients(mails))
	}
}

// Every subscription event emails the person who asked for it.
func TestSubscriptionEmails(t *testing.T) {
	f := newFixture(t)
	store := subscriptions.NewMemoryStore(nil)
	now := time.Date(2026, 11, 14, 9, 5, 0, 0, time.UTC)
	if err := devseed.SeedMemorySubscriptions(context.Background(), store, now); err != nil {
		t.Fatal(err)
	}
	f.handlers.SetSubscriptionSource(store.Subscription)
	ev := func(eventType string) outbox.Event {
		return outbox.Event{ID: uuid.New(), Type: eventType, AggregateType: subscriptions.AggregateSubscription, AggregateID: devseed.PendingAgentSubscription,
			Payload: []byte(`{}`), CreatedAt: now}
	}
	if err := f.handle(ev(subscriptions.EventRequested)); err != nil {
		t.Fatal(err)
	}
	mails := f.sender.byKind("subscription-invoice")
	if len(mails) != 1 || mails[0].To != identity.SeedAgentEmail {
		t.Fatalf("invoice emails: %q", recipients(mails))
	}
	for _, want := range []string{"Invoice: RPS-", "Amount: USD 30.00", "Period: 3 months", "nothing is charged automatically"} {
		if !strings.Contains(mails[0].Text, want) {
			t.Fatalf("the invoice email lacks %q:\n%s", want, mails[0].Text)
		}
	}
	start, end := now, now.AddDate(0, 3, 0)
	if _, err := store.ChangeSubscription(context.Background(), devseed.PendingAgentSubscription, func(x *subscriptions.Subscription) (subscriptions.Change, error) {
		x.Status, x.StartsAt, x.EndsAt, x.ActivatedAt = subscriptions.StatusActive, &start, &end, &now
		x.Payment = &subscriptions.Payment{Reference: "B-1", Amount: "30.00", ReceivedOn: "2026-11-14"}
		return subscriptions.Change{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, t2 := range []struct{ event, template string }{
		{subscriptions.EventActivated, "subscription-activated"}, {subscriptions.EventCancelled, "subscription-cancelled"},
		{subscriptions.EventRejected, "subscription-rejected"}, {subscriptions.EventExpired, "subscription-expired"},
	} {
		if err := f.handle(ev(t2.event)); err != nil {
			t.Fatal(err)
		}
		if mails := f.sender.byKind(t2.template); len(mails) != 1 {
			t.Fatalf("%s emails: %q", t2.template, recipients(mails))
		}
	}
	if !strings.Contains(f.sender.byKind("subscription-activated")[0].Text, "Ends: 14 February 2027") {
		t.Fatalf("activation email:\n%s", f.sender.byKind("subscription-activated")[0].Text)
	}
}
