package venues

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
)

type fakeListings struct{ m map[uuid.UUID]ListingInfo }

func (f fakeListings) Listing(_ context.Context, id uuid.UUID) (ListingInfo, error) {
	l, ok := f.m[id]
	if !ok {
		return ListingInfo{}, ErrListingNotFound
	}
	return l, nil
}

func (f fakeListings) AgentListingIDs(_ context.Context, org, agent uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, l := range f.m {
		if l.OrganisationID != nil && *l.OrganisationID == org && l.AgentUserID != nil && *l.AgentUserID == agent {
			out = append(out, l.ID)
		}
	}
	return out, nil
}

type fakeMarkets struct{}

func (fakeMarkets) Market(_ context.Context, code string) (string, string, error) {
	switch code {
	case "GB":
		return "Europe/London", "GBP", nil
	case "ZW":
		return "Africa/Harare", "USD", nil
	}
	return "", "", nil
}

// fixture: one host organisation with an OWNER and an AGENT, a showroom
// (the agent's) and an office of another type, two customers.
type fixture struct {
	svc                 *Service
	store               Store
	now                 time.Time
	org                 uuid.UUID
	showroom, house     uuid.UUID
	owner, agent, staff *auth.Principal
	alice, bob          *auth.Principal
}

func member(org uuid.UUID, role, name string) *auth.Principal {
	return &auth.Principal{UserID: uuid.New(), Name: name, Email: name + "@example.test",
		Memberships: []auth.Membership{{OrganisationID: org, OrganisationKind: auth.KindHost, Role: role, MarketCode: "ZW"}}}
}

func newFixture(t *testing.T, store Store, ids ...uuid.UUID) *fixture {
	t.Helper()
	f := &fixture{store: store, now: time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)} // a Monday
	f.org, f.showroom, f.house = uuid.New(), uuid.New(), uuid.New()
	if len(ids) >= 5 {
		f.org, f.showroom, f.house = ids[0], ids[1], ids[2]
	}
	f.owner, f.agent, f.staff = member(f.org, auth.RoleOwner, "owner"), member(f.org, auth.RoleAgent, "agent"), member(f.org, auth.RoleStaff, "staff")
	f.alice, f.bob = &auth.Principal{UserID: uuid.New(), Name: "Alice", Email: "alice@example.test"}, &auth.Principal{UserID: uuid.New(), Name: "Bob", Email: "bob@example.test"}
	if len(ids) >= 5 {
		f.alice.UserID, f.bob.UserID = ids[3], ids[4]
	}
	// The Postgres fixture also passes the members' IDs: they are users there
	// (venue_settings.updated_by and the decision columns reference users).
	if len(ids) == 8 {
		f.owner.UserID, f.agent.UserID, f.staff.UserID = ids[5], ids[6], ids[7]
	}
	agent := f.agent.UserID
	org := f.org
	listings := fakeListings{m: map[uuid.UUID]ListingInfo{
		f.showroom: {ID: f.showroom, Slug: "showroom", Title: "Showroom", PropertyType: "showroom", MarketCode: "ZW", OrganisationID: &org, AgentUserID: &agent, Public: true},
		f.house:    {ID: f.house, Slug: "house", Title: "House", PropertyType: "house", MarketCode: "ZW", OrganisationID: &org, AgentUserID: &agent, Public: true},
	}}
	f.svc = NewService(Deps{Store: store, Listings: listings, Markets: fakeMarkets{},
		Hourly: func(context.Context) (map[string]bool, error) { return map[string]bool{"showroom": true}, nil },
		Now:    func() time.Time { return f.now }})
	in := SettingsInput{Enabled: ptr(true), Currency: strp("USD"), RatePerHour: strp("50"), IncrementMinutes: ptr(30), MinMinutes: ptr(120),
		MaxMinutes: ptr(480), CleaningBufferMinutes: ptr(30), Deposit: strp("100"), CancellationPolicy: strp("Free up to 48 hours before."),
		FreeCancellationHours: ptr(48), LeadTimeHours: ptr(24),
		Fees: []Fee{{Code: "TAX", Label: "VAT 15%", Kind: KindPercent, Amount: "15", EffectiveFrom: "2026-01-01"}}}
	for wd := 1; wd <= 6; wd++ {
		in.OpeningHours = append(in.OpeningHours, OpeningRange{Weekday: wd, Opens: "08:00", Closes: "18:00"})
	}
	if _, err := f.svc.PutSettings(context.Background(), f.owner, f.showroom, in); err != nil {
		t.Fatal(err)
	}
	return f
}

func ptr[T any](v T) *T { return &v }

