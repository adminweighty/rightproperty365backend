package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/savedsearches"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Saved search alerts (API 0.52.0): the customer gets saved-search-alert
// (and the in-app notice) with the new listings the worker found, a link to
// all results and a one-click unsubscribe link for that search. The search
// is read at send time: one switched off meanwhile sends nothing.

// PathSavedSearches is the customer's saved searches page.
const PathSavedSearches = "/account/saved-searches"

// SetSavedSearchSource supplies the saved search behind an alert. Without it
// those events are dead-lettered.
func (h *Handlers) SetSavedSearchSource(source func(ctx context.Context, id uuid.UUID) (savedsearches.SavedSearch, error)) {
	h.savedSearch = source
}

func (h *Handlers) savedSearchNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	if h.savedSearch == nil {
		return outbox.Permanent(fmt.Errorf("notifications: no saved search source for event type %q", ev.Type))
	}
	var p savedsearches.EventPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload: %w", ev.Type, err))
	}
	ss, err := h.savedSearch(ctx, ev.AggregateID)
	if errors.Is(err, savedsearches.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if ss.Frequency == savedsearches.FrequencyOff || len(p.Lines) == 0 {
		return nil
	}
	people, err := h.source.Recipients(ctx, &ss.UserID, nil)
	if err != nil {
		return err
	}
	query, _ := url.ParseQuery(ss.Query)
	lines := make([]mail.MatchLine, 0, len(p.Lines))
	for _, l := range p.Lines {
		lines = append(lines, mail.MatchLine{Title: l.Title, Place: l.Place, Price: l.Price, URL: h.renderer.Link("/listings/"+url.PathEscape(l.Slug), nil)})
	}
	for _, person := range people {
		d := mail.Data{Name: person.Name, Path: "/listings", Query: query, WithName: ss.Name, When: occurredAt(ev),
			Matches: lines, MatchTotal: p.Total, MatchMore: p.More,
			UnsubscribeURL: h.renderer.Link("/saved-searches/unsubscribe", url.Values{"token": {ss.UnsubscribeToken}})}
		if err := add("saved-search-alert", "customer", person, d); err != nil {
			return err
		}
	}
	return nil
}
