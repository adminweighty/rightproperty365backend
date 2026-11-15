package notifications

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/teams"
)

// assignees resolves the seeded accounts for the request service.
type assignees struct{ store *identity.MemoryStore }

func (a assignees) Assignable(ctx context.Context, org, user uuid.UUID) (string, bool, error) {
	u, err := a.store.UserByID(ctx, user)
	if err != nil {
		return "", false, nil
	}
	for _, m := range u.Memberships {
		if m.OrganisationID == org {
			return u.Name, true, nil
		}
	}
	return "", false, nil
}

func (a assignees) Name(ctx context.Context, user uuid.UUID) (string, error) {
	u, err := a.store.UserByID(ctx, user)
	if err != nil {
		return "", nil
	}
	return u.Name, nil
}

// Assigning a request emails the person it was given to, and only them:
// a reference, the listing and a preview with contact details removed.
func TestRequestAssignedEmail(t *testing.T) {
	f := newFixture(t)
	created := f.propertyRequest(7, nil) // Chipo Dube's listing
	if err := f.handle(created); err != nil {
		t.Fatal(err)
	}
	id := created.AggregateID
	svc := propertyrequests.NewService(f.requests, propertyrequests.WithAssignees(assignees{f.identity}))
	owner := propertyrequests.InboxScope{UserID: devseed.EstatesOwnerID, ManagedOrganisationIDs: []uuid.UUID{devseed.SampleEstates.ID}}
	before := len(f.events)
	if _, err := svc.Assign(context.Background(), owner, propertyrequests.Actor{UserID: devseed.EstatesOwnerID, Name: "Rudo Moyo"}, id, &identity.SeedAgentUserID); err != nil {
		t.Fatal(err)
	}
	if len(f.events) != before+1 || f.events[len(f.events)-1].Type != propertyrequests.EventRequestAssigned {
		t.Fatalf("events after the assignment: %d", len(f.events)-before)
	}
	if err := f.handle(f.events[len(f.events)-1]); err != nil {
		t.Fatal(err)
	}
	mails := f.sender.byKind("property-request-assigned")
	if len(mails) != 1 || mails[0].To != identity.SeedAgentEmail {
		t.Fatalf("assignment emails: %q", recipients(mails))
	}
	text := mails[0].Text
	for _, want := range []string{"Rudo Moyo assigned you a property request", "Reference: RP-"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the email lacks %q:\n%s", want, text)
		}
	}
	for _, secret := range []string{"tariro@example.com", "123 4567"} {
		if strings.Contains(text, secret) || strings.Contains(mails[0].HTML, secret) {
			t.Fatalf("the email carries the customer's contact details (%q)", secret)
		}
	}
	// Unassigning sends nothing.
	before = len(f.events)
	if _, err := svc.Assign(context.Background(), owner, propertyrequests.Actor{UserID: devseed.EstatesOwnerID, Name: "Rudo Moyo"}, id, nil); err != nil {
		t.Fatal(err)
	}
	for _, ev := range f.events[before:] {
		if ev.Type == propertyrequests.EventRequestAssigned {
			t.Fatal("an unassignment must not email anyone")
		}
	}
}

// The invitation email carries the link with the token in its path and
// names the organisation, the role and the inviter.
func TestInvitationEmail(t *testing.T) {
	f := newFixture(t)
	token := "not-a-real-invitation-token"
	if err := f.mailer.SendInvitation(context.Background(), "new.agent@example.com", teams.InvitationMail{
		Token: token, OrganisationName: "Sample Estates", RoleLabel: "Estate Agent", InviterName: "Rudo Moyo",
		ExpiresAt: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	mails := f.sender.byKind("team-invitation")
	if len(mails) != 1 || mails[0].To != "new.agent@example.com" {
		t.Fatalf("invitation emails: %q", recipients(mails))
	}
	if !strings.Contains(mails[0].Text, publicURL+"/invitations/"+token) || !strings.Contains(mails[0].Subject, "Sample Estates") {
		t.Fatalf("unexpected invitation email:\n%s\n%s", mails[0].Subject, mails[0].Text)
	}
	// The delivery record never holds the token.
	for _, r := range f.log.Records() {
		if strings.Contains(r.Key, token) {
			t.Fatal("the notification key contains the token")
		}
	}
}
