package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/reviews"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// A moderation tells the reviewer the outcome and, on a first publication
// only, the provider's owners; never the moderator; safe text only (the
// provider's name and the outcome, never the review, the reason or names).
func TestReviewNotifications(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	plumbers := devseed.PlumbersProfile()
	review := uuid.New()
	reviewer := identity.SeedConsumerUserID
	status := reviews.StatusPublished
	f.handlers.SetReviewSource(func(_ context.Context, id uuid.UUID) (reviews.NotificationInfo, error) {
		if id != review {
			return reviews.NotificationInfo{}, reviews.ErrNotFound
		}
		return reviews.NotificationInfo{ReviewID: review, Status: status, ProviderName: plumbers.Name,
			ProviderOrganisationID: plumbers.Organisation.ID, ReviewerUserID: &reviewer, ReviewerKind: reviews.ReviewerCustomer}, nil
	})
	moderator := devseed.ModeratorID
	event := func(action string, first bool) outbox.Event {
		raw, _ := json.Marshal(reviews.EventPayload{ReviewID: review, Action: action, Status: status, FirstPublish: first,
			ActorUserID: &moderator, OccurredAt: now})
		return outbox.Event{ID: uuid.New(), Type: reviews.OutboxModerated, AggregateType: reviews.AggregateReview, AggregateID: review,
			Payload: raw, CreatedAt: now}
	}

	if err := f.handle(event(reviews.ActionPublish, true)); err != nil {
		t.Fatal(err)
	}
	toReviewer, toProvider := f.sender.byKind("review-moderated"), f.sender.byKind("review-published")
	if len(toReviewer) != 1 || toReviewer[0].To != identity.SeedConsumerEmail || len(toProvider) != 1 ||
		toProvider[0].To != devseed.BusinessOwnerEmail(plumbers.Slug) {
		t.Fatalf("first publication: reviewer %+v provider %+v", toReviewer, toProvider)
	}
	for _, m := range append(toReviewer, toProvider...) {
		if !strings.Contains(m.Text, plumbers.Name) || strings.Contains(m.Text, "Casey C.") || strings.Contains(m.Text, "Morgan") {
			t.Fatalf("unsafe or incomplete text:\n%s", m.Text)
		}
	}
	if !strings.Contains(toReviewer[0].Text, "/account/reviews") || !strings.Contains(toProvider[0].Text, "/agent/reviews") {
		t.Fatalf("links: %s / %s", toReviewer[0].Text, toProvider[0].Text)
	}

	// Hidden: the reviewer only.
	status = reviews.StatusHidden
	if err := f.handle(event(reviews.ActionHide, false)); err != nil {
		t.Fatal(err)
	}
	if got := f.sender.byKind("review-moderated"); len(got) != 2 || !strings.Contains(got[1].Text, "Hidden") ||
		len(f.sender.byKind("review-published")) != 1 {
		t.Fatalf("after hide: %d reviewer emails, %d provider emails", len(got), len(f.sender.byKind("review-published")))
	}
	// Published again (not the first time): no news to the provider.
	status = reviews.StatusPublished
	if err := f.handle(event(reviews.ActionPublish, false)); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.byKind("review-published")) != 1 || len(f.sender.byKind("review-moderated")) != 3 {
		t.Fatal("a second publication told the provider again")
	}
	// A review deleted since tells nobody.
	review2 := event(reviews.ActionReject, false)
	review2.AggregateID = uuid.New()
	before := f.sender.count()
	if err := f.handle(review2); err != nil || f.sender.count() != before {
		t.Fatalf("deleted review: err %v, %d emails", err, f.sender.count()-before)
	}
}
