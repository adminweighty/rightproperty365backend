package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// expiryBody is the expiry object of the managed listing and its list rows.
type expiryBody struct {
	ExpiresAt time.Time `json:"expires_at"`
	DaysLeft  int       `json:"days_left"`
	State     string    `json:"state"`
	RelistNo  int       `json:"relist_no"`
}

type expiryView struct {
	Listing struct {
		Status   string      `json:"status"`
		Public   bool        `json:"public"`
		Actions  []string    `json:"actions"`
		Expiry   *expiryBody `json:"expiry"`
		Timeline []struct {
			Action string `json:"action"`
			Actor  string `json:"actor"`
		} `json:"timeline"`
	} `json:"listing"`
}

type expiryListBody struct {
	Items []struct {
		ID     string      `json:"id"`
		Status string      `json:"status"`
		Expiry *expiryBody `json:"expiry"`
	} `json:"items"`
	Total int `json:"total"`
}

func (b expiryListBody) row(id string) (string, *expiryBody, bool) {
	for _, it := range b.Items {
		if it.ID == id {
			return it.Status, it.Expiry, true
		}
	}
	return "", nil, false
}

// expiryEvents returns the listing's LISTING_EXPIRING / LISTING_EXPIRED
// events as "14#1" (days and cycle) or "EXPIRED#1".
func (e *env) expiryEvents(id string) []string {
	e.t.Helper()
	var out []string
	for _, ev := range e.listings.Outbox() {
		if ev.AggregateID.String() != id || (ev.Type != listings.EventListingExpiring && ev.Type != listings.EventListingExpired) {
			continue
		}
		var p listings.ExpiryEventPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			e.t.Fatal(err)
		}
		s := "EXPIRED"
		if ev.Type == listings.EventListingExpiring {
			s = map[int]string{14: "14", 7: "7", 1: "1"}[p.Days]
		}
		out = append(out, s+"#"+string(rune('0'+p.RelistNo)))
	}
	return out
}