// slot is a local (Harare, UTC+2) time on 9 November 2026, a Monday a week ahead.
func slot(hour, minutes int) string {
	return time.Date(2026, 11, 9, hour-2, 0, 0, 0, time.UTC).Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339)
}

func (f *fixture) request(t *testing.T, who *auth.Principal, startHour, hours int) Booking {
	t.Helper()
	q, err := f.svc.CreateQuote(context.Background(), f.showroom, QuoteInput{Start: slot(startHour, 0), End: slot(startHour+hours, 0)})
	if err != nil {
		t.Fatal(err)
	}
	b, replayed, err := f.svc.Request(context.Background(), who, RequestInput{QuoteID: q.ID.String(), Consent: &Consent{PrivacyVersion: "v1", Accepted: true}})
	if err != nil || replayed || b.Status != StatusRequested {
		t.Fatalf("request %+v %v %v", b, replayed, err)
	}
	return b
}

// FR-024 acceptance on a store: two overlapping requests confirmed at the
// same moment from two goroutines; exactly one wins, the other gets
// ErrSlotTaken.
func concurrentConfirm(t *testing.T, f *fixture) {
	t.Helper()
	a := f.request(t, f.alice, 10, 4) // 10:00-14:00
	b := f.request(t, f.bob, 12, 3)   // 12:00-15:00: overlaps, and both are REQUESTED
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, id := range []uuid.UUID{a.ID, b.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.svc.Confirm(context.Background(), f.owner, Host{OrganisationID: &f.org}, id)
		}()
	}
	close(start)
	wg.Wait()
	ok, taken := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrSlotTaken):
			taken++
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	if ok != 1 || taken != 1 {
		t.Fatalf("%d confirmed, %d refused: %v", ok, taken, errs)
	}
	// The buffer counts too: 14:00-16:00 starts inside the 30-minute cleaning time of 10:00-14:00.
	winner := a.ID
	if errs[0] != nil {
		winner = b.ID
	}
	won, _ := f.store.Booking(context.Background(), winner)
	if won.Status != StatusConfirmed || !won.Held {
		t.Fatalf("winner %+v", won)
	}
}

func TestMemoryConcurrentConfirm(t *testing.T) {
	for range 20 {
		concurrentConfirm(t, newFixture(t, NewMemoryStore()))
	}
}

func TestRequestRulesAndBuffers(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, NewMemoryStore())
	a := f.request(t, f.alice, 10, 4)
	if _, err := f.svc.Confirm(ctx, f.owner, Host{OrganisationID: &f.org}, a.ID); err != nil {
		t.Fatal(err)
	}
	// 14:00 is inside the cleaning buffer; 14:30 is free.
	_, err := f.svc.CreateQuote(ctx, f.showroom, QuoteInput{Start: slot(14, 0), End: slot(16, 0)})
	var un *UnavailableError
	if !errors.As(err, &un) || un.Reason != ReasonBuffer {
		t.Fatalf("buffer: %v", err)
	}
	if _, err := f.svc.CreateQuote(ctx, f.showroom, QuoteInput{Start: slot(14, 30), End: slot(16, 30)}); err != nil {
		t.Fatal(err)
	}
	// A quote is priced, in one currency, with the notice's mode.
	q, err := f.svc.CreateQuote(ctx, f.showroom, QuoteInput{Start: slot(15, 0), End: slot(17, 30)})
	if err != nil || q.Pricing.TotalExcludingDeposit != "143.75" || *q.Pricing.Deposit != "100.00" || !q.ExpiresAt.Equal(f.now.Add(30*time.Minute)) {
		t.Fatalf("quote %+v %v", q.Pricing, err)
	}
	// Idempotent per quote: the same customer gets the same booking, another gets 409.
	body := RequestInput{QuoteID: q.ID.String(), Consent: &Consent{PrivacyVersion: "v1", Accepted: true}}
	first, _, err := f.svc.Request(ctx, f.alice, body)
	if err != nil {
		t.Fatal(err)
	}
	again, replayed, err := f.svc.Request(ctx, f.alice, body)
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("replay %v %v", replayed, err)
	}
	if _, _, err := f.svc.Request(ctx, f.bob, body); !errors.Is(err, ErrQuoteUsed) {
		t.Fatalf("another customer's replay: %v", err)
	}
	// An expired quote.
	q2, _ := f.svc.CreateQuote(ctx, f.showroom, QuoteInput{Start: slot(8, 0), End: slot(9, 0)})
	if q2.ID != uuid.Nil {
		t.Fatal("a 1-hour quote passed the 2-hour minimum")
	}
	q3, err := f.svc.CreateQuote(ctx, f.showroom, QuoteInput{Start: slot(16, 0), End: slot(18, 0)})
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(31 * time.Minute)
	var exp *QuoteExpiredError
	if _, _, err := f.svc.Request(ctx, f.bob, RequestInput{QuoteID: q3.ID.String(), Consent: &Consent{PrivacyVersion: "v1", Accepted: true}}); !errors.As(err, &exp) {
		t.Fatalf("expired: %v", err)
	}
	// Not eligible: 400 on PUT, eligible=false on GET, 404 publicly.
	if _, err := f.svc.PutSettings(ctx, f.owner, f.house, SettingsInput{}); !errors.Is(err, ErrListingNotEligible) {
		t.Fatalf("not eligible: %v", err)
	}
	if v, err := f.svc.GetSettings(ctx, f.owner, f.house); err != nil || v.Eligible || v.Configured {
		t.Fatalf("GET not eligible %+v %v", v, err)
	}
	if _, err := f.svc.CreateQuote(ctx, f.house, QuoteInput{Start: slot(10, 0), End: slot(12, 0)}); !errors.Is(err, ErrVenueNotFound) {
		t.Fatalf("house quote: %v", err)
	}
	// STAFF and another organisation see nothing; the agent may edit their listing.
	if _, err := f.svc.GetSettings(ctx, f.staff, f.showroom); !errors.Is(err, ErrListingNotFound) {
		t.Fatalf("staff: %v", err)
	}
	if _, _, err := f.svc.HostList(ctx, f.staff, Host{OrganisationID: &f.org}, Filter{Page: 1, PageSize: 10}); !errors.Is(err, ErrOrganisationUnknown) {
		t.Fatalf("staff list: %v", err)
	}
	if list, total, err := f.svc.HostList(ctx, f.agent, Host{OrganisationID: &f.org}, Filter{Page: 1, PageSize: 10}); err != nil || total != 2 || len(list) != 2 {
		t.Fatalf("agent list %d %v", total, err)
	}
}

