package reports

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// runAll calls every report and export once; both stores must answer.
func runAll(t *testing.T, s Store, r Range) map[string][]Count {
	t.Helper()
	ctx := context.Background()
	out := map[string][]Count{}
	for name, f := range map[string]func(context.Context, Range) ([]Count, error){
		"users": s.NewUsers, "onboarding": s.Onboarding, "listings": s.Listings, "requests": s.PropertyRequests,
		"enquiries": s.BusinessEnquiries, "leases": s.Leases, "agreements": s.Agreements, "subscriptions": s.Subscriptions,
	} {
		counts, err := f(ctx, r)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = counts
	}
	if _, err := s.Revenue(ctx, r); err != nil {
		t.Fatal(err)
	}
	started := -1
	if err := s.ExportUsers(ctx, r, 5, func(n int) { started = n }, func(UserRow) error { return nil }); err != nil || started < 0 {
		t.Fatalf("export users: %d %v", started, err)
	}
	if err := s.ExportListings(ctx, r, 5, func(int) {}, func(listings.ReportRow) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.ExportSubscriptions(ctx, r, 5, func(int) {}, func(subscriptions.Subscription) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMemoryStore(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	in, out := now.Add(-time.Hour), now.AddDate(0, -2, 0)
	s := NewMemoryStore(MemorySources{
		Users: func() []UserRow {
			return []UserRow{
				{ID: uuid.New(), AccountRoles: []string{"AGENT", "LANDLORD"}, MarketCodes: []string{"ZW"}, CreatedAt: in},
				{ID: uuid.New(), CreatedAt: in},
				{ID: uuid.New(), PlatformRoles: []string{"SUPPORT"}, CreatedAt: in},
				{ID: uuid.New(), AccountRoles: []string{"AGENT"}, CreatedAt: out},
			}
		},
		Requests: func() []Row {
			return []Row{{Group: "NEW_ENQUIRY", MarketCode: "ZW", At: in}, {Group: "NEW_ENQUIRY", MarketCode: "GB", At: in}}
		},
		Subscriptions: func() []subscriptions.Subscription {
			return []subscriptions.Subscription{{ID: uuid.New(), Status: subscriptions.StatusActive, MarketCode: "ZW", CurrencyCode: "USD", CreatedAt: in,
				Payment: &subscriptions.Payment{Amount: "30.00", ReceivedOn: "2026-11-16"}}}
		},
		Now: func() time.Time { return now },
	})
	r := Range{From: now.AddDate(0, 0, -7).Truncate(24 * time.Hour), To: now.Truncate(24 * time.Hour).Add(24 * time.Hour)}
	got := runAll(t, s, r)
	// Administrators are not counted; a person with no role is NONE.
	if len(got["users"]) != 3 {
		t.Fatalf("new users: %+v", got["users"])
	}
	r.Markets = []string{"ZW"}
	got = runAll(t, s, r)
	if len(got["requests"]) != 1 || got["requests"][0].MarketCode != "ZW" {
		t.Fatalf("market filter: %+v", got["requests"])
	}
	amounts, _ := s.Revenue(context.Background(), r)
	if len(amounts) != 1 || amounts[0].Total != "30.00" {
		t.Fatalf("revenue: %+v", amounts)
	}
}

func TestPostgresStore(t *testing.T) {
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
	defer pool.Close()
	now := time.Now().UTC()
	r := Range{From: now.AddDate(-1, 0, 0), To: now.Add(24 * time.Hour)}
	runAll(t, NewPostgresStore(pool), r)
	r.Markets = []string{"ZW"}
	runAll(t, NewPostgresStore(pool), r)
}
