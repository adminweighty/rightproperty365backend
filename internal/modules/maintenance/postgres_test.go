package maintenance

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
// Every run uses a fresh organisation, user, property and unit.
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

type fixtures struct{ org, user, property, unit uuid.UUID }

// seedRows inserts every row the requests point at (organisation, user,
// property and its unit).
func seedRows(t *testing.T, pool *pgxpool.Pool) fixtures {
	t.Helper()
	ctx := context.Background()
	fx := fixtures{org: uuid.New(), user: uuid.New(), property: uuid.New(), unit: uuid.New()}
	suffix := strings.ReplaceAll(fx.org.String(), "-", "")[:12]
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'AGENCY', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
			[]any{fx.org, "Maintenance Test " + suffix}},
		{`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Maintenance Tester', 'ACTIVE', now())`,
			[]any{fx.user, "maintenance." + suffix + "@example.test"}},
		{`INSERT INTO properties (id, organisation_id, name, address, market_code) VALUES ($1, $2, 'Avondale Court', '12 King George Road', 'ZW')`,
			[]any{fx.property, fx.org}},
		{`INSERT INTO property_units (id, property_id, label, position) VALUES ($1, $2, 'Flat 1', 1)`, []any{fx.unit, fx.property}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

func TestPostgresStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	fx := seedRows(t, pool)
	s := NewPostgresStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	book := tenancies.OrganisationBook(fx.org)
	ref, err := s.NextReference(ctx, 2026)
	if err != nil || !strings.HasPrefix(ref, "RPM-2026-") {
		t.Fatalf("reference %q: %v", ref, err)
	}
	r := Request{ID: newID(), Reference: ref, Book: book, PropertyID: fx.property, UnitID: &fx.unit, Category: "plumbing",
		CategoryLabel: "Plumbing", Priority: "high", PriorityLabel: "High", Title: "Tap", Status: StatusNew, RaisedByKind: ActorManager,
		RaisedBy: &fx.user, RaisedByName: "Maintenance Tester", StatusChangedAt: now, CreatedAt: now, UpdatedAt: now}
	created := Event{ID: newID(), RequestID: r.ID, Type: EventCreated, ActorKind: ActorManager, ActorUserID: &fx.user, ActorName: "M",
		ToStatus: StatusNew, At: now}
	notice := Notice{Type: OutboxRequestCreated, Payload: EventPayload{RequestID: r.ID, Reference: ref, ToStatus: StatusNew, RaisedBy: ActorManager,
		ActorKind: ActorManager, OccurredAt: now}}
	if err := s.Apply(ctx, Change{Create: &r, Events: []Event{created}, Notices: []Notice{notice}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Request(ctx, r.ID)
	if err != nil || got.Reference != ref || got.UnitID == nil || *got.UnitID != fx.unit || !got.Book.Same(book) || got.Provider != nil {
		t.Fatalf("request: %+v %v", got, err)
	}
	var outbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, r.ID,
		OutboxRequestCreated).Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("outbox: %d %v", outbox, err)
	}
	// A stale expectation is a conflict; a good one updates.
	next := got
	next.Status, next.Title = StatusTriaged, "Tap dripping"
	if err := s.Apply(ctx, Change{Expect: map[uuid.UUID]Expect{r.ID: {Status: StatusTriaged}}, Requests: []Request{next}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale expect: %v", err)
	}
	if err := s.Apply(ctx, Change{Expect: map[uuid.UUID]Expect{r.ID: {Status: StatusNew}}, Requests: []Request{next}}); err != nil {
		t.Fatal(err)
	}
	// Photos: the limit holds.
	for i := range MaxPhotos {
		p := Photo{ID: newID(), RequestID: r.ID, ObjectKey: "maintenance/" + strings.ReplaceAll(uuid.NewString(), "-", ""), ContentType: "image/jpeg",
			Width: 10, Height: 10, Size: 100, SHA256: strings.Repeat("a", 64), UploaderKind: ActorManager, UploadedBy: &fx.user,
			UploadedAt: now.Add(time.Duration(i) * time.Second)}
		if err := s.Apply(ctx, Change{Photos: []Photo{p}}); err != nil {
			t.Fatal(err)
		}
	}
	extra := Photo{ID: newID(), RequestID: r.ID, ObjectKey: "maintenance/extra" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		ContentType: "image/jpeg", Width: 1, Height: 1, Size: 1, SHA256: strings.Repeat("b", 64), UploaderKind: ActorTenant, UploadedAt: now}
	if err := s.Apply(ctx, Change{Photos: []Photo{extra}}); !errors.Is(err, ErrPhotoLimit) {
		t.Fatalf("photo limit: %v", err)
	}
	// A cost and its EXPENSE entry in one transaction; a failing cost keeps no entry.
	entry := ledger.Entry{ID: newID(), Book: ledger.OrganisationBook(fx.org), Type: ledger.TypeExpense, Direction: ledger.Out,
		Method: ledger.MethodCash, Amount: "85.00", Currency: "USD", EffectiveDate: now.Format(time.DateOnly), RecordedAt: now,
		AuthorUserID: &fx.user, AuthorName: "M", Status: ledger.StatusUnverified, PropertyID: &fx.property, Description: "Maintenance", Reference: ref}
	bad := Cost{ID: newID(), RequestID: uuid.New(), Amount: "85.00", Currency: "USD", Description: "x", Method: ledger.MethodCash,
		IncurredOn: now.Format(time.DateOnly), LedgerEntryID: entry.ID, RecordedAt: now}
	if err := s.Apply(ctx, Change{Ledger: &ledger.Change{Entries: []ledger.Entry{entry}}, Costs: []Cost{bad}}); err == nil {
		t.Fatal("a cost of a missing request was written")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE id = $1`, entry.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the entry of a failed cost was kept: %d %v", n, err)
	}
	good := bad
	good.RequestID = r.ID
	if err := s.Apply(ctx, Change{Ledger: &ledger.Change{Entries: []ledger.Entry{entry}}, Costs: []Cost{good},
		Touch: map[uuid.UUID]time.Time{r.ID: now.Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	costs, err := s.Costs(ctx, []uuid.UUID{r.ID})
	if err != nil || len(costs) != 1 || costs[0].Amount != "85.00" || costs[0].LedgerEntryID != entry.ID {
		t.Fatalf("costs: %+v %v", costs, err)
	}
	var property uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT property_id FROM ledger_entries WHERE id = $1`, entry.ID).Scan(&property); err != nil || property != fx.property {
		t.Fatalf("entry property: %v %v", property, err)
	}
	// The property link is posted like every other column.
	if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET property_id = NULL WHERE id = $1`, entry.ID); err == nil {
		t.Fatal("a posted entry's property link changed")
	}
	// Lists, counts and the timeline.
	list, total, counts, err := s.Requests(ctx, book, Filter{Q: "dripping"})
	if err != nil || total != 1 || len(list) != 1 || counts[StatusTriaged] != 1 || list[0].PhotoCount != MaxPhotos {
		t.Fatalf("list: %d %+v %v", total, counts, err)
	}
	events, err := s.Events(ctx, r.ID)
	if err != nil || len(events) != 1 || events[0].ToStatus != StatusNew {
		t.Fatalf("events: %+v %v", events, err)
	}
	// Account deletion unlinks the person.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ForgetUserTx(ctx, tx, fx.user, "Deleted account"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.Request(ctx, r.ID)
	if err != nil || after.RaisedBy != nil || after.RaisedByName != "Maintenance Tester" {
		t.Fatalf("after forget: %+v %v", after, err)
	}
}
