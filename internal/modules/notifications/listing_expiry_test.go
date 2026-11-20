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
	"rightproperty365.com/backend/internal/modules/inapp"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// withListings connects the seed catalogue as the listing source.
func (f *fixture) withListings(now time.Time) *listings.MemoryStore {
	f.t.Helper()
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
	return catalogue
}

func expiryEvent(eventType string, id uuid.UUID, days int, expires, now time.Time) outbox.Event {
	raw, _ := json.Marshal(listings.ExpiryEventPayload{ListingID: id, Days: days, ExpiresAt: expires, RelistNo: 1, OccurredAt: now})
	return outbox.Event{ID: uuid.New(), Type: eventType, AggregateType: listings.AggregateListing, AggregateID: id, Payload: raw, CreatedAt: now}
}

// The expiry sweep's events (API 0.45.0): one email per advertiser, the
// same recipients as the review emails, sent once however often the worker
// runs the event, and an in-app notice of type LISTING_EXPIRY.
func TestListingExpiryNotifications(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	f.withListings(now)
	_, svc := f.withInbox()
	agent := agentID(t, f)
	expires := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name, event                   string
		days                          int
		listing                       uuid.UUID
		template, subject, to, inText string
	}{
		{"the agent's listing, 7 days", listings.EventListingExpiring, 7, propertyrequests.SeedListingID(1), "listing-expiring",
			"Your listing expires in 7 days", "agent@example.test", "Expires: 27 November 2026"},
		{"a direct owner's listing, 1 day", listings.EventListingExpiring, 1, propertyrequests.SeedListingID(6), "listing-expiring",
			"Your listing expires in 1 day", "seller@example.test", "Expires: 27 November 2026"},
		{"an agency listing without an agent, expired", listings.EventListingExpired, 0, devseed.ExpiredListingID, "listing-expired",
			"Your listing has expired", "agency.admin@example.test," + devseed.EstatesOwnerEmail, "Expired: 27 November 2026"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := expiryEvent(tc.event, tc.listing, tc.days, expires, now)
			before := len(f.sender.byKind(tc.template))
			for range 2 { // a duplicate worker run sends once
				if err := f.handle(ev); err != nil {
					t.Fatal(err)
				}
			}
			sent := f.sender.byKind(tc.template)[before:]
			if recipients(sent) != tc.to || sent[0].Subject != tc.subject {
				t.Fatalf("sent %q to %v, want %q to %s (once each)", sent[0].Subject, recipients(sent), tc.subject, tc.to)
			}
			if !strings.Contains(sent[0].Text, tc.inText) || !strings.Contains(sent[0].Text, publicURL+PathMyListings) {
				t.Fatalf("text lacks %q or the link:\n%s", tc.inText, sent[0].Text)
			}
		})
	}

	// The agent's in-app notices: type, title and link to the listing.
	items := notificationsOf(t, svc, agent)
	if len(items) != 1 || items[0].Type != inapp.TypeListingExpiry || items[0].Title != "Your listing expires in 7 days" ||
		items[0].Link != "/agent/listings/"+propertyrequests.SeedListingID(1).String() {
		t.Fatalf("in-app: %+v", items)
	}
	// Tomorrow.
	if err := f.handle(expiryEvent(listings.EventListingExpiring, propertyrequests.SeedListingID(1), 1, expires, now)); err != nil {
		t.Fatal(err)
	}
	if items := notificationsOf(t, svc, agent); len(items) != 2 ||
		(items[0].Title != "Your listing expires tomorrow" && items[1].Title != "Your listing expires tomorrow") {
		t.Fatalf("in-app tomorrow: %+v", items)
	}

	// An event without the date (written before 0.45.0) takes the listing's.
	raw, _ := json.Marshal(map[string]string{"listing_id": propertyrequests.SeedListingID(1).String()})
	legacy := outbox.Event{ID: uuid.New(), Type: listings.EventListingExpired, AggregateType: listings.AggregateListing,
		AggregateID: propertyrequests.SeedListingID(1), Payload: raw, CreatedAt: now}
	if err := f.handle(legacy); err != nil {
		t.Fatal(err)
	}
	if sent := f.sender.byKind("listing-expired"); len(sent) != 3 || strings.Contains(sent[2].Text, "January 0001") {
		t.Fatalf("legacy expired: %d sent", len(sent))
	}

	// A reminder without days is dead-lettered; an unknown listing too.
	if err := f.handle(expiryEvent(listings.EventListingExpiring, propertyrequests.SeedListingID(1), 0, expires, now)); !outbox.IsPermanent(err) {
		t.Fatalf("no days: %v", err)
	}
	if err := f.handle(expiryEvent(listings.EventListingExpired, uuid.New(), 0, expires, now)); !outbox.IsPermanent(err) {
		t.Fatalf("unknown listing: %v", err)
	}
}

