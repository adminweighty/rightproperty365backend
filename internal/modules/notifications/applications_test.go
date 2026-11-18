package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Application emails: the applicant hears of the submission (with the
// reference), a request for more information and a decision; the
// receiving side, the same audience as property requests, hears of a new
// application. Nothing for UNDER_REVIEW or a withdrawal.
func TestApplicationNotifications(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 11, 14, 9, 5, 0, 0, time.UTC)
	org := devseed.SampleEstates.ID
	tenant := devseed.TenantID
	agent := identity.SeedAgentUserID
	notices := map[uuid.UUID]applications.Notice{}
	f.handlers.SetApplicationSource(func(_ context.Context, id uuid.UUID) (applications.Notice, error) {
		n, ok := notices[id]
		if !ok {
			return applications.Notice{}, applications.ErrNotFound
		}
		return n, nil
	})
	withAgent, withoutAgent := uuid.New(), uuid.New()
	base := applications.Notice{Reference: "RPA-APP-2026-000007", ListingTitle: "Flat one", ListingReference: "RP-ZW-10004", ApplicantUserID: &tenant,
		ApplicantName: "Tariro Tenant", ApplicantEmail: devseed.TenantEmail, OrganisationID: &org, InfoMessage: "Please add a payslip. Call +263 77 123 4567."}
	notices[withoutAgent] = base
	base.AgentUserID = &agent
	notices[withAgent] = base
	event := func(t string, id uuid.UUID, p applications.EventPayload) outbox.Event {
		p.ApplicationID, p.OccurredAt = id, now
		raw, _ := json.Marshal(p)
		return outbox.Event{ID: uuid.New(), Type: t, AggregateType: applications.AggregateApplication, AggregateID: id, Payload: raw, CreatedAt: now}
	}
	cases := []struct {
		name      string
		ev        outbox.Event
		templates map[string]string // template -> recipients
		text      string
	}{
		{"submitted, to the assigned agent", event(applications.EventTypeSubmitted, withAgent, applications.EventPayload{}),
			map[string]string{"application-submitted": devseed.TenantEmail, "application-new": identity.SeedAgentEmail}, "RPA-APP-2026-000007"},
		{"submitted, to the owners and managers", event(applications.EventTypeSubmitted, withoutAgent, applications.EventPayload{}),
			map[string]string{"application-submitted": devseed.TenantEmail, "application-new": "agency.admin@example.test," + devseed.EstatesOwnerEmail}, "Flat one"},
		{"more information", event(applications.EventTypeInfoRequested, withAgent, applications.EventPayload{To: applications.StatusMoreInfo}),
			map[string]string{"application-more-info": devseed.TenantEmail}, "Please add a payslip. Call [number hidden]."},
		{"shortlisted", event(applications.EventTypeStatusChanged, withAgent, applications.EventPayload{To: applications.StatusShortlisted}),
			map[string]string{"application-status-changed": devseed.TenantEmail}, "Status: Shortlisted"},
		{"under review sends nothing", event(applications.EventTypeStatusChanged, withAgent, applications.EventPayload{To: applications.StatusUnderReview}), nil, ""},
		{"withdrawn sends nothing", event(applications.EventTypeWithdrawn, withAgent, applications.EventPayload{To: applications.StatusWithdrawn}), nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := f.sender.count()
			for range 2 {
				if err := f.handle(tc.ev); err != nil {
					t.Fatal(err)
				}
			}
			total := 0
			for template, to := range tc.templates {
				sent := f.sender.byKind(template)
				if len(sent) == 0 {
					t.Fatalf("no %s", template)
				}
				last := sent[len(sent)-len(strings.Split(to, ",")):]
				if recipients(last) != to {
					t.Fatalf("%s sent to %s, want %s", template, recipients(last), to)
				}
				for _, m := range last {
					if strings.Contains(m.Text, "+263") || (template == "application-new" && strings.Contains(m.Text, devseed.TenantEmail)) {
						t.Fatalf("%s leaks contact details:\n%s", template, m.Text)
					}
				}
				if tc.text != "" && template != "application-new" && !strings.Contains(last[0].Text, tc.text) {
					t.Fatalf("%s lacks %q:\n%s", template, tc.text, last[0].Text)
				}
				total += len(last)
			}
			if f.sender.count()-before != total {
				t.Fatalf("%d emails, want %d (once each)", f.sender.count()-before, total)
			}
		})
	}
	if err := f.handle(event(applications.EventTypeSubmitted, uuid.New(), applications.EventPayload{})); !outbox.IsPermanent(err) {
		t.Fatalf("unknown application: %v", err)
	}
}
