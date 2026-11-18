package saved

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/auth"
)

// fakeCatalogue answers CardStates from a map the test changes.
type fakeCatalogue struct {
	mu     sync.Mutex
	states map[uuid.UUID]listings.CardState
}

func (c *fakeCatalogue) CardStates(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]listings.CardState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[uuid.UUID]listings.CardState{}
	for _, id := range ids {
		if st, ok := c.states[id]; ok {
			out[id] = st
		}
	}
	return out, nil
}

func (c *fakeCatalogue) set(id uuid.UUID, st listings.CardState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states[id] = st
}

func card(id uuid.UUID, slug, title string) listings.Listing {
	city, period := "harare", "month"
	return listings.Listing{ID: id, Slug: slug, Reference: "RP-ZW-" + slug, Title: title,
		Category: listings.Coded{Code: "residential", Label: "Residential"}, Price: listings.Money{Amount: "850.00", Currency: "USD"},
		PricePeriod: &period, City: listings.Place{Code: &city, Label: "Harare"}, Country: listings.Country{Code: "ZW", Name: "Zimbabwe"},
		Photos: []listings.Photo{{URL: "/assets/images/x.jpg", Alt: "x", Width: 10, Height: 10}}}
}

func public(l listings.Listing) listings.CardState {
	return listings.CardState{Public: true, Status: listings.StatusPublished, Listing: l}
}

type fixture struct {
	svc   *Service
	store *MemoryStore
	cat   *fakeCatalogue
	now   time.Time
}

func newFixture() *fixture {
	f := &fixture{store: NewMemoryStore(), cat: &fakeCatalogue{states: map[uuid.UUID]listings.CardState{}},
		now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	f.svc = NewService(f.store, f.cat, func() time.Time { return f.now })
	return f
}

func customerP() *auth.Principal {
	return &auth.Principal{UserID: uuid.New(), SessionScope: auth.ScopeCustomer}
}

func TestSaveIsIdempotentAndKeepsSavedAt(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	alice := customerP()
	id := uuid.New()
	f.cat.set(id, public(card(id, "flat", "Flat")))

	first, err := f.svc.Save(ctx, alice, id)
	if err != nil || !first.Saved || !first.Created || !first.SavedAt.Equal(f.now) {
		t.Fatalf("first save: %+v %v", first, err)
	}
	f.now = f.now.Add(time.Hour)
	again, err := f.svc.Save(ctx, alice, id)
	if err != nil || !again.Saved || again.Created || !again.SavedAt.Equal(first.SavedAt) {
		t.Fatalf("second save must keep the first saved_at and report created=false: %+v %v", again, err)
	}
	if evs := f.store.Outbox(); len(evs) != 1 || evs[0].Type != EventListingSaved || evs[0].AggregateType != AggregateListing || evs[0].AggregateID != id {
		t.Fatalf("one LISTING_SAVED event only: %+v", evs)
	}
	var payload map[string]string
	if err := json.Unmarshal(f.store.Outbox()[0].Payload, &payload); err != nil ||
		payload["user_id"] != alice.UserID.String() || payload["listing_id"] != id.String() || payload["at"] == "" || len(payload) != 3 {
		t.Fatalf("payload: %v %v", payload, err)
	}

	// Remove twice: one LISTING_UNSAVED, no error either time.
	for i := 0; i < 2; i++ {
		if err := f.svc.Remove(ctx, alice, id); err != nil {
			t.Fatalf("remove %d: %v", i, err)
		}
	}
	if err := f.svc.Remove(ctx, alice, uuid.New()); err != nil {
		t.Fatalf("removing an unknown id: %v", err)
	}
	evs := f.store.Outbox()
	if len(evs) != 2 || evs[1].Type != EventListingUnsaved {
		t.Fatalf("one LISTING_UNSAVED event only: %+v", evs)
	}
	page, _ := f.svc.List(ctx, alice, 1, 24)
	if page.Total != 0 || len(page.Items) != 0 || page.TotalPages != 0 {
		t.Fatalf("after removal: %+v", page)
	}
	// Saving again after a removal is a new save.
	if res, err := f.svc.Save(ctx, alice, id); err != nil || !res.Created || !res.SavedAt.Equal(f.now) {
		t.Fatalf("save after remove: %+v %v", res, err)
	}
}

func TestOnlyPublicListingsCanBeSaved(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	alice := customerP()
	expired, unknown := uuid.New(), uuid.New()
	f.cat.set(expired, listings.CardState{Public: false, Status: listings.StatusExpired, Listing: card(expired, "old", "Old")})
	for _, id := range []uuid.UUID{expired, unknown} {
		if _, err := f.svc.Save(ctx, alice, id); !errors.Is(err, ErrListingNotFound) {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	if len(f.store.Outbox()) != 0 {
		t.Fatal("no event for a refused save")
	}
}

func TestUnavailableAfterExpiry(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	alice := customerP()
	id := uuid.New()
	f.cat.set(id, public(card(id, "flat", "Flat")))
	if _, err := f.svc.Save(ctx, alice, id); err != nil {
		t.Fatal(err)
	}
	// Seen public again with a new title: the snapshot follows.
	f.cat.set(id, public(card(id, "flat", "Flat, renovated")))
	page, err := f.svc.List(ctx, alice, 1, 24)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("list: %+v %v", page, err)
	}
	it := page.Items[0]
	if !it.Available || it.UnavailableReason != nil || it.UnavailableLabel != nil || it.Listing.Slug == nil || *it.Listing.Slug != "flat" ||
		it.Listing.Title != "Flat, renovated" || it.Listing.Photo == nil {
		t.Fatalf("available item: %+v", it)
	}

	// The listing expires and its card changes again: the customer sees the
	// last public snapshot, without a slug.
	gone := card(id, "flat", "Changed after expiry")
	f.cat.set(id, listings.CardState{Public: false, Status: listings.StatusExpired, Listing: gone})
	page, _ = f.svc.List(ctx, alice, 1, 24)
	it = page.Items[0]
	if it.Available || it.UnavailableReason == nil || *it.UnavailableReason != ReasonExpired || *it.UnavailableLabel != "Listing expired" ||
		it.Listing.Slug != nil || it.Listing.Title != "Flat, renovated" || it.Listing.Reference != "RP-ZW-flat" {
		t.Fatalf("unavailable item: %+v", it)
	}
	raw, _ := json.Marshal(it)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if l := m["listing"].(map[string]any); l["slug"] != nil {
		t.Fatalf("slug must be null: %s", raw)
	}
	// Removing still works when the listing is not public.
	if err := f.svc.Remove(ctx, alice, id); err != nil {
		t.Fatal(err)
	}
}

func TestReasons(t *testing.T) {
	for status, want := range map[string]string{
		"EXPIRED": ReasonExpired, "LET": ReasonLet, "SOLD": ReasonSold, "ARCHIVED": ReasonWithdrawn, "CLOSED": ReasonWithdrawn,
		"PAUSED": ReasonWithdrawn, "PENDING_REVIEW": ReasonWithdrawn, "PUBLISHED": ReasonWithdrawn,
	} {
		if got := ReasonFor(status); got != want {
			t.Fatalf("%s: %s, want %s", status, got, want)
		}
	}
	for _, r := range Reasons() {
		if ReasonLabel(r) == "" {
			t.Fatalf("no label for %s", r)
		}
	}
}

func TestIDsArePublicOnlyNewestFirst(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	alice := customerP()
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{a, b, c} {
		f.cat.set(id, public(card(id, id.String()[:8], "x")))
		if _, err := f.svc.Save(ctx, alice, id); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(time.Minute)
	}
	f.cat.set(b, listings.CardState{Public: false, Status: "SOLD"})
	ids, err := f.svc.IDs(ctx, alice)
	if err != nil || len(ids) != 2 || ids[0] != c || ids[1] != a {
		t.Fatalf("ids: %v %v", ids, err)
	}
	page, _ := f.svc.List(ctx, alice, 1, 2)
	if page.Total != 3 || page.TotalPages != 2 || len(page.Items) != 2 || page.Items[0].ListingID != c ||
		page.Items[1].ListingID != b || *page.Items[1].UnavailableReason != ReasonSold {
		t.Fatalf("page 1: %+v", page)
	}
	page, _ = f.svc.List(ctx, alice, 2, 2)
	if len(page.Items) != 1 || page.Items[0].ListingID != a {
		t.Fatalf("page 2: %+v", page)
	}
}

func TestWhoMaySave(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	id := uuid.New()
	f.cat.set(id, public(card(id, "flat", "Flat")))
	admin := &auth.Principal{UserID: uuid.New(), SessionScope: auth.ScopeAdmin, PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformSuperAdmin}}}
	if _, err := f.svc.Save(ctx, admin, id); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin save: %v", err)
	}
	if _, err := f.svc.List(ctx, admin, 1, 24); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin list: %v", err)
	}
	if _, err := f.svc.IDs(ctx, admin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin ids: %v", err)
	}
	if err := f.svc.Remove(ctx, admin, id); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin remove: %v", err)
	}
	if _, err := f.svc.Save(ctx, nil, id); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous save: %v", err)
	}
	if _, err := f.svc.List(ctx, nil, 1, 24); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous list: %v", err)
	}
	// An agent (customer-scope session with a membership) may save.
	agent := &auth.Principal{UserID: uuid.New(), SessionScope: auth.ScopeCustomer,
		Memberships: []auth.Membership{{OrganisationID: uuid.New(), Role: auth.RoleAgent}}}
	if _, err := f.svc.Save(ctx, agent, id); err != nil {
		t.Fatalf("agent save: %v", err)
	}
}