// The state machine, as a table: which actions each party has in each status.
func TestAllowedTable(t *testing.T) {
	start := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	before, during, after := start.Add(-time.Hour), start.Add(time.Hour), start.Add(5*time.Hour)
	b := func(status, proposedBy string) Booking {
		x := Booking{Status: status, Start: start, End: start.Add(4 * time.Hour), Held: status == StatusConfirmed}
		if proposedBy != "" {
			x.Proposal = &Proposal{By: proposedBy, Start: start.Add(48 * time.Hour), End: start.Add(52 * time.Hour)}
		}
		return x
	}
	cases := []struct {
		booking Booking
		party   string
		now     time.Time
		want    []string
	}{
		{b(StatusRequested, ""), PartyHost, before, []string{DoConfirm, DoDecline, DoPropose, DoCancel}},
		{b(StatusRequested, ""), PartyCustomer, before, []string{DoCancel}},
		{b(StatusRequested, ""), PartyHost, during, []string{DoDecline, DoPropose, DoCancel}},
		{b(StatusRequested, ""), PartyCustomer, during, []string{}},
		{b(StatusConfirmed, ""), PartyCustomer, before, []string{DoCancel, DoReschedule}},
		{b(StatusConfirmed, ""), PartyHost, before, []string{DoPropose, DoCancel}},
		{b(StatusConfirmed, ""), PartyHost, during, []string{DoPropose, DoCancel}},
		{b(StatusConfirmed, ""), PartyHost, after, []string{DoComplete, DoNoShow}},
		{b(StatusConfirmed, ""), PartyCustomer, after, []string{}},
		{b(StatusRescheduleRequested, PartyCustomer), PartyHost, before, []string{DoConfirm, DoPropose, DoCancel}},
		{b(StatusRescheduleRequested, PartyCustomer), PartyCustomer, before, []string{DoCancel}},
		{b(StatusRescheduleRequested, PartyHost), PartyCustomer, before, []string{DoCancel, DoReschedule, DoAccept}},
		{b(StatusRescheduleRequested, PartyHost), PartyHost, before, []string{DoCancel}},
		{b(StatusDeclined, ""), PartyHost, before, []string{}},
		{b(StatusCancelled, ""), PartyCustomer, before, []string{}},
		{b(StatusCompleted, ""), PartyHost, after, []string{}},
		{b(StatusNoShow, ""), PartyHost, after, []string{}},
	}
	for i, c := range cases {
		if got := Allowed(c.booking, c.party, c.now); !slices.Equal(got, c.want) {
			t.Fatalf("case %d %s/%s: %v, want %v", i, c.booking.Status, c.party, got, c.want)
		}
	}
}

func TestTransitionsEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, NewMemoryStore())
	org := Host{OrganisationID: &f.org}
	a := f.request(t, f.alice, 10, 2)
	// Decline needs a reason; a declined booking takes no further action.
	if _, err := f.svc.Decline(ctx, f.owner, org, a.ID, ""); err == nil {
		t.Fatal("decline without a reason")
	}
	d, err := f.svc.Decline(ctx, f.owner, org, a.ID, "Closed for painting")
	if err != nil || d.Status != StatusDeclined || d.DeclineReason != "Closed for painting" {
		t.Fatalf("decline %+v %v", d, err)
	}
	var trans *TransitionError
	if _, err := f.svc.Confirm(ctx, f.owner, org, a.ID); !errors.As(err, &trans) || trans.From != StatusDeclined || trans.Action != DoConfirm {
		t.Fatalf("confirm a declined booking: %v", err)
	}
	// Confirm, then the host proposes a new time, the customer accepts: re-locked at the new time.
	b := f.request(t, f.bob, 12, 2)
	if _, err := f.svc.Confirm(ctx, f.owner, org, b.ID); err != nil {
		t.Fatal(err)
	}
	q, err := f.svc.CreateQuote(ctx, f.showroom, QuoteInput{Start: slot(15, 0), End: slot(17, 0)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.Propose(ctx, f.owner, org, b.ID, QuoteRef{QuoteID: q.ID.String()})
	if err != nil || p.Status != StatusRescheduleRequested || !p.Held || p.Proposal == nil || p.Proposal.By != PartyHost {
		t.Fatalf("propose %+v %v", p, err)
	}
	acc, err := f.svc.CustomerAccept(ctx, f.bob, b.ID)
	if err != nil || acc.Status != StatusConfirmed || acc.Start.Format(time.RFC3339) != slot(15, 0) || acc.Proposal != nil || acc.Confirmations != 2 {
		t.Fatalf("accept %+v %v", acc, err)
	}
	// The old slot is free again; the history records every step with its actor.
	if _, err := f.svc.CreateQuote(ctx, f.showroom, QuoteInput{Start: slot(10, 0), End: slot(12, 0)}); err != nil {
		t.Fatalf("old slot: %v", err)
	}
	events, _ := f.svc.Events(ctx, b.ID)
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action+"/"+e.ActorType)
		if e.At.IsZero() || e.ActorUserID == nil {
			t.Fatalf("event without actor or time %+v", e)
		}
	}
	if !slices.Equal(actions, []string{"REQUESTED/CUSTOMER", "CONFIRMED/HOST", "PROPOSED/HOST", "ACCEPTED/CUSTOMER"}) {
		t.Fatalf("events %v", actions)
	}
	// Complete only after the end.
	if _, err := f.svc.Complete(ctx, f.owner, org, b.ID); !errors.As(err, &trans) {
		t.Fatalf("complete before the end: %v", err)
	}
	f.now = time.Date(2026, 11, 9, 16, 0, 0, 0, time.UTC)
	if c, err := f.svc.Complete(ctx, f.owner, org, b.ID); err != nil || c.Status != StatusCompleted {
		t.Fatalf("complete %v", err)
	}
	// Other customers and other organisations: not found.
	if _, err := f.svc.Mine(ctx, f.alice, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another customer's: %v", err)
	}
	other := uuid.New()
	if _, err := f.svc.HostGet(ctx, member(other, auth.RoleOwner, "x"), Host{OrganisationID: &other}, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another organisation's: %v", err)
	}
}

// Cancellation policy windows: free until free_cancellation_hours before
// the start (inclusive), then the policy applies; never after the start.
func TestCancellationWindows(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC) // 10:00 Harare
	for _, c := range []struct {
		before time.Duration
		free   bool
		ok     bool
	}{
		{72 * time.Hour, true, true}, {48 * time.Hour, true, true}, {47 * time.Hour, false, true}, {time.Minute, false, true}, {0, false, false},
	} {
		f := newFixture(t, NewMemoryStore())
		b := f.request(t, f.alice, 10, 2)
		if _, err := f.svc.Confirm(ctx, f.owner, Host{OrganisationID: &f.org}, b.ID); err != nil {
			t.Fatal(err)
		}
		f.now = start.Add(-c.before)
		got, err := f.svc.CustomerCancel(ctx, f.alice, b.ID, "Plans changed")
		if !c.ok {
			var trans *TransitionError
			if !errors.As(err, &trans) {
				t.Fatalf("%s before: %v", c.before, err)
			}
			continue
		}
		if err != nil || got.Status != StatusCancelled || got.CancelFree == nil || *got.CancelFree != c.free || got.CancelledBy != PartyCustomer || got.Held {
			t.Fatalf("%s before: %+v %v", c.before, got, err)
		}
		if got.CancelledAt == nil || !got.CancelledAt.Equal(f.now) {
			t.Fatalf("cancelled at %v", got.CancelledAt)
		}
	}
}