// LISTING_EXPIRY is not essential: with its email switched off the
// advertiser gets the in-app notice only.
func TestListingExpiryFollowsThePreference(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	f.withListings(now)
	_, svc := f.withInbox()
	agent := agentID(t, f)
	off := false
	if _, err := svc.SetPreferences(context.Background(), agent, []inapp.PreferenceItem{{Type: inapp.TypeListingExpiry, Email: &off}}, inapp.SourceWeb); err != nil {
		t.Fatal(err)
	}
	if err := f.handle(expiryEvent(listings.EventListingExpiring, propertyrequests.SeedListingID(1), 14, now.AddDate(0, 0, 14), now)); err != nil {
		t.Fatal(err)
	}
	if err := f.handle(expiryEvent(listings.EventListingExpired, propertyrequests.SeedListingID(1), 0, now, now)); err != nil {
		t.Fatal(err)
	}
	if n := len(f.sender.byKind("listing-expiring")) + len(f.sender.byKind("listing-expired")); n != 0 {
		t.Fatalf("email switched off, %d sent", n)
	}
	items := notificationsOf(t, svc, agent)
	titles := map[string]bool{}
	for _, n := range items {
		titles[n.Title] = true
	}
	if len(items) != 2 || !titles["Your listing has expired"] || !titles["Your listing expires in 14 days"] {
		t.Fatalf("in-app: %+v", items)
	}
	if info, ok := inapp.Lookup(inapp.TypeListingExpiry); !ok || info.Essential || info.Label != "Listing expiry reminders" {
		t.Fatalf("catalogue: %+v", info)
	}
}

// The approval of a relist sends listing-relisted, not listing-approved.
func TestRelistApprovalSendsListingRelisted(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	f.withListings(now)
	_, svc := f.withInbox()
	id := propertyrequests.SeedListingID(1)
	raw, _ := json.Marshal(listings.ReviewEventPayload{ListingID: id, ReviewKind: listings.ReviewKindNew, Relist: true, OccurredAt: now})
	ev := outbox.Event{ID: uuid.New(), Type: listings.EventListingApproved, AggregateType: listings.AggregateListing, AggregateID: id, Payload: raw, CreatedAt: now}
	for range 2 {
		if err := f.handle(ev); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.sender.byKind("listing-approved")); n != 0 {
		t.Fatalf("%d listing-approved for a relist", n)
	}
	sent := f.sender.byKind("listing-relisted")
	if recipients(sent) != "agent@example.test" || sent[0].Subject != "Your listing is shown to customers again" ||
		!strings.Contains(sent[0].Text, "Shown until: ") {
		t.Fatalf("relisted: %v %+v", recipients(sent), sent)
	}
	items := notificationsOf(t, svc, agentID(t, f))
	if len(items) != 1 || items[0].Type != inapp.TypeListingReviewed || items[0].Title != "Your listing is live again" {
		t.Fatalf("in-app: %+v", items)
	}
}