// One customer never sees, nor removes, another's saved listings.
func TestCrossUser(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	alice, bob := customerP(), customerP()
	id, other := uuid.New(), uuid.New()
	f.cat.set(id, public(card(id, "flat", "Flat")))
	f.cat.set(other, public(card(other, "house", "House")))
	if _, err := f.svc.Save(ctx, alice, id); err != nil {
		t.Fatal(err)
	}
	if page, _ := f.svc.List(ctx, bob, 1, 24); page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("bob sees alice's saves: %+v", page)
	}
	if ids, _ := f.svc.IDs(ctx, bob); len(ids) != 0 {
		t.Fatalf("bob's ids: %v", ids)
	}
	// Bob saving the same listing is his own save; removing his does not touch alice's.
	f.now = f.now.Add(time.Hour)
	if res, err := f.svc.Save(ctx, bob, id); err != nil || !res.Created || !res.SavedAt.Equal(f.now) {
		t.Fatalf("bob's save: %+v %v", res, err)
	}
	if _, err := f.svc.Save(ctx, bob, other); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Remove(ctx, bob, id); err != nil {
		t.Fatal(err)
	}
	page, _ := f.svc.List(ctx, alice, 1, 24)
	if page.Total != 1 || page.Items[0].ListingID != id {
		t.Fatalf("alice's saves after bob's removal: %+v", page)
	}
	if ids, _ := f.svc.IDs(ctx, alice); len(ids) != 1 || ids[0] != id {
		t.Fatalf("alice's ids: %v", ids)
	}
	f.store.DeleteUser(alice.UserID)
	if page, _ := f.svc.List(ctx, bob, 1, 24); page.Total != 1 || page.Items[0].ListingID != other {
		t.Fatalf("deleting alice's saves left bob's: %+v", page)
	}
}