// API 0.45.0 end to end: publish, let the clock run, sweep; the reminders,
// the expiry, the views and the list filter; a relist and its new cycle.
func TestListingExpiryEndToEnd(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	admin := e.admin()
	sweep := func() {
		t.Helper()
		if _, err := listings.SweepExpiry(context.Background(), e.listings, e.now); err != nil {
			t.Fatal(err)
		}
	}
	view := func(id string) expiryView {
		t.Helper()
		rec := e.get("/v1/me/listings/"+id, tendai)
		wantStatus(t, rec, http.StatusOK)
		return decode[expiryView](t, rec)
	}
	wantEvents := func(id string, want ...string) {
		t.Helper()
		got := e.expiryEvents(id)
		if len(got) != len(want) {
			t.Fatalf("expiry events %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("expiry events %v, want %v", got, want)
			}
		}
	}

	// A draft has no expiry.
	l := e.readyListing(tendai, "Expiring family house in Borrowdale")
	id := l.ID
	if v := view(id); v.Listing.Expiry != nil {
		t.Fatalf("draft expiry: %+v", v.Listing.Expiry)
	}
	managedOf(t, e.post("/v1/me/listings/"+id+"/submit", nil, tendai), http.StatusOK)
	managedOf(t, e.post("/v1/admin/listings/"+id+"/approve", nil, browser(admin)), http.StatusOK)
	expires := e.now.Add(listings.PublicationPeriod)
	v := view(id)
	if x := v.Listing.Expiry; x == nil || !x.ExpiresAt.Equal(expires) || x.DaysLeft != 90 || x.State != "ACTIVE" || x.RelistNo != 1 {
		t.Fatalf("published: %+v", x)
	}
	sweep()
	wantEvents(id)

	// Six days before the end: EXPIRING_SOON, and the 7-day reminder once.
	e.now = expires.Add(-6 * 24 * time.Hour)
	tendai, admin = newCast(e).as(identity.SeedAgentEmail), e.admin()
	sweep()
	sweep()
	wantEvents(id, "7#1")
	if x := view(id).Listing.Expiry; x.State != "EXPIRING_SOON" || x.DaysLeft != 6 {
		t.Fatalf("expiring: %+v", x)
	}
	list := decode[expiryListBody](t, e.get("/v1/me/listings?expiry=EXPIRING_SOON&page_size=48", tendai))
	if status, x, ok := list.row(id); !ok || status != "PUBLISHED" || x == nil || x.State != "EXPIRING_SOON" || x.DaysLeft != 6 {
		t.Fatalf("expiring row: %v %+v %v", status, x, ok)
	}
	for _, it := range list.Items {
		if it.Expiry == nil || it.Expiry.State != "EXPIRING_SOON" {
			t.Fatalf("filter let through %+v", it)
		}
	}
	env := wantError(t, e.get("/v1/me/listings?expiry=SOON", tendai), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["expiry"] == "" {
		t.Fatalf("expiry filter: %v", env.Error.Fields)
	}

	// Past the end: EXPIRED at once (the read rule), stored by the sweep.
	e.now = expires.Add(time.Hour)
	tendai, admin = newCast(e).as(identity.SeedAgentEmail), e.admin()
	v = view(id)
	if v.Listing.Status != "EXPIRED" || v.Listing.Public {
		t.Fatalf("read rule: %s %v", v.Listing.Status, v.Listing.Public)
	}
	if status, _, _ := decode[expiryListBody](t, e.get("/v1/me/listings?status=EXPIRED&page_size=48", tendai)).row(id); status != "EXPIRED" {
		t.Fatalf("list before the sweep: %q", status)
	}
	sweep()
	sweep()
	wantEvents(id, "7#1", "EXPIRED#1")
	got, err := e.listings.Managed(context.Background(), uuid.MustParse(id))
	if err != nil || got.Status != listings.StatusExpired {
		t.Fatalf("stored: %s %v", got.Status, err)
	}
	v = view(id)
	last := v.Listing.Timeline[len(v.Listing.Timeline)-1]
	if x := v.Listing.Expiry; v.Listing.Status != "EXPIRED" || x.State != "EXPIRED" || x.DaysLeft != 0 || x.RelistNo != 1 ||
		!contains(v.Listing.Actions, "relist") || last.Action != "EXPIRED" || last.Actor != "SYSTEM" {
		t.Fatalf("expired: %+v", v.Listing)
	}
	list = decode[expiryListBody](t, e.get("/v1/me/listings?expiry=EXPIRED&status=EXPIRED&page_size=48", tendai))
	if _, x, ok := list.row(id); !ok || x.State != "EXPIRED" {
		t.Fatalf("expired row: %+v %v", x, ok)
	}
	wantError(t, e.get("/v1/listings/"+l.Slug), http.StatusGone, listings.CodeListingUnavailable)
	adminView := decode[expiryView](t, e.get("/v1/admin/listings/"+id, withCookie(admin)))
	if x := adminView.Listing.Expiry; x == nil || x.State != "EXPIRED" {
		t.Fatalf("admin view: %+v", x)
	}

	// Relist: in review it still shows the last cycle, EXPIRED.
	managedOf(t, e.post("/v1/me/listings/"+id+"/relist", nil, tendai), http.StatusOK)
	if x := view(id).Listing.Expiry; x.State != "EXPIRED" || x.RelistNo != 1 {
		t.Fatalf("relist in review: %+v", x)
	}
	approved := managedOf(t, e.post("/v1/admin/listings/"+id+"/approve", nil, browser(admin)), http.StatusOK)
	if approved.Status != "PUBLISHED" {
		t.Fatalf("relist approved: %s", approved.Status)
	}
	var relistFlags []bool
	for _, ev := range e.listings.Outbox() {
		if ev.AggregateID.String() == id && ev.Type == listings.EventListingApproved {
			var p listings.ReviewEventPayload
			_ = json.Unmarshal(ev.Payload, &p)
			relistFlags = append(relistFlags, p.Relist)
		}
	}
	if len(relistFlags) != 2 || relistFlags[0] || !relistFlags[1] {
		t.Fatalf("relist flags of the approvals: %v", relistFlags)
	}
	second := e.now.Add(listings.PublicationPeriod)
	if x := view(id).Listing.Expiry; x.State != "ACTIVE" || x.RelistNo != 2 || !x.ExpiresAt.Equal(second) {
		t.Fatalf("second cycle: %+v", x)
	}

	// The new cycle gets its own reminders.
	e.now = second.Add(-13 * 24 * time.Hour)
	tendai = newCast(e).as(identity.SeedAgentEmail)
	sweep()
	wantEvents(id, "7#1", "EXPIRED#1", "14#2")
	if x := view(id).Listing.Expiry; x.State != "EXPIRING_SOON" || x.DaysLeft != 13 || x.RelistNo != 2 {
		t.Fatalf("second cycle expiring: %+v", x)
	}
}
