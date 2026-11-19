package crm

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/migrations"
)

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
func openTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	return pool
}

// storeContract runs the same checks on both stores. org is an organisation
// and user a person who may act in it (real rows for Postgres).
func storeContract(t *testing.T, s Store, org, user uuid.UUID, outbox func() int) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	book := OrganisationBook(org)
	mk := func(name, stage string, emails, phones, tags []string, created time.Time) Contact {
		return Contact{ID: newID(), Book: book, Name: name, Type: "buyer", Stage: stage, Source: "walk-in", OwnerUserID: &user, CreatedBy: &user,
			Emails: emails, Phones: phones, Tags: tags, CreatedAt: created, UpdatedAt: created,
			Marketing: &Marketing{Granted: true, GrantedAt: created, Source: "Form", RecordedBy: &user}}
	}
	a := mk("Alpha", StageNew, []string{"alpha@example.com"}, []string{"+263771234567"}, []string{"vip", "harare"}, now)
	b := mk("Bravo", StageContacted, []string{"bravo@example.com"}, nil, []string{"vip"}, now.Add(time.Second))
	listing := uuid.New()
	if err := s.Apply(ctx, Change{Contacts: []Contact{a, b},
		Notes:     []Note{{ID: newID(), ContactID: b.ID, Text: "On Bravo", AuthorUserID: &user, AuthorName: "U", CreatedAt: now}},
		Reminders: []Reminder{{ID: newID(), ContactID: b.ID, DueAt: now.Add(-time.Minute), Text: "Due", CreatedBy: &user, CreatedAt: now}},
		Events:    []Event{{ID: newID(), ContactID: b.ID, Kind: EventCreated, ActorUserID: &user, Data: map[string]any{"stage": "NEW"}, At: now}},
		Links: []Link{{ContactID: b.ID, Kind: KindListing, ItemID: listing, LinkedAt: now}, {ContactID: a.ID, Kind: KindListing, ItemID: listing, LinkedAt: now},
			{ContactID: b.ID, Kind: KindAppointment, ItemID: uuid.New(), Auto: true, LinkedAt: now}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, b.ID)
	if err != nil || got.Name != "Bravo" || !slices.Equal(got.Tags, []string{"vip"}) || got.OpenReminders != 1 || got.Marketing == nil ||
		!got.Marketing.Granted || !slices.Equal(got.ListingIDs, []uuid.UUID{listing}) || !got.CreatedAt.Equal(b.CreatedAt) {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := s.Get(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	rows, total, err := s.List(ctx, book, ListFilter{Sort: SortCreated, Now: now})
	if err != nil || total != 2 || rows[0].ID != b.ID || rows[0].DueReminders != 1 || rows[1].DueReminders != 0 {
		t.Fatalf("list: %+v %d %v", rows, total, err)
	}
	for name, tc := range map[string]struct {
		f    ListFilter
		want int
	}{
		"q":     {ListFilter{Q: "ALP"}, 1},
		"phone": {ListFilter{Q: "0771234"}, 1},
		"email": {ListFilter{Q: "bravo@"}, 1},
		"tag":   {ListFilter{Tag: "VIP"}, 2},
		"stage": {ListFilter{Stage: StageContacted}, 1},
		"owner": {ListFilter{Owner: &user}, 2},
		"none":  {ListFilter{Unowned: true}, 0},
		"due":   {ListFilter{ReminderDueBefore: &now}, 1},
		"page":  {ListFilter{Limit: 1, Offset: 5}, 0},
	} {
		got, total, err := s.List(ctx, book, tc.f)
		if err != nil || (name != "page" && (len(got) != tc.want || total != tc.want)) || (name == "page" && total != 2) {
			t.Errorf("%s: %d rows, total %d, %v", name, len(got), total, err)
		}
	}
	counts, err := s.StageCounts(ctx, book, ListFilter{Stage: StageContacted})
	if err != nil || counts[StageNew] != 1 || counts[StageContacted] != 1 {
		t.Fatalf("stage counts: %v %v", counts, err)
	}
	tags, err := s.Tags(ctx, book)
	if err != nil || len(tags) != 2 || tags[0] != (TagCount{Tag: "vip", Count: 2}) {
		t.Fatalf("tags: %v %v", tags, err)
	}
	match, err := s.Matching(ctx, book, []string{"bravo@example.com"}, []string{"+263771234567"})
	if err != nil || len(match) != 2 {
		t.Fatalf("matching: %v %v", match, err)
	}
	if other, _ := s.Matching(ctx, OrganisationBook(uuid.New()), []string{"bravo@example.com"}, nil); len(other) != 0 {
		t.Fatal("matched across books")
	}
	// Optimistic concurrency.
	stale := a
	stale.Name = "Stale"
	if err := s.Apply(ctx, Change{Expect: map[uuid.UUID]time.Time{a.ID: now.Add(time.Hour)}, Contacts: []Contact{stale}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write: %v", err)
	}
	// Merge Bravo into Alpha.
	later := now.Add(time.Minute)
	keep, tomb := a, b
	keep.Emails, keep.UpdatedAt = append(keep.Emails, "bravo@example.com"), later
	tomb.MergedInto, tomb.UpdatedAt = &a.ID, later
	audit := orgaudit.New(org, user, "U", orgaudit.ActionContactsMerged, orgaudit.TargetContact, a.ID, "Alpha", nil, nil, later)
	if err := s.Apply(ctx, Change{Expect: map[uuid.UUID]time.Time{a.ID: a.UpdatedAt, b.ID: b.UpdatedAt}, Contacts: []Contact{keep, tomb},
		Merge:  &Merge{Keep: a.ID, From: []uuid.UUID{b.ID}},
		Events: []Event{{ID: newID(), ContactID: b.ID, Kind: EventMergedInto, At: later}}, Audit: []orgaudit.Event{audit}}); err != nil {
		t.Fatal(err)
	}
	notes, _ := s.Notes(ctx, a.ID)
	rems, _ := s.Reminders(ctx, a.ID)
	links, _ := s.Links(ctx, a.ID)
	events, _ := s.Events(ctx, a.ID)
	tombEvents, _ := s.Events(ctx, b.ID)
	if len(notes) != 1 || notes[0].MergedFrom == nil || *notes[0].MergedFrom != b.ID || len(rems) != 1 || len(links) != 2 || len(events) != 1 ||
		len(tombEvents) != 1 || tombEvents[0].Kind != EventMergedInto {
		t.Fatalf("after merge: notes %v reminders %v links %v events %v / %v", notes, rems, links, events, tombEvents)
	}
	if rows, total, _ := s.List(ctx, book, ListFilter{}); total != 1 || rows[0].ID != a.ID {
		t.Fatalf("tombstone listed: %+v", rows)
	}
	if got, _ := s.Get(ctx, b.ID); got.MergedInto == nil || *got.MergedInto != a.ID || !slices.Equal(got.Emails, []string{"bravo@example.com"}) {
		t.Fatalf("tombstone: %+v", got)
	}
	// Reminders: due ones, claimed once.
	due, err := s.DueReminders(ctx, book, now)
	if err != nil || len(due) != 1 || due[0].ContactName != "Alpha" {
		t.Fatalf("due: %+v %v", due, err)
	}
	before := outbox()
	for range 2 {
		if _, err := s.ClaimDueReminders(ctx, now, 10); err != nil {
			t.Fatal(err)
		}
	}
	if outbox()-before != 1 {
		t.Fatalf("%d CONTACT_REMINDER_DUE events", outbox()-before)
	}
	// Unlink.
	if err := s.Apply(ctx, Change{Unlinks: []Link{{ContactID: a.ID, Kind: KindListing, ItemID: listing}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, a.ID); len(got.ListingIDs) != 0 {
		t.Fatalf("unlinked: %v", got.ListingIDs)
	}
}

func TestMemoryStoreContract(t *testing.T) {
	s := NewMemoryStore()
	storeContract(t, s, uuid.New(), uuid.New(), func() int { return len(s.Outbox()) })
}

func TestPostgresStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	org, user := uuid.New(), uuid.New()
	suffix := org.String()[:8]
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'AGENCY', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
		org, "CRM Test "+suffix)
	exec(`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'CRM Tester', 'ACTIVE', now())`, user, "crm."+suffix+"@example.test")
	s := NewPostgresStore(pool)
	storeContract(t, s, org, user, func() int {
		var n int
		// Only this test's book: ClaimDueReminders claims across every book,
		// so reminders an earlier run left due would otherwise be counted too.
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type = $1
			AND aggregate_id IN (SELECT id FROM contacts WHERE organisation_id = $2)`, EventReminderDue, org).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM organisation_audit_events WHERE organisation_id = $1 AND action = 'CONTACTS_MERGED'`, org).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("%d audit rows, %v", audits, err)
	}
	// The item queries are valid SQL and scoped to the book.
	items := NewPostgresItems(pool)
	for _, kind := range ItemKinds() {
		if _, err := items.Item(ctx, kind, uuid.New()); !errors.Is(err, ErrItemNotFound) {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	for _, b := range []Book{OrganisationBook(org), PersonalBook(user)} {
		if got, err := items.Candidates(ctx, b, []string{"nobody@example.test"}, []string{"1234567"}); err != nil || len(got) != 0 {
			t.Fatalf("candidates: %v %v", got, err)
		}
	}
}
