package notifications

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Each onboarding transition sends ONE email, to the applicant only, and the
// email carries no document name and no private detail.
func TestOnboardingNotifications(t *testing.T) {
	f := newFixture(t)
	applicant := uuid.New()
	info := onboarding.NotificationInfo{
		UserID: applicant, Name: "Pamela Provider", Email: "pamela@example.test", RoleLabel: "Service provider",
		Reason:  "The proof of address is older than three months.",
		Profile: &onboarding.ProfileRef{Kind: "service-providers", Slug: "sample-roofing-repairs"}, ProfileName: "Sample Roofing Repairs",
	}
	calls := 0
	f.handlers.SetOnboardingSource(func(_ context.Context, id uuid.UUID) (onboarding.NotificationInfo, error) {
		calls++
		if id == uuid.Nil {
			return onboarding.NotificationInfo{}, onboarding.ErrNotFound
		}
		return info, nil
	})
	event := func(eventType string, id uuid.UUID) outbox.Event {
		return outbox.Event{ID: uuid.New(), Type: eventType, AggregateType: onboarding.AggregateApplication, AggregateID: id,
			Payload: []byte(`{"occurred_at":"2026-11-14T09:05:00Z"}`), CreatedAt: time.Date(2026, 11, 14, 9, 5, 0, 0, time.UTC)}
	}

	cases := []struct {
		event, template, subject, link string
		reason, profile                bool
	}{
		{onboarding.EventSubmitted, "onboarding-submitted", "We have received your application", "/onboarding", false, false},
		{onboarding.EventApproved, "onboarding-approved", "Your application has been approved", "/directory/service-providers/sample-roofing-repairs", false, true},
		{onboarding.EventChangesRequested, "onboarding-changes-requested", "Your application needs changes", "/onboarding", true, false},
		{onboarding.EventRejected, "onboarding-rejected", "Your application was not approved", "/account", true, false},
	}
	for _, tc := range cases {
		ev := event(tc.event, uuid.New())
		// Handling the same event twice (a duplicate worker run) sends once.
		for i := 0; i < 2; i++ {
			if err := f.handle(ev); err != nil {
				t.Fatalf("%s: %v", tc.event, err)
			}
		}
		sent := f.sender.byKind(tc.template)
		if len(sent) != 1 {
			t.Fatalf("%s: %d emails of kind %s, want 1", tc.event, len(sent), tc.template)
		}
		m := sent[0]
		if m.To != info.Email || m.Subject != tc.subject {
			t.Fatalf("%s: to %q subject %q", tc.event, m.To, m.Subject)
		}
		for _, body := range []string{m.HTML, m.Text} {
			if !strings.Contains(body, "Service provider") || !strings.Contains(body, "Pamela Provider") || !strings.Contains(body, tc.link) {
				t.Fatalf("%s: body lacks the role, the name or the link %s:\n%s", tc.event, tc.link, m.Text)
			}
			if strings.Contains(body, info.Reason) != tc.reason {
				t.Fatalf("%s: reason shown = %v, want %v", tc.event, !tc.reason, tc.reason)
			}
			if strings.Contains(body, "Sample Roofing Repairs") != tc.profile {
				t.Fatalf("%s: profile shown = %v, want %v", tc.event, !tc.profile, tc.profile)
			}
			for _, private := range []string{"+263", "SAMPLE-0001", "sample-proof", "scan.pdf", "PROOF_OF", "Proof of identity"} {
				if strings.Contains(body, private) {
					t.Fatalf("%s: the email contains %q", tc.event, private)
				}
			}
		}
	}
	if f.sender.count() != len(cases) {
		t.Fatalf("%d emails sent, want %d", f.sender.count(), len(cases))
	}

	// An approval without a profile (a private seller) links to the account.
	info.Profile, info.ProfileName = nil, ""
	if err := f.handle(event(onboarding.EventApproved, uuid.New())); err != nil {
		t.Fatal(err)
	}
	last := f.sender.byKind("onboarding-approved")[1]
	if !strings.Contains(last.Text, "/account") || !strings.Contains(last.Text, "Your role is now active") || strings.Contains(last.Text, "/directory/") {
		t.Fatalf("approval without a profile:\n%s", last.Text)
	}

	// An application that no longer exists is dead-lettered, not retried.
	if err := f.handle(event(onboarding.EventSubmitted, uuid.Nil)); err == nil || !outbox.IsPermanent(err) {
		t.Fatalf("missing application: %v, want a permanent error", err)
	}
	// Without a source the events cannot be handled; that is permanent too.
	f.handlers.SetOnboardingSource(nil)
	if err := f.handle(event(onboarding.EventSubmitted, uuid.New())); err == nil || !outbox.IsPermanent(err) {
		t.Fatalf("no source: %v, want a permanent error", err)
	}
}
