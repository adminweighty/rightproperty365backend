package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/notifications"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/savedsearches"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/mail"
)

type savedSearchBody struct {
	SavedSearch struct {
		ID          string     `json:"id"`
		Name        string     `json:"name"`
		Query       string     `json:"query"`
		Frequency   string     `json:"frequency"`
		LastAlertAt *time.Time `json:"last_alert_at"`
	} `json:"saved_search"`
}

// API 0.52.0: saved searches, the alert sweep and the unsubscribe link.
func TestSavedSearches(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	consumer := identity.SeedConsumerEmail

	wantError(t, e.post("/v1/me/saved-searches", obj{"name": "x", "query": "deal=sale"}), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	f := wantError(t, e.post("/v1/me/saved-searches", obj{"name": "x", "query": "deal=nonsense"}, c.as(consumer)),
		http.StatusBadRequest, httpx.CodeValidationFailed).Error.Fields
	if f["query.deal"] == "" {
		t.Fatalf("fields: %v", f)
	}

	// Saved sixty days ago: everything listed since is new to it.
	now := e.now
	e.now = now.Add(-60 * 24 * time.Hour)
	rec := e.post("/v1/me/saved-searches", obj{"name": "Homes for sale", "query": "?deal=sale&page=3&sort=price_asc&bbox=1,2,3,4", "frequency": "WEEKLY"}, c.as(consumer))
	wantStatus(t, rec, http.StatusCreated)
	e.now = now
	ss := decode[savedSearchBody](t, rec).SavedSearch
	if ss.Query != "deal=sale" || ss.Frequency != "WEEKLY" || ss.LastAlertAt != nil {
		t.Fatalf("saved: %+v", ss)
	}
	rec = e.patch("/v1/me/saved-searches/"+ss.ID, obj{"name": "All homes for sale"}, c.as(consumer))
	wantStatus(t, rec, http.StatusOK)
	wantError(t, e.patch("/v1/me/saved-searches/"+ss.ID, obj{"name": "Mine"}, c.as(identity.SeedAgentEmail)), http.StatusNotFound, savedsearches.CodeNotFound)
	wantError(t, e.deleteReq("/v1/me/saved-searches/"+ss.ID, c.as(identity.SeedAgentEmail)), http.StatusNotFound, savedsearches.CodeNotFound)

	// The worker's run: new listings for sale since the watermark.
	sweeper := SavedSearchService(e.savedSearches, e.listings, e.listings, reference.NewService(e.reference, nil), e.marketStore,
		func() time.Time { return e.now })
	n, err := sweeper.Sweep(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if n, _ := sweeper.Sweep(context.Background()); n != 0 {
		t.Fatal("a second run at once found it due again")
	}
	events := e.savedSearches.Outbox()
	if len(events) != 1 || events[0].Type != savedsearches.EventMatched {
		t.Fatalf("events: %+v", events)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	renderer, err := mail.NewRenderer(testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	sender := &captureSender{}
	handlers := notifications.NewHandlers(mail.NewService(sender, mail.NewMemoryDeliveryLog(), logger), renderer,
		notifications.NewMemorySource(e.identity, e.requests, e.enquiries), logger)
	handlers.SetSavedSearchSource(e.savedSearches.Get)
	for range 2 { // run twice: sent once
		if err := handlers.Handle(context.Background(), events[0]); err != nil {
			t.Fatal(err)
		}
	}
	if len(sender.sent) != 1 || sender.sent[0].Kind != "saved-search-alert" || sender.sent[0].To != consumer {
		t.Fatalf("sent: %+v", sender.sent)
	}
	m := sender.sent[0]
	if !strings.Contains(m.Subject, "All homes for sale") || !strings.Contains(m.Text, "/saved-searches/unsubscribe?token=") ||
		!strings.Contains(m.Text, testOrigin+"/listings/") || !strings.Contains(m.HTML, "deal=sale") {
		t.Fatalf("email:\n%s\n%s", m.Subject, m.Text)
	}

	// The unsubscribe link switches it off; an alert written before that
	// sends nothing.
	stored, _ := e.savedSearches.Get(context.Background(), events[0].AggregateID)
	wantError(t, e.post("/v1/saved-searches/unsubscribe", obj{"token": "nope"}), http.StatusBadRequest, savedsearches.CodeInvalidToken)
	rec = e.post("/v1/saved-searches/unsubscribe", obj{"token": stored.UnsubscribeToken})
	wantStatus(t, rec, http.StatusOK)
	if got := decode[obj](t, rec); got["name"] != "All homes for sale" {
		t.Fatalf("unsubscribe: %v", got)
	}
	sender.sent = nil
	handlers2 := notifications.NewHandlers(mail.NewService(sender, mail.NewMemoryDeliveryLog(), logger), renderer,
		notifications.NewMemorySource(e.identity, e.requests, e.enquiries), logger)
	handlers2.SetSavedSearchSource(e.savedSearches.Get)
	if err := handlers2.Handle(context.Background(), events[0]); err != nil || len(sender.sent) != 0 {
		t.Fatalf("after unsubscribe: %v %d", err, len(sender.sent))
	}

	rec = e.get("/v1/me/saved-searches", c.as(consumer))
	wantStatus(t, rec, http.StatusOK)
	if s := decode[obj](t, rec)["items"].([]any)[0].(map[string]any); s["frequency"] != "OFF" || s["last_alert_at"] == nil {
		t.Fatalf("list: %v", s)
	}
	wantStatus(t, e.deleteReq("/v1/me/saved-searches/"+ss.ID, c.as(consumer)), http.StatusNoContent)
}
