package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/reviews"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Where the review emails send the reviewer and the provider.
const (
	// PathMyReviews is the customer's reviews page.
	PathMyReviews = "/account/reviews"
	// PathAgentReviews is the organisation's reviews page (written and received).
	PathAgentReviews = "/agent/reviews"
)

// SetReviewSource supplies the data behind review events (reviews.Notices).
// Without it those events are dead-lettered.
func (h *Handlers) SetReviewSource(source func(ctx context.Context, id uuid.UUID) (reviews.NotificationInfo, error)) {
	h.review = source
}

// reviewStatusText is the outcome as the reviewer reads it.
func reviewStatusText(status string) string {
	switch status {
	case reviews.StatusPublished:
		return "Published"
	case reviews.StatusHidden:
		return "Hidden"
	case reviews.StatusRejected:
		return "Not published"
	}
	return reviews.StatusLabel(status)
}

// reviewNotes builds the emails of one moderation: the outcome to the
// reviewer (review-moderated) and, on a first publication, the news to the
// provider organisation's owners and managers (review-published); never to
// the moderator who acted. Safe text only: the provider's public name and
// the outcome; never the review's text, the reviewer's name, the reason or
// a reply.
func (h *Handlers) reviewNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	if h.review == nil {
		return outbox.Permanent(fmt.Errorf("notifications: no review source for event type %q", ev.Type))
	}
	var payload reviews.EventPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload: %w", ev.Type, err))
	}
	n, err := h.review(ctx, ev.AggregateID)
	if errors.Is(err, reviews.ErrNotFound) {
		return nil // deleted by its reviewer since: nothing to tell
	}
	if err != nil {
		return err
	}
	actor := func(id *uuid.UUID) bool {
		return id != nil && payload.ActorUserID != nil && *id == *payload.ActorUserID
	}
	d := mail.Data{ProfileName: n.ProviderName, StatusText: reviewStatusText(payload.Status), When: occurredAt(ev)}
	if n.ReviewerUserID != nil && !actor(n.ReviewerUserID) {
		u, err := h.source.User(ctx, *n.ReviewerUserID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return err
		default:
			rd := d
			rd.Name, rd.Path = u.Name, PathMyReviews
			if n.ReviewerKind != reviews.ReviewerCustomer {
				rd.Path = PathAgentReviews
			}
			if err := add("review-moderated", recipientKey(u.Person), u.Person, rd); err != nil {
				return err
			}
		}
	}
	if !payload.FirstPublish || payload.Status != reviews.StatusPublished {
		return nil
	}
	org := n.ProviderOrganisationID
	managers, err := h.source.Recipients(ctx, nil, &org)
	if err != nil {
		return err
	}
	seen := map[uuid.UUID]bool{}
	for _, m := range managers {
		if actor(m.UserID) || (m.UserID != nil && (seen[*m.UserID] || *m.UserID == derefOr(n.ReviewerUserID))) {
			continue
		}
		if m.UserID != nil {
			seen[*m.UserID] = true
		}
		md := d
		md.Name, md.Path = m.Name, PathAgentReviews
		if err := add("review-published", recipientKey(m), m, md); err != nil {
			return err
		}
	}
	return nil
}

func derefOr(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}
