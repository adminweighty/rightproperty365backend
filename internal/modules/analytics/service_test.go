package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
)

func TestFR035FixtureMemory(t *testing.T) {
	store := NewMemoryStore()
	r := newRig(t, store)
	f := runFixture(t, r)
	checkFixture(t, r, f, func() int { return len(store.Events()) })

	// The listing report reads the rollups for 7d, raw events for 24h.
	owner := uuid.New()
	l := r.listings.m[f.zw]
	l.SellerUserID = &owner
	r.listings.m[f.zw] = l
	p := &auth.Principal{UserID: owner, SessionScope: auth.ScopeCustomer, AccountRoles: []auth.AccountRole{{Role: auth.AccountRoleLandlord, Status: auth.AccountRoleActive}}}
	r.now = f.rollupAt
	rep, err := r.svc.ListingReport(context.Background(), p, f.zw, PeriodQuery{Name: Period7d})
	if err != nil {
		t.Fatal(err)
	}
	wantTotals := Totals{Views: 19, UniqueViews: 15, Impressions: 8, ContactClicks: 3, VideoPlays: 2, Saves: 2, Enquiries: 1, ViewingRequests: 1, AppointmentsConfirmed: 1}
	if rep.Totals != wantTotals || rep.FilteredBotEvents != 4 || rep.LateEventsDropped != 0 || rep.NearRealTime || rep.Label != nil ||
		rep.From != "2026-10-20" || rep.To != "2026-10-26" || len(rep.Daily) != 7 || rep.Daily[5].Date != "2026-10-25" || rep.Daily[5].Views != 11 ||
		rep.Timezone != "Africa/Harare" || rep.RefreshedAt == nil {
		t.Fatalf("7d report: %+v", rep)
	}
	if rep.CTR == nil || *rep.CTR != 0.1579 || rep.Conversion == nil || *rep.Conversion != 0.0526 {
		t.Fatalf("rates: ctr %v conversion %v", rep.CTR, rep.Conversion)
	}
	if rep.Sources[0] != (KeyCount{Key: Unknown, Count: 8}) || rep.Sources[1] != (KeyCount{Key: "direct", Count: 6}) || len(rep.Sources) != len(Sources())+1 {
		t.Fatalf("sources: %+v", rep.Sources)
	}
	live, err := r.svc.ListingReport(context.Background(), p, f.zw, PeriodQuery{Name: Period24h})
	if err != nil {
		t.Fatal(err)
	}
	if live.Totals != (Totals{Views: 3, UniqueViews: 3, VideoPlays: 2, ViewingRequests: 1, AppointmentsConfirmed: 1}) ||
		live.FilteredBotEvents != 4 || !live.NearRealTime || live.Label == nil || *live.Label != NearRealTimeLabel {
		t.Fatalf("24h report: %+v", live)
	}
	// Mauritius' late event is in its 30d report.
	mu := r.listings.m[f.mu]
	mu.SellerUserID = &owner
	r.listings.m[f.mu] = mu
	if rep, _ := r.svc.ListingReport(context.Background(), p, f.mu, PeriodQuery{}); rep.LateEventsDropped != 1 || rep.Totals.Views != 14 || rep.Totals.UniqueViews != 13 {
		t.Fatalf("MU 30d: %+v", rep)
	}
}

