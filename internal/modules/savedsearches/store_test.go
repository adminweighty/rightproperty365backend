package savedsearches

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

type fixedChecker struct{}

func (fixedChecker) Canonical(_ context.Context, q string) (string, map[string]string, error) {
	if q == "bad=1" {
		return "", map[string]string{"deal": "is not known"}, nil
	}
	return q, nil, nil
}

type fakeMatcher struct {
	alert Alert
	calls int
	after []time.Time
}

func (m *fakeMatcher) Match(_ context.Context, _ string, after, _ time.Time) (Alert, error) {
	m.calls++
	m.after = append(m.after, after)
	return m.alert, nil
}

// contract runs the store rules through the Service. user exists;
// alerts counts the alert events written for a search.
func contract(t *testing.T, store Store, user uuid.UUID, alerts func(id uuid.UUID) int) {
	t.Helper()
	ctx := context.Background()
	clock := time.Now().UTC().Truncate(time.Second)
	m := &fakeMatcher{}
	svc := NewService(Deps{Store: store, Checker: fixedChecker{}, Matcher: m, Now: func() time.Time { return clock }})
	str := func(s string) *string { return &s }

	if _, err := svc.Create(ctx, user, Input{Name: str(" "), Query: str("bad=1"), Frequency: str("HOURLY")}); err == nil {
		t.Fatal("want a validation error")
	} else {
		var verr *ValidationError
		if !errors.As(err, &verr) || verr.Fields["name"] == "" || verr.Fields["query.deal"] == "" || verr.Fields["frequency"] == "" {
			t.Fatalf("fields: %v", err)
		}
	}
	daily, err := svc.Create(ctx, user, Input{Name: str("Houses  in   Borrowdale"), Query: str("deal=rent&city=harare")})
	if err != nil || daily.Name != "Houses in Borrowdale" || daily.Frequency != FrequencyDaily || daily.UnsubscribeToken == "" {
		t.Fatalf("create: %+v %v", daily, err)
	}
	instant, err := svc.Create(ctx, user, Input{Name: str("Flats"), Query: str("deal=rent&property_type=flat"), Frequency: str(FrequencyInstant)})
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(ctx, user)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if _, err := svc.Update(ctx, uuid.New(), daily.ID, Input{Name: str("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's: %v", err)
	}

	// Nothing is due yet; ten minutes on, the instant one runs.
	if n, err := svc.Sweep(ctx); err != nil || n != 0 || m.calls != 0 {
		t.Fatalf("nothing due: %d %d %v", n, m.calls, err)
	}
	clock = clock.Add(11 * time.Minute)
	m.alert = Alert{Total: 12, More: true, Lines: make([]Line, 12)}
	for i := range m.alert.Lines {
		m.alert.Lines[i] = Line{ListingID: uuid.New(), Slug: "home", Title: "Home", Place: "Avondale, Harare", Price: "USD 500"}
	}
	if n, err := svc.Sweep(ctx); err != nil || n != 1 || m.calls != 1 || !m.after[0].Equal(instant.LastCheckedAt) {
		t.Fatalf("instant: %d %d %v %v", n, m.calls, m.after, err)
	}
	if alerts(instant.ID) != 1 || alerts(daily.ID) != 0 {
		t.Fatalf("alerts: %d %d", alerts(instant.ID), alerts(daily.ID))
	}
	got, _ := store.Get(ctx, instant.ID)
	if !got.LastCheckedAt.Equal(clock) || got.LastAlertAt == nil || got.NextCheckAt == nil || !got.NextCheckAt.Equal(clock.Add(10*time.Minute)) {
		t.Fatalf("after the run: %+v", got)
	}
	// Run again at once: not due. Nothing new later: no alert.
	if n, _ := svc.Sweep(ctx); n != 0 || m.calls != 1 {
		t.Fatal("ran twice")
	}
	clock = clock.Add(11 * time.Minute)
	m.alert = Alert{}
	if n, _ := svc.Sweep(ctx); n != 0 || m.calls != 2 || alerts(instant.ID) != 1 {
		t.Fatalf("no new listings: %d %d", n, alerts(instant.ID))
	}

	// Unsubscribe switches it off, once and for all; back on starts from now.
	off, err := svc.Unsubscribe(ctx, instant.UnsubscribeToken)
	if err != nil || off.Frequency != FrequencyOff || off.NextCheckAt != nil {
		t.Fatalf("unsubscribe: %+v %v", off, err)
	}
	if _, err := svc.Unsubscribe(ctx, "not-a-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("bad token: %v", err)
	}
	clock = clock.Add(time.Hour)
	on, err := svc.Update(ctx, user, instant.ID, Input{Frequency: str(FrequencyWeekly)})
	if err != nil || !on.LastCheckedAt.Equal(clock) || on.NextCheckAt == nil || !on.NextCheckAt.Equal(clock.Add(7*24*time.Hour)) {
		t.Fatalf("back on: %+v %v", on, err)
	}

	// The daily one, a day on.
	clock = clock.Add(24 * time.Hour)
	m.alert = Alert{Total: 1, Lines: []Line{{ListingID: uuid.New(), Slug: "house", Title: "House"}}}
	if n, _ := svc.Sweep(ctx); n != 1 || alerts(daily.ID) != 1 {
		t.Fatalf("daily: %d %d", n, alerts(daily.ID))
	}

	// The limit, then delete.
	for i := 2; i < MaxPerUser; i++ {
		if _, err := svc.Create(ctx, user, Input{Name: str("More"), Query: str("deal=sale")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Create(ctx, user, Input{Name: str("One too many"), Query: str("deal=sale")}); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit: %v", err)
	}
	if err := svc.Delete(ctx, uuid.New(), daily.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete another's: %v", err)
	}
	if err := svc.Delete(ctx, user, daily.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, daily.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}

func TestMemoryStoreContract(t *testing.T) {
	s := NewMemoryStore()
	contract(t, s, uuid.New(), func(id uuid.UUID) int {
		n := 0
		for _, ev := range s.Outbox() {
			if ev.AggregateID == id && ev.Type == EventMatched {
				n++
			}
		}
		return n
	})
}

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

func TestPostgresStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	user := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, email_verified_at, password_hash) VALUES ($1, $2, 'Search Tester', 'ACTIVE', now(), 'x')`,
		user, "searches."+user.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	// Other runs' searches on this database must not be claimed by this one.
	if _, err := pool.Exec(ctx, `UPDATE saved_searches SET frequency = 'OFF', next_check_at = NULL WHERE user_id <> $1`, user); err != nil {
		t.Fatal(err)
	}
	contract(t, NewPostgresStore(pool), user, func(id uuid.UUID) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, id, EventMatched).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
}
