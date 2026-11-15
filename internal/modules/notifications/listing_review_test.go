package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// A review decision sends ONE email, to the advertiser only: the assigned
// agent of an agency listing, the owners and managers of an agency listing
// without an agent, the direct owner of their own listing.
func TestListingReviewNotifications(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 11, 14, 9, 5, 0, 0, time.UTC)
	catalogue := listings.NewMemoryStore(noLabels{})
	devseed.SeedMemoryContent(catalogue, directory.NewMemoryStore(), now)
	f.handlers.SetListingSource(func(ctx context.Context, id uuid.UUID) (listings.Record, error) {
		records, err := catalogue.ByIDs(ctx, []uuid.UUID{id})
		if err != nil {
			return listings.Record{}, err
		}
		if len(records) == 0 {
			return listings.Record{}, listings.ErrNotFound
		}
		return records[0], nil
	})
	event := func(eventType string, id uuid.UUID, kind, reason string) outbox.Event {
		raw, _ := json.Marshal(listings.ReviewEventPayload{ListingID: id, ReviewKind: kind, Reason: reason, OccurredAt: now})
		return outbox.Event{ID: uuid.New(), Type: eventType, AggregateType: listings.AggregateListing, AggregateID: id, Payload: raw, CreatedAt: now}
	}

	cases := []struct {
		name, event, kind, reason, template, subject, to, text string
		listing                                                uuid.UUID
	}{
		{"agent's listing approved", listings.EventListingApproved, listings.ReviewKindNew, "", "listing-approved", "Your listing is approved",
			"agent@example.test", "It is now shown to customers.", propertyrequests.SeedListingID(1)},
		{"changes approved", listings.EventListingApproved, listings.ReviewKindEdit, "", "listing-approved", "Your listing is approved",
			"agent@example.test", "the changes to your listing", propertyrequests.SeedListingID(2)},
		{"changes requested of a direct owner", listings.EventListingChangesRequested, listings.ReviewKindNew,
			"Please describe every photo. Call +263 77 123 4567.", "listing-changes-requested", "Your listing needs changes",
			"seller@example.test", "What to change: Please describe every photo. Call [number hidden].", propertyrequests.SeedListingID(6)},
		{"agency listing without an agent rejected", listings.EventListingRejected, listings.ReviewKindNew, "A duplicate of RP-ZW-10001.",
			"listing-rejected", "Your listing was not approved", "agency.admin@example.test," + devseed.EstatesOwnerEmail, "Reason: A duplicate of RP-ZW-10001.", devseed.ExpiredListingID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := event(tc.event, tc.listing, tc.kind, tc.reason)
			before := len(f.sender.byKind(tc.template))
			for i := 0; i < 2; i++ { // a duplicate worker run sends once
				if err := f.handle(ev); err != nil {
					t.Fatal(err)
				}
			}
			sent := f.sender.byKind(tc.template)[before:]
			if recipients(sent) != tc.to || sent[0].Subject != tc.subject {
				t.Fatalf("sent to %v, want %q to %s (once each)", recipients(sent), tc.subject, tc.to)
			}
			if !strings.Contains(sent[0].Text, tc.text) || !strings.Contains(sent[0].Text, publicURL+PathMyListings) {
				t.Fatalf("text lacks %q or the link:\n%s", tc.text, sent[0].Text)
			}
		})
	}

	// An unknown listing is dead-lettered, not retried.
	if err := f.handle(event(listings.EventListingApproved, uuid.New(), listings.ReviewKindNew, "")); !outbox.IsPermanent(err) {
		t.Fatalf("unknown listing: err = %v, want a permanent error", err)
	}
}
