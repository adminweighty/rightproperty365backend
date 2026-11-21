package listings

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/internal/platform/outbox"
	"rightproperty365.com/backend/migrations"
)

func TestExpiryOf(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	published := Record{Status: StatusPublished, PublishedAt: now.AddDate(0, 0, -80)}
	type expiryCase struct {
		name string
		r    Record
		want *Expiry
	}
	cases := []expiryCase{
		{"never published", Record{Status: StatusDraft}, nil},
		{"15 days left", periodOf(published, now.Add(15*24*time.Hour), 0), &Expiry{DaysLeft: 15, State: ExpiryActive, RelistNo: 1}},
		{"exactly 14 days", periodOf(published, now.Add(14*24*time.Hour), 1), &Expiry{DaysLeft: 14, State: ExpiryExpiringSoon, RelistNo: 1}},
		{"5 days 2 hours", periodOf(published, now.Add(5*24*time.Hour+2*time.Hour), 3), &Expiry{DaysLeft: 6, State: ExpiryExpiringSoon, RelistNo: 3}},
		{"an hour", periodOf(published, now.Add(time.Hour), 1), &Expiry{DaysLeft: 1, State: ExpiryExpiringSoon, RelistNo: 1}},
		{"past, still stored PUBLISHED", periodOf(published, now.Add(-time.Minute), 1), &Expiry{State: ExpiryExpired, RelistNo: 1}},
	}
	stored := periodOf(published, now.Add(-time.Hour), 2)
	stored.Status = StatusExpired
	cases = append(cases, expiryCase{"stored EXPIRED", stored, &Expiry{State: ExpiryExpired, RelistNo: 2}})
	relist := periodOf(published, now.Add(-time.Hour), 1)
	relist.Status = StatusPendingReview
	cases = append(cases, expiryCase{"relist waiting for review", relist, &Expiry{State: ExpiryExpired, RelistNo: 1}})
	for _, tc := range cases {
		got := ExpiryOf(tc.r, now)
		if tc.want == nil {
			if got != nil {
				t.Fatalf("%s: %+v, want nil", tc.name, got)
			}
			continue
		}
		tc.want.ExpiresAt = tc.r.ExpiresAt
		if got == nil || *got != *tc.want {
			t.Fatalf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func periodOf(r Record, expires time.Time, cycles int) Record {
	r.ExpiresAt, r.Cycles = expires, cycles
	return r
}

func TestPlanExpiry(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for left, want := range map[time.Duration][]expiryNotice{
		15 * day:           nil,
		14*day + time.Hour: nil,
		14 * day:           {{kind: NoticeDays14}},
		8 * day:            {{kind: NoticeDays14}},
		7 * day:            {{kind: NoticeDays7}, {kind: NoticeDays14, skipped: true}},
		3 * day:            {{kind: NoticeDays7}, {kind: NoticeDays14, skipped: true}},
		day:                {{kind: NoticeDays1}, {kind: NoticeDays7, skipped: true}, {kind: NoticeDays14, skipped: true}},
		time.Minute:        {{kind: NoticeDays1}, {kind: NoticeDays7, skipped: true}, {kind: NoticeDays14, skipped: true}},
		0:                  {{kind: NoticeExpired}},
		-time.Hour:         {{kind: NoticeExpired}},
	} {
		step, ok := planExpiry(now.Add(left), now)
		if ok != (want != nil) || len(step.notices) != len(want) {
			t.Fatalf("%v left: %+v %v, want %+v", left, step, ok, want)
		}
		for i := range want {
			if step.notices[i] != want[i] {
				t.Fatalf("%v left: %+v, want %+v", left, step.notices, want)
			}
		}
	}
}

// expiryStore is a store the sweep runs on.
type expiryStore interface {
	ManageStore
	ExpiryStore
}

// expiryEvents returns a store's LISTING_EXPIRING and LISTING_EXPIRED events
// of one listing, oldest first.
type expiryEvents func(id uuid.UUID) []outbox.Event

// expiryStoreContract runs the same checks on both stores. seller is a
// user who may own a listing. Every sweep runs at the real clock, and the
// checks only look at this test's own listings (the Postgres sweep also
// deals with whatever else is due in the database).
func expiryStoreContract(t *testing.T, store expiryStore, seller uuid.UUID, events expiryEvents) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	day := 24 * time.Hour
	sweep := func() {
		t.Helper()
		if _, err := SweepExpiry(ctx, store, now); err != nil {
			t.Fatal(err)
		}
	}
	// publish creates a PUBLISHED listing ending at expires, with its cycle.
	publish := func(title string, expires time.Time) uuid.UUID {
		t.Helper()
		id := uuid.New()
		ref := "RP-ZW-" + id.String()[:8]
		m := ManagedListing{Record: Record{
			ID: id, Slug: slugFor(title, ref), Reference: ref, Title: title, MarketCode: "ZW", Price: Money{Currency: "USD"},
			Status: StatusDraft, UpdatedAt: now.Add(-81 * day), SellerUserID: &seller, Features: []string{},
		}, CreatedAt: now.Add(-81 * day)}
		if err := store.CreateListing(ctx, m, Change{}); err != nil {
			t.Fatal(err)
		}
		expect := m.UpdatedAt
		m.Status, m.PublishedAt, m.ExpiresAt, m.UpdatedAt = StatusPublished, now.Add(-80*day), expires, now.Add(-80*day)
		if _, err := store.SaveListing(ctx, m, expect, Change{NewCycle: true}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// move changes the end of the current period (time passing, seen from
	// the listing's side, so every sweep can run at the real clock).
	move := func(id uuid.UUID, expires time.Time) {
		t.Helper()
		m, err := store.Managed(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		expect := m.UpdatedAt
		m.ExpiresAt, m.UpdatedAt = expires, expect.Add(time.Second)
		if _, err := store.SaveListing(ctx, m, expect, Change{}); err != nil {
			t.Fatal(err)
		}
	}
	// sent sums up a listing's expiry events: "14", "7", "1" or "EXPIRED",
	// with the cycle.
	sent := func(id uuid.UUID) []string {
		t.Helper()
		var out []string
		for _, ev := range events(id) {
			var p ExpiryEventPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.ListingID != id || p.ExpiresAt.IsZero() || p.RelistNo < 1 {
				t.Fatalf("payload %s", ev.Payload)
			}
			s := "EXPIRED"
			if ev.Type == EventListingExpiring {
				s = map[int]string{14: "14", 7: "7", 1: "1"}[p.Days]
			}
			out = append(out, s+"#"+string(rune('0'+p.RelistNo)))
		}
		return out
	}
	expectSent := func(id uuid.UUID, want ...string) {
		t.Helper()
		got := sent(id)
		if len(got) != len(want) {
			t.Fatalf("sent %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sent %v, want %v", got, want)
			}
		}
	}

	// A: every reminder once, then the expiry once.
	a := publish("Expiry contract A", now.Add(10*day))
	far := publish("Expiry contract far", now.Add(20*day))
	sweep()
	sweep()
	expectSent(a, "14#1")
	expectSent(far)
	move(a, now.Add(5*day))
	sweep()
	sweep()
	expectSent(a, "14#1", "7#1")
	move(a, now.Add(12*time.Hour))
	sweep()
	expectSent(a, "14#1", "7#1", "1#1")
	move(a, now.Add(-time.Minute))
	sweep()
	sweep()
	expectSent(a, "14#1", "7#1", "1#1", "EXPIRED#1")
	got, err := store.Managed(ctx, a)
	if err != nil || got.Status != StatusExpired || !got.UpdatedAt.Equal(now) {
		t.Fatalf("expired: %s %v %v", got.Status, got.UpdatedAt, err)
	}
	timeline, err := store.Timeline(ctx, a)
	if err != nil || len(timeline) != 1 || timeline[0].Action != ActionExpired || timeline[0].ActorKind != ActorSystem ||
		timeline[0].ActorUserID != nil || timeline[0].FromStatus != StatusPublished || timeline[0].ToStatus != StatusExpired {
		t.Fatalf("timeline: %+v %v", timeline, err)
	}

	// B: the worker was down until 3 days before the end: only the 7-day
	// reminder (the 14-day one is recorded as skipped and never sent).
	b := publish("Expiry contract B", now.Add(3*day))
	sweep()
	expectSent(b, "7#1")
	move(b, now.Add(10*day))
	sweep()
	expectSent(b, "7#1")
	move(b, now.Add(20*time.Hour))
	sweep()
	expectSent(b, "7#1", "1#1")

	// A relist's approval starts cycle 2, with reminders of its own.
	got, err = store.Managed(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	expect := got.UpdatedAt
	got.Status, got.PublishedAt, got.ExpiresAt, got.UpdatedAt = StatusPublished, now.Add(-80*day), now.Add(10*day), now.Add(time.Second)
	if _, err := store.SaveListing(ctx, got, expect, Change{NewCycle: true, MinRelistNo: 1}); err != nil {
		t.Fatal(err)
	}
	if got, err = store.Managed(ctx, a); err != nil || got.Cycles != 2 || got.CycleNo() != 2 {
		t.Fatalf("cycles: %d %v", got.Cycles, err)
	}
	sweep()
	expectSent(a, "14#1", "7#1", "1#1", "EXPIRED#1", "14#2")

	// Concurrent sweeps: every expiry exactly once.
	var expired []uuid.UUID
	for range 5 {
		expired = append(expired, publish("Expiry contract concurrent", now.Add(-time.Hour)))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := SweepExpiry(ctx, store, now); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, id := range expired {
		expectSent(id, "EXPIRED#1")
		if got, err := store.Managed(ctx, id); err != nil || got.Status != StatusExpired {
			t.Fatalf("concurrent: %s %v", got.Status, err)
		}
		if timeline, err := store.Timeline(ctx, id); err != nil || len(timeline) != 1 {
			t.Fatalf("concurrent timeline: %+v %v", timeline, err)
		}
	}

	// A paused listing (API 0.48.0): its period keeps running, it gets its
	// reminder and it expires; the timeline says it came from PAUSED.
	paused := publish("Expiry contract paused", now.Add(3*day))
	pm, perr := store.Managed(ctx, paused)
	if perr != nil {
		t.Fatal(perr)
	}
	pausedAt := pm.UpdatedAt
	pm.Status, pm.UpdatedAt = StatusPaused, pausedAt.Add(time.Second)
	if _, err := store.SaveListing(ctx, pm, pausedAt, Change{Entry: &TimelineEntry{ID: uuid.New(), ListingID: paused, ActorKind: ActorAdvertiser,
		Action: ActionPaused, FromStatus: StatusPublished, ToStatus: StatusPaused, CreatedAt: pm.UpdatedAt}}); err != nil {
		t.Fatal(err)
	}
	sweep()
	expectSent(paused, "7#1")
	move(paused, now.Add(-time.Minute))
	sweep()
	expectSent(paused, "7#1", "EXPIRED#1")
	if got, err := store.Managed(ctx, paused); err != nil || got.Status != StatusExpired {
		t.Fatalf("paused expiry: %s %v", got.Status, err)
	}
	timeline, terr := store.Timeline(ctx, paused)
	if terr != nil || len(timeline) != 2 || timeline[1].FromStatus != StatusPaused || timeline[0].Action != ActionPaused {
		t.Fatalf("paused timeline: %+v %v", timeline, terr)
	}
}

func TestMemoryExpirySweep(t *testing.T) {
	store := NewMemoryStore(noLabels{})
	expiryStoreContract(t, store, uuid.New(), func(id uuid.UUID) []outbox.Event {
		var out []outbox.Event
		for _, ev := range store.Outbox() {
			if ev.AggregateID == id && (ev.Type == EventListingExpiring || ev.Type == EventListingExpired) {
				out = append(out, ev)
			}
		}
		return out
	})
}

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
// It passes run after run on the same database: every check is scoped to
// this run's listings.
func TestPostgresExpirySweep(t *testing.T) {
	url := os.Getenv("RP365_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RP365_TEST_DATABASE_URL is not set: the Postgres store is only exercised against a throwaway database")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.NewRunner(conn, migs).Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	seller := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, created_at, updated_at) VALUES ($1, $2, 'Expiry Contract', 'ACTIVE', now(), now())`,
		seller, "expiry-"+seller.String()[:12]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	expiryStoreContract(t, NewPostgresStore(pool), seller, func(id uuid.UUID) []outbox.Event {
		rows, err := pool.Query(ctx, `SELECT id, event_type, aggregate_id, payload::text, created_at FROM outbox_events
			WHERE aggregate_id = $1 AND event_type IN ($2, $3) ORDER BY (payload->>'relist_no')::int, (payload->>'days')::int DESC NULLS LAST`,
			id, EventListingExpiring, EventListingExpired)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (outbox.Event, error) {
			var ev outbox.Event
			var payload string
			err := row.Scan(&ev.ID, &ev.Type, &ev.AggregateID, &payload, &ev.CreatedAt)
			ev.Payload = json.RawMessage(payload)
			return ev, err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	})

	// The notices of the listings above, as recorded.
	var skipped int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM listing_expiry_notices n JOIN listings l ON l.id = n.listing_id
		WHERE l.seller_user_id = $1 AND n.skipped`, seller).Scan(&skipped); err != nil || skipped != 2 {
		t.Fatalf("%d skipped notices, %v (want 2: the 14-day reminders of B and of the paused listing)", skipped, err)
	}
}
