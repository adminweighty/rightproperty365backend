package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/saved"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// ---------------------------------------------------------------------------
// Test doubles shared by the tests of this package
// ---------------------------------------------------------------------------

type fakeListings struct{ m map[uuid.UUID]ListingInfo }

func (f *fakeListings) Listings(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]ListingInfo, error) {
	out := map[uuid.UUID]ListingInfo{}
	for _, id := range ids {
		if l, ok := f.m[id]; ok {
			out[id] = l
		}
	}
	return out, nil
}

func (f *fakeListings) OrganisationListings(_ context.Context, org uuid.UUID) ([]ListingInfo, error) {
	var out []ListingInfo
	for _, l := range f.m {
		if l.OrganisationID != nil && *l.OrganisationID == org {
			out = append(out, l)
		}
	}
	return out, nil
}

type fakeZones map[string]string

func (z fakeZones) CountryZones(context.Context) (map[string]string, error) { return z, nil }

var testZones = fakeZones{"ZW": "Africa/Harare", "GB": "Europe/London", "MU": "Indian/Mauritius"}

// rig is a service on a store with a settable clock.
type rig struct {
	t        *testing.T
	store    Store
	svc      *Service
	listings *fakeListings
	now      time.Time
	seq      int
}

func newRig(t *testing.T, store Store) *rig {
	r := &rig{t: t, store: store, listings: &fakeListings{m: map[uuid.UUID]ListingInfo{}}}
	r.svc = NewService(Deps{Store: store, Listings: r.listings, Zones: testZones, Now: func() time.Time { return r.now }})
	return r
}

func (r *rig) listing(market string, public bool) uuid.UUID {
	id := uuid.New()
	r.listings.m[id] = ListingInfo{ID: id, Title: "Listing in " + market, Reference: "RP-" + market + "-" + id.String()[:4], MarketCode: market, Public: public}
	return id
}

// eventID returns a new deterministic event_id.
func (r *rig) eventID() string {
	r.seq++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", r.seq)
}

func sess(n int) string { return fmt.Sprintf("sess%028d", n) }

type obj = map[string]any

func ev(id, eventType string, at time.Time, listing uuid.UUID, extra obj) json.RawMessage {
	m := obj{"event_id": id, "type": eventType, "occurred_at": at.UTC().Format(time.RFC3339), "listing_id": listing.String(),
		"consent": obj{"analytics": true}}
	for k, v := range extra {
		m[k] = v
	}
	raw, _ := json.Marshal(m)
	return raw
}

const humanUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"