func TestIdempotentEventID(t *testing.T) {
	store := NewMemoryStore()
	r := newRig(t, store)
	l := r.listing("ZW", true)
	now := utc(2026, 11, 18, 10, 0)
	one := ev(r.eventID(), TypeDetailView, now, l, obj{"session": sess(1)})
	if res := r.send(now, humanUA, one); res != (IngestResult{Accepted: 1}) {
		t.Fatalf("first: %+v", res)
	}
	if res := r.send(now.Add(time.Minute), humanUA, one, one); res != (IngestResult{Duplicates: 2}) {
		t.Fatalf("retry: %+v", res)
	}
	if n := len(store.Events()); n != 1 {
		t.Fatalf("%d events stored", n)
	}
	// A server-side event recorded twice from the same outbox event.
	src := uuid.New()
	for range 2 {
		if err := r.svc.RecordServer(context.Background(), ServerEvent{SourceID: src, Type: TypeListingSave, ListingID: l, OccurredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(store.Events()); n != 2 || store.Events()[1].ID != ServerEventID(src) || store.Events()[1].Session != "" {
		t.Fatalf("server events: %+v", store.Events())
	}
}

func TestBrowserEventRules(t *testing.T) {
	store := NewMemoryStore()
	r := newRig(t, store)
	public, hidden := r.listing("ZW", true), r.listing("ZW", false)
	now := utc(2026, 11, 18, 10, 0)
	// Every server-only type is rejected from the browser.
	var batch []json.RawMessage
	for _, typ := range ServerTypes() {
		batch = append(batch, ev(r.eventID(), typ, now, public, nil))
	}
	batch = append(batch,
		ev(r.eventID(), TypeDetailView, now, hidden, nil),                                     // not public
		ev(r.eventID(), TypeDetailView, now, uuid.New(), nil),                                 // unknown
		ev(r.eventID(), "page_view", now, public, nil),                                        // unknown type
		ev(r.eventID(), TypeDetailView, now.Add(-25*time.Hour), public, nil),                  // too old
		ev(r.eventID(), TypeDetailView, now.Add(6*time.Minute), public, nil),                  // too far ahead
		ev(r.eventID(), TypeDetailView, now, public, obj{"consent": obj{"analytics": false}}), // no consent
		ev(r.eventID(), TypeDetailView, now, public, obj{"source": "google.com"}),             // a referrer, not a class
		ev(r.eventID(), TypeDetailView, now, public, obj{"session": "short"}),
		ev(r.eventID(), TypeCardImpression, now, public, obj{"placement": "sidebar"}),
		json.RawMessage(`{"event_id": 5}`),
	)
	if res := r.send(now, humanUA, batch...); res.Accepted != 0 || res.Rejected != len(batch) {
		t.Fatalf("rejections: %+v of %d", res, len(batch))
	}
	// A time slightly ahead is clamped to the receive time.
	if res := r.send(now, humanUA, ev(r.eventID(), TypeDetailView, now.Add(2*time.Minute), public, nil)); res.Accepted != 1 {
		t.Fatalf("clamped: %+v", res)
	}
	if got := store.Events()[0]; !got.OccurredAt.Equal(now) || got.LocalDay != "2026-11-18" || got.MarketCode != "ZW" {
		t.Fatalf("clamped event: %+v", got)
	}
	// Request-level rules.
	for _, c := range []struct {
		schema int
		n      int
	}{{2, 1}, {0, 1}, {1, 0}, {1, MaxEventsPerBatch + 1}} {
		events := make([]json.RawMessage, c.n)
		for i := range events {
			events[i] = ev(r.eventID(), TypeDetailView, now, public, nil)
		}
		var verr *ValidationError
		if _, err := r.svc.Ingest(context.Background(), Client{UserAgent: humanUA}, c.schema, events); !errors.As(err, &verr) {
			t.Fatalf("schema %d, %d events: %v", c.schema, c.n, err)
		}
	}
}

func TestIPTruncation(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.77":        "203.0.113.0/24",
		"2001:db8:abcd:12::1": "2001:db8:abcd::/48",
		"::ffff:198.51.100.9": "198.51.100.0/24",
		"fe80::1%eth0":        "fe80::/48",
		"not an address":      "",
		"":                    "",
	} {
		if got := TruncateIP(in); got != want {
			t.Fatalf("TruncateIP(%q) = %q, want %q", in, got, want)
		}
	}
	store := NewMemoryStore()
	r := newRig(t, store)
	l := r.listing("GB", true)
	now := utc(2026, 11, 18, 10, 0)
	r.now = now
	for _, ip := range []string{"203.0.113.77", "2001:db8:abcd:12::1"} {
		if _, err := r.svc.Ingest(context.Background(), Client{UserAgent: humanUA, IP: ip}, 1, []json.RawMessage{ev(r.eventID(), TypeContactClick, now, l, nil)}); err != nil {
			t.Fatal(err)
		}
	}
	got := store.Events()
	if len(got) != 2 || got[0].Network != "203.0.113.0/24" || got[1].Network != "2001:db8:abcd::/48" {
		t.Fatalf("stored networks: %+v", got)
	}
}

func TestBots(t *testing.T) {
	for _, ua := range []string{"", "Googlebot/2.1", "Mozilla/5.0 HeadlessChrome/130", "curl/8.0", "UptimeRobot/2.0", "facebookexternalhit/1.1"} {
		if !IsBotUserAgent(ua) {
			t.Fatalf("%q is a bot", ua)
		}
	}
	if IsBotUserAgent(humanUA) || IsBotUserAgent("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148 Safari/604.1") {
		t.Fatal("a browser is not a bot")
	}
	// Over 300 events of one session in 10 minutes: the excess is bot.
	store := NewMemoryStore()
	r := newRig(t, store)
	l := r.listing("ZW", true)
	now := utc(2026, 11, 18, 10, 0)
	for b := range 7 {
		batch := make([]json.RawMessage, 50)
		for i := range batch {
			batch[i] = ev(r.eventID(), TypeCardImpression, now, l, obj{"session": sess(1)})
		}
		r.send(now.Add(time.Duration(b)*time.Minute), humanUA, batch...)
	}
	rate := 0
	for _, e := range store.Events() {
		if e.Bot && e.BotReason == BotRate {
			rate++
		}
	}
	if rate != 50 {
		t.Fatalf("%d events marked by rate, want 50", rate)
	}
}

func TestRetention(t *testing.T) {
	store := NewMemoryStore()
	r := newRig(t, store)
	l := r.listing("ZW", true)
	now := utc(2026, 11, 18, 10, 0)
	r.now = now
	ctx := context.Background()
	old, recent := now.AddDate(0, 0, -91), now.AddDate(0, 0, -89)
	for _, at := range []time.Time{old, recent} {
		if _, err := store.Insert(ctx, Event{ID: uuid.New(), Type: TypeDetailView, OccurredAt: at, ReceivedAt: at, LocalDay: LocalDay(at, time.UTC),
			ListingID: l, MarketCode: "ZW", Origin: OriginBrowser}, SPAWindow); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.RecordLate(ctx, LateEvent{EventID: uuid.New(), Type: TypeEnquirySubmit, ListingID: l, MarketCode: "ZW", OccurredAt: old.AddDate(0, 0, -4), ReceivedAt: old, LocalDay: "2026-08-19"})
	_ = store.UpsertRollups(ctx, []DayRow{{ListingID: l, Day: LocalDay(old, time.UTC), MarketCode: "ZW", Totals: Totals{Views: 1}}}, now)
	events, late, err := r.svc.PurgeRaw(ctx)
	if err != nil || events != 1 || late != 1 {
		t.Fatalf("purge: %d %d %v", events, late, err)
	}
	if got := store.Events(); len(got) != 1 || !got[0].OccurredAt.Equal(recent) {
		t.Fatalf("left: %+v", got)
	}
	if rows, _ := store.Rollups(ctx, RollupFilter{}); len(rows) != 1 {
		t.Fatal("rollups must be kept")
	}
	custom := NewService(Deps{Store: store, Listings: r.listings, Zones: testZones, Now: func() time.Time { return now }, RawRetentionDays: 30})
	if !custom.RetentionCutoff().Equal(now.AddDate(0, 0, -30)) {
		t.Fatal("retention days not applied")
	}
}

func TestReportPermissions(t *testing.T) {
	store := NewMemoryStore()
	r := newRig(t, store)
	ctx := context.Background()
	org, other := uuid.New(), uuid.New()
	agent, otherAgent := uuid.New(), uuid.New()
	mine := r.listing("ZW", true)
	theirs := r.listing("ZW", true)
	foreign := r.listing("ZW", true)
	set := func(id uuid.UUID, o uuid.UUID, a uuid.UUID) {
		l := r.listings.m[id]
		l.OrganisationID, l.AgentUserID = &o, &a
		r.listings.m[id] = l
	}
	set(mine, org, agent)
	set(theirs, org, otherAgent)
	set(foreign, other, uuid.New())
	member := func(user uuid.UUID, role string) *auth.Principal {
		return &auth.Principal{UserID: user, SessionScope: auth.ScopeCustomer, Memberships: []auth.Membership{
			{OrganisationID: org, OrganisationKind: auth.KindAgency, MarketCode: "ZW", Role: role, OrganisationName: "Org"}}}
	}
	owner, ag, staff := member(uuid.New(), auth.RoleOwner), member(agent, auth.RoleAgent), member(uuid.New(), auth.RoleStaff)
	q := PeriodQuery{Name: Period30d}
	r.now = utc(2026, 11, 18, 10, 0)

	if _, err := r.svc.ListingReport(ctx, owner, theirs, q); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if _, err := r.svc.ListingReport(ctx, ag, mine, q); err != nil {
		t.Fatalf("assigned agent: %v", err)
	}
	for _, c := range []struct {
		p  *auth.Principal
		id uuid.UUID
	}{{ag, theirs}, {owner, foreign}, {staff, mine}, {owner, uuid.New()}} {
		if _, err := r.svc.ListingReport(ctx, c.p, c.id, q); !errors.Is(err, ErrNotFound) {
			t.Fatalf("listing %s: %v, want not found", c.id, err)
		}
	}
	if rep, err := r.svc.OrganisationReport(ctx, owner, org, q); err != nil || rep.ListingsCount != 2 || rep.Scope != ScopeOrganisation {
		t.Fatalf("owner's org report: %+v %v", rep, err)
	}
	if rep, err := r.svc.OrganisationReport(ctx, ag, org, q); err != nil || rep.ListingsCount != 1 || rep.Scope != ScopeAssigned {
		t.Fatalf("agent's org report: %+v %v", rep, err)
	}
	if _, err := r.svc.OrganisationReport(ctx, owner, other, q); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong organisation: %v", err)
	}
	if _, err := r.svc.OrganisationReport(ctx, staff, org, q); !errors.Is(err, ErrForbidden) {
		t.Fatalf("staff: %v", err)
	}
	if _, err := r.svc.OrganisationReport(ctx, nil, org, q); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous: %v", err)
	}
	// Admin markets.
	admin := func(role, market string) *auth.Principal {
		return &auth.Principal{SessionScope: auth.ScopeAdmin, PlatformRoles: []auth.PlatformRole{{Role: role, MarketCode: market}}}
	}
	if _, err := r.svc.AdminSummary(ctx, admin(auth.PlatformMarketAdmin, "ZW"), "GB", q); !errors.Is(err, ErrForbidden) {
		t.Fatalf("market admin, other market: %v", err)
	}
	if s, err := r.svc.AdminSummary(ctx, admin(auth.PlatformMarketAdmin, "ZW"), "", q); err != nil || s.Timezone != "Africa/Harare" {
		t.Fatalf("market admin: %+v %v", s, err)
	}
	if _, err := r.svc.AdminSummary(ctx, admin(auth.PlatformSupport, ""), "", q); !errors.Is(err, ErrForbidden) {
		t.Fatalf("support: %v", err)
	}
	var verr *ValidationError
	if _, err := r.svc.AdminSummary(ctx, admin(auth.PlatformAdmin, ""), "XX", q); !errors.As(err, &verr) {
		t.Fatalf("unknown market: %v", err)
	}
}

func TestParsePeriod(t *testing.T) {
	if q, err := ParsePeriod("", "", ""); err != nil || q.Name != Period30d {
		t.Fatalf("default: %+v %v", q, err)
	}
	if _, err := ParsePeriod("custom", "2026-01-01", "2026-01-31"); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][3]string{{"1y", "", ""}, {"custom", "", ""}, {"custom", "2026-02-01", "2026-01-01"}, {"custom", "2025-01-01", "2026-06-01"}, {"7d", "2026-01-01", ""}} {
		if _, err := ParsePeriod(c[0], c[1], c[2]); err == nil {
			t.Fatalf("%v accepted", c)
		}
	}
}