// send ingests events at clock with a user agent.
func (r *rig) send(clock time.Time, ua string, events ...json.RawMessage) IngestResult {
	r.t.Helper()
	r.now = clock
	res, err := r.svc.Ingest(context.Background(), Client{UserAgent: ua, IP: "203.0.113.77"}, SchemaVersion, events)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func utc(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// FR-035 fixture: 100 events across three time zones
// ---------------------------------------------------------------------------

type fixtureResult struct {
	zw, gb, mu uuid.UUID
	ingest     IngestResult
	sent       int
	rollupAt   time.Time
}

// runFixture sends the FR-035 fixture: 100 events, 90 from the browser and
// 10 server-side (through the outbox Recorder), across Zimbabwe (UTC+2),
// the United Kingdom (around the end of summer time, 25 October 2026 01:00
// UTC) and Mauritius (UTC+4), placed around their local midnights. It holds
// duplicates (event_id), an SPA double view, bot user agents, three browser
// events that are rejected and one late server-side event.
func runFixture(t *testing.T, r *rig) fixtureResult {
	t.Helper()
	f := fixtureResult{zw: r.listing("ZW", true), gb: r.listing("GB", true), mu: r.listing("MU", true), rollupAt: utc(2026, 10, 26, 18, 0)}
	hidden := r.listing("MU", false)
	minute := time.Minute
	add := func(res IngestResult, n int) {
		f.ingest.Accepted += res.Accepted
		f.ingest.Duplicates += res.Duplicates
		f.ingest.Rejected += res.Rejected
		f.sent += n
	}
	// views sends one detail view per session at t (session 0: none).
	views := func(listing uuid.UUID, at time.Time, ua string, extra obj, sessions ...int) []json.RawMessage {
		var out []json.RawMessage
		for _, s := range sessions {
			x := obj{}
			for k, v := range extra {
				x[k] = v
			}
			if s > 0 {
				x["session"] = sess(s)
			}
			out = append(out, ev(r.eventID(), TypeDetailView, at, listing, x))
		}
		res := r.send(at.Add(minute), ua, out...)
		add(res, len(out))
		return out
	}
	of := func(eventType string, listing uuid.UUID, at time.Time, ua string, extra func(i int) obj, sessions ...int) {
		var out []json.RawMessage
		for i, s := range sessions {
			x := obj{"session": sess(s)}
			if extra != nil {
				for k, v := range extra(i) {
					x[k] = v
				}
			}
			out = append(out, ev(r.eventID(), eventType, at, listing, x))
		}
		add(r.send(at.Add(minute), ua, out...), len(out))
	}
	resend := func(clock time.Time, events ...json.RawMessage) { add(r.send(clock, humanUA, events...), len(events)) }

	// Server-side events, through the worker's outbox handler. Appointments
	// are looked up by ID.
	appts := map[uuid.UUID]uuid.UUID{}
	rec := NewRecorder(r.svc, func(_ context.Context, id uuid.UUID) (*uuid.UUID, error) {
		if l, ok := appts[id]; ok {
			return &l, nil
		}
		return nil, nil
	})
	server := func(clock time.Time, eventType string, listing uuid.UUID, at time.Time) {
		t.Helper()
		r.now = clock
		e := outbox.Event{ID: uuid.New(), Type: eventType, CreatedAt: at}
		var payload obj
		switch eventType {
		case saved.EventListingSaved:
			payload = obj{"user_id": uuid.NewString(), "listing_id": listing.String(), "at": at}
		case propertyrequests.EventPropertyRequestCreated:
			payload = obj{"request_id": uuid.NewString(), "listing_id": listing.String(), "request_type": "ENQUIRY", "created_at": at}
		case appointments.EventRequested, appointments.EventConfirmed:
			appt := uuid.New()
			appts[appt] = listing
			e.AggregateID = appt
			payload = obj{"appointment_id": appt, "occurred_at": at}
		case listings.EventListingRelisted:
			e.AggregateID = listing
			payload = obj{"listing_id": listing.String(), "occurred_at": at}
		}
		e.Payload, _ = json.Marshal(payload)
		if err := rec.Handle(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		f.sent++
	}

	searchMobile := obj{"source": "search_engine", "device": "mobile"}
	directDesktop := obj{"source": "direct", "device": "desktop"}

	// 24 October.
	a := views(f.zw, utc(2026, 10, 24, 21, 59), humanUA, searchMobile, 1, 2, 3, 4, 5) // ZW 23:59, 24 Oct
	views(f.zw, utc(2026, 10, 24, 22, 0), humanUA, directDesktop, 6, 7, 8, 9, 10, 11) // ZW 00:00, 25 Oct
	resend(utc(2026, 10, 24, 22, 5), a[0], a[1])                                      // 2 duplicates
	k := views(f.gb, utc(2026, 10, 24, 22, 59), humanUA, nil, 40, 41, 42, 43)         // GB 23:59 BST, 24 Oct
	views(f.gb, utc(2026, 10, 24, 23, 0), humanUA, nil, 44, 45, 46)                   // GB 00:00 BST, 25 Oct
	resend(utc(2026, 10, 24, 23, 10), k[0])                                           // 1 duplicate
	// 25 October (summer time ends in the UK at 01:00 UTC).
	views(f.gb, utc(2026, 10, 25, 1, 30), humanUA, nil, 47, 48) // GB 01:30 GMT, 25 Oct
	of(TypeCardImpression, f.zw, utc(2026, 10, 25, 9, 0), humanUA, func(i int) obj { return obj{"placement": "search", "rank": i + 1} },
		20, 21, 22, 23, 24, 25, 26, 27)
	views(f.zw, utc(2026, 10, 25, 10, 0), humanUA, nil, 6, 7)
	views(f.zw, utc(2026, 10, 25, 11, 0), humanUA, nil, 6)
	views(f.zw, utc(2026, 10, 25, 12, 0), humanUA, nil, 7)
	of(TypeCardImpression, f.gb, utc(2026, 10, 25, 12, 0), humanUA, func(int) obj { return obj{"placement": "rail"} }, 60, 61, 62, 63, 64, 65)
	views(f.gb, utc(2026, 10, 25, 12, 0), "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 HeadlessChrome/130.0 Safari/537.36", nil, 66, 67)
	of(TypeCardImpression, f.gb, utc(2026, 10, 25, 12, 0), "curl/8.0.1", nil, 68)
	server(utc(2026, 10, 25, 12, 1), saved.EventListingSaved, f.zw, utc(2026, 10, 25, 12, 0))
	server(utc(2026, 10, 25, 12, 1), saved.EventListingSaved, f.zw, utc(2026, 10, 25, 12, 0))
	server(utc(2026, 10, 25, 13, 1), propertyrequests.EventPropertyRequestCreated, f.zw, utc(2026, 10, 25, 13, 0))
	views(f.zw, utc(2026, 10, 25, 14, 0), humanUA, nil, 12)
	views(f.zw, utc(2026, 10, 25, 14, 10), humanUA, nil, 12) // the SPA double view
	of(TypeContactClick, f.zw, utc(2026, 10, 25, 15, 0), humanUA, nil, 6, 7, 8)
	server(utc(2026, 10, 25, 15, 1), listings.EventListingRelisted, f.gb, utc(2026, 10, 25, 15, 0))
	views(f.mu, utc(2026, 10, 25, 19, 59), humanUA, nil, 70, 71, 72, 73, 74)     // MU 23:59, 25 Oct
	v := views(f.mu, utc(2026, 10, 25, 20, 0), humanUA, nil, 75, 76, 77, 78, 79) // MU 00:00, 26 Oct
	resend(utc(2026, 10, 25, 20, 5), v[0])                                       // 1 duplicate
	views(f.gb, utc(2026, 10, 25, 23, 30), humanUA, nil, 49, 50, 51)             // GB 23:30 GMT: still 25 Oct
	// 26 October.
	views(f.gb, utc(2026, 10, 26, 0, 0), humanUA, nil, 52, 53) // GB 00:00 GMT, 26 Oct
	views(f.gb, utc(2026, 10, 26, 0, 45), humanUA, nil, 52)    // 45 minutes later: a second view
	of(TypeCardImpression, f.mu, utc(2026, 10, 26, 5, 0), humanUA, func(int) obj { return obj{"placement": "similar"} }, 90, 91, 92, 93)
	views(f.mu, utc(2026, 10, 26, 6, 0), humanUA, nil, 70, 71, 75, 80)
	server(utc(2026, 10, 26, 7, 1), appointments.EventRequested, f.zw, utc(2026, 10, 26, 7, 0))
	server(utc(2026, 10, 26, 7, 1), appointments.EventConfirmed, f.zw, utc(2026, 10, 26, 7, 0))
	of(TypeContactClick, f.mu, utc(2026, 10, 26, 7, 0), humanUA, nil, 75)
	views(f.mu, utc(2026, 10, 26, 7, 0), "python-requests/2.31.0", nil, 95)
	// Rejected: a server-only type from the browser, a listing that is not
	// public, a time too far in the future.
	clock := utc(2026, 10, 26, 7, 1)
	add(r.send(clock, humanUA,
		ev(r.eventID(), TypeListingSave, clock, f.mu, nil),
		ev(r.eventID(), TypeDetailView, clock, hidden, obj{"session": sess(96)}),
		ev(r.eventID(), TypeDetailView, clock.Add(10*minute), f.mu, obj{"session": sess(97)})), 3)
	of(TypeVideoPlay, f.zw, utc(2026, 10, 26, 8, 0), humanUA, nil, 30, 31)
	server(utc(2026, 10, 26, 8, 1), propertyrequests.EventPropertyRequestCreated, f.mu, utc(2026, 10, 26, 8, 0))
	server(utc(2026, 10, 26, 8, 1), propertyrequests.EventPropertyRequestCreated, f.mu, utc(2026, 10, 26, 8, 0))
	views(f.zw, utc(2026, 10, 26, 9, 0), humanUA, nil, 0, 0, 0) // no session: each counts as unique
	views(f.zw, utc(2026, 10, 26, 9, 30), "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", nil, 32, 33, 34, 35)
	of(TypeContactClick, f.gb, utc(2026, 10, 26, 10, 0), humanUA, nil, 52, 53)
	// The late event: an enquiry of 21 October processed on 26 October.
	server(utc(2026, 10, 26, 10, 0), propertyrequests.EventPropertyRequestCreated, f.mu, utc(2026, 10, 21, 10, 0))
	server(utc(2026, 10, 26, 11, 1), saved.EventListingSaved, f.gb, utc(2026, 10, 26, 11, 0))
	return f
}

// want is one expected rollup row.
type want struct {
	day      string
	totals   Totals
	filtered int
	late     int
}

func expectedFixture(f fixtureResult) map[uuid.UUID][]want {
	return map[uuid.UUID][]want{
		f.zw: {
			{day: "2026-10-24", totals: Totals{Views: 5, UniqueViews: 5}},
			{day: "2026-10-25", totals: Totals{Views: 11, UniqueViews: 7, Impressions: 8, ContactClicks: 3, Saves: 2, Enquiries: 1}},
			{day: "2026-10-26", totals: Totals{Views: 3, UniqueViews: 3, VideoPlays: 2, ViewingRequests: 1, AppointmentsConfirmed: 1}, filtered: 4},
		},
		f.gb: {
			{day: "2026-10-24", totals: Totals{Views: 4, UniqueViews: 4}},
			{day: "2026-10-25", totals: Totals{Views: 8, UniqueViews: 8, Impressions: 6, Relists: 1}, filtered: 3},
			{day: "2026-10-26", totals: Totals{Views: 3, UniqueViews: 2, ContactClicks: 2, Saves: 1}},
		},
		f.mu: {
			{day: "2026-10-25", totals: Totals{Views: 5, UniqueViews: 5}},
			{day: "2026-10-26", totals: Totals{Views: 9, UniqueViews: 8, Impressions: 4, ContactClicks: 1, Enquiries: 2}, filtered: 1, late: 1},
		},
	}
}

// checkFixture runs the rollup and asserts every figure of the fixture.
func checkFixture(t *testing.T, r *rig, f fixtureResult, storedEvents func() int) {
	t.Helper()
	ctx := context.Background()
	if f.sent != 100 {
		t.Fatalf("the fixture sent %d events, want 100", f.sent)
	}
	if f.ingest != (IngestResult{Accepted: 82, Duplicates: 5, Rejected: 3}) {
		t.Fatalf("ingestion: %+v, want 82 accepted, 5 duplicates (4 event_id, 1 SPA), 3 rejected", f.ingest)
	}
	// 82 browser events (8 of them bots) and 9 server-side ones; the late
	// one is not stored as an event.
	if n := storedEvents(); n != 91 {
		t.Fatalf("%d raw events stored, want 91", n)
	}

	r.now = f.rollupAt
	res, err := r.svc.Rollup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Markets != 3 || res.Rows != 8 {
		t.Fatalf("rollup: %+v", res)
	}
	rows, err := r.store.Rollups(ctx, RollupFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID][]want{}
	var filtered, late int
	for _, row := range rows {
		got[row.ListingID] = append(got[row.ListingID], want{day: row.Day, totals: row.Totals, filtered: row.FilteredBotEvents, late: row.LateEventsDropped})
		filtered += row.FilteredBotEvents
		late += row.LateEventsDropped
		if !row.RefreshedAt.Equal(f.rollupAt) {
			t.Fatalf("refreshed_at %v", row.RefreshedAt)
		}
	}
	for listing, days := range expectedFixture(f) {
		if !reflect.DeepEqual(got[listing], days) {
			t.Fatalf("listing %s (%s):\n got %+v\nwant %+v", listing, r.listings.m[listing].MarketCode, got[listing], days)
		}
	}
	if filtered != 8 || late != 1 {
		t.Fatalf("filtered %d (want 8), late %d (want 1)", filtered, late)
	}
	// Breakdowns of Zimbabwe's views.
	zw, _ := r.store.Rollups(ctx, RollupFilter{ListingIDs: []uuid.UUID{f.zw}, FromDay: "2026-10-24", ToDay: "2026-10-25"})
	if len(zw) != 2 || !reflect.DeepEqual(zw[0].Sources, map[string]int{"search_engine": 5}) || !reflect.DeepEqual(zw[0].Devices, map[string]int{"mobile": 5}) ||
		!reflect.DeepEqual(zw[1].Sources, map[string]int{"direct": 6, Unknown: 5}) || !reflect.DeepEqual(zw[1].Devices, map[string]int{"desktop": 6, Unknown: 5}) {
		t.Fatalf("zw breakdowns: %+v", zw)
	}

	// Idempotent: a second run (15 minutes later) gives the same rows.
	r.now = f.rollupAt.Add(15 * time.Minute)
	if _, err := r.svc.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	again, _ := r.store.Rollups(ctx, RollupFilter{})
	if len(again) != len(rows) {
		t.Fatalf("second run: %d rows, want %d", len(again), len(rows))
	}
	for i := range rows {
		a, b := rows[i], again[i]
		if !b.RefreshedAt.Equal(r.now) {
			t.Fatalf("second run refreshed_at %v", b.RefreshedAt)
		}
		a.RefreshedAt, b.RefreshedAt = time.Time{}, time.Time{}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("second run changed a row:\n%+v\n%+v", a, b)
		}
	}
	if last, _ := r.store.LastRefresh(ctx); last == nil || !last.Equal(r.now) {
		t.Fatalf("last refresh %v", last)
	}
}
