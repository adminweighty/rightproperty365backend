package subscriptions

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// storeContract is what every Store must do; the memory and the Postgres
// store run the same checks.
func storeContract(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	n, err := s.NextInvoiceNumber(ctx, 2026)
	if err != nil || !regexp.MustCompile(`^RPS-2026-[0-9]{6,}$`).MatchString(n) {
		t.Fatalf("invoice number %q: %v", n, err)
	}

	ten := 10
	plan := Plan{ID: uuid.New(), Name: "Contract plan", SubscriberKind: KindIndividual, EligibleRoles: []string{"AGENT"},
		Features: []Feature{{Code: FeatureMaxActiveListings, Limit: &ten}, {Code: FeatureMaxTeamMembers}}, Benefits: []string{"One"},
		Prices: []Price{{CurrencyCode: "USD", Period: PeriodQuarterly, Amount: "30.00"}, {CurrencyCode: "USD", Period: PeriodYearly, Amount: "100.00"}},
		Status: PlanActive, CreatedAt: now, UpdatedAt: now}
	audit := adminaudit.New(uuid.Nil, "Test", adminaudit.ActionPlanCreated, adminaudit.TargetPlan, plan.ID.String(), plan.Name, "", nil, nil, now)
	if err := s.CreatePlan(ctx, plan, audit); err != nil {
		t.Fatal(err)
	}
	got, err := s.Plan(ctx, plan.ID)
	if err != nil || got.Name != plan.Name || len(got.Prices) != 2 || len(got.Features) != 2 || got.Features[1].Limit != nil || *got.Features[0].Limit != 10 {
		t.Fatalf("plan: %+v %v", got, err)
	}
	if _, err := s.Plan(ctx, uuid.New()); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("unknown plan: %v", err)
	}
	changed, err := s.ChangePlan(ctx, plan.ID, func(p *Plan) (adminaudit.Event, error) {
		p.Prices = []Price{{CurrencyCode: "GBP", Period: PeriodYearly, Amount: "80.00"}}
		p.Status, p.RetiredAt = PlanRetired, &now
		return adminaudit.New(uuid.Nil, "Test", adminaudit.ActionPlanRetired, adminaudit.TargetPlan, p.ID.String(), p.Name, "", nil, nil, now), nil
	})
	if err != nil || changed.Status != PlanRetired || len(changed.Prices) != 1 {
		t.Fatalf("change plan: %+v %v", changed, err)
	}
	active, err := s.Plans(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range active {
		if p.ID == plan.ID {
			t.Fatal("a retired plan is not listed without include_retired")
		}
	}

	subscriber := uuid.New()
	x := Subscription{ID: uuid.New(), InvoiceNumber: n, PlanID: plan.ID, PlanName: plan.Name, Period: PeriodQuarterly, Status: StatusPendingPayment,
		SubscriberKind: KindIndividual, SubscriberID: subscriber, SubscriberName: "Sub", MarketCode: "ZW", CurrencyCode: "USD", Amount: "30.00",
		CreatedAt: now, UpdatedAt: now}
	if err := s.CreateSubscription(ctx, x, NewEvent(x.ID, ActionRequested, ActorSubscriber, uuid.Nil, "Sub", "", now), nil); err != nil {
		t.Fatal(err)
	}
	n2, _ := s.NextInvoiceNumber(ctx, 2026)
	dup := x
	dup.ID, dup.InvoiceNumber = uuid.New(), n2
	if err := s.CreateSubscription(ctx, dup, NewEvent(dup.ID, ActionRequested, ActorSubscriber, uuid.Nil, "Sub", "", now), nil); !errors.Is(err, ErrPendingExists) {
		t.Fatalf("a second pending subscription: %v", err)
	}
	sentinel := errors.New("refused")
	if err := s.CreateSubscription(ctx, dup, Event{}, func(existing []Subscription) error {
		if len(existing) != 1 || existing[0].ID != x.ID {
			t.Fatalf("existing: %+v", existing)
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("check: %v", err)
	}

	list, total, err := s.Subscriptions(ctx, Filter{Subscribers: []uuid.UUID{subscriber}, Status: StatusPendingPayment})
	if err != nil || total != 1 || list[0].ID != x.ID || list[0].Amount != "30.00" {
		t.Fatalf("list: %+v %d %v", list, total, err)
	}
	if _, total, _ := s.Subscriptions(ctx, Filter{Subscribers: []uuid.UUID{subscriber}, CreatedFrom: now.Add(time.Hour)}); total != 0 {
		t.Fatalf("created_from: %d", total)
	}
	if _, total, _ := s.Subscriptions(ctx, Filter{Subscribers: []uuid.UUID{subscriber}, Markets: []string{"GB"}}); total != 0 {
		t.Fatalf("markets: %d", total)
	}

	start, end := now.Add(-2*time.Hour), now.Add(-time.Hour)
	updated, err := s.ChangeSubscription(ctx, x.ID, func(cur *Subscription) (Change, error) {
		cur.Status, cur.StartsAt, cur.EndsAt, cur.ActivatedAt, cur.UpdatedAt = StatusActive, &start, &end, &now, now
		cur.Payment = &Payment{Reference: "BANK-1", Amount: "30.00", ReceivedOn: "2026-11-01", Note: "n", RecordedByName: "Fin"}
		return Change{Events: []Event{NewEvent(cur.ID, ActionActivated, ActorAdmin, uuid.Nil, "Fin", "", now)}, Outbox: EventActivated}, nil
	})
	if err != nil || updated.Status != StatusActive || updated.Payment == nil {
		t.Fatalf("activate: %+v %v", updated, err)
	}
	again, err := s.Subscription(ctx, x.ID)
	if err != nil || again.Payment == nil || again.Payment.ReceivedOn != "2026-11-01" || again.Payment.Amount != "30.00" || !again.EndsAt.Equal(end) {
		t.Fatalf("reload: %+v %v", again, err)
	}
	if _, err := s.ChangeSubscription(ctx, uuid.New(), func(*Subscription) (Change, error) { return Change{}, nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown subscription: %v", err)
	}

	expired, err := s.ExpireDue(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range expired {
		found = found || (e.ID == x.ID && e.Status == StatusExpired && e.ExpiredAt != nil)
	}
	if !found {
		t.Fatalf("expired: %+v", expired)
	}
	events, err := s.Events(ctx, x.ID)
	if err != nil || len(events) != 3 || events[2].Action != ActionExpired {
		t.Fatalf("events: %+v %v", events, err)
	}
	if n, err := s.ActiveListings(ctx, KindIndividual, subscriber, now); err != nil || n != 0 {
		t.Fatalf("active listings: %d %v", n, err)
	}
}

func TestMemoryStore(t *testing.T) {
	s := NewMemoryStore(nil)
	storeContract(t, s)
	if len(s.Outbox()) != 3 { // requested, activated, expired
		t.Fatalf("outbox: %+v", s.Outbox())
	}
	if len(s.AuditLog().All()) != 2 {
		t.Fatalf("audit: %+v", s.AuditLog().All())
	}
}

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

func TestPostgresStore(t *testing.T) {
	pool := openTestDatabase(t)
	storeContract(t, NewPostgresStore(pool))
	if _, err := pool.Exec(context.Background(), `UPDATE subscription_events SET note = 'changed' WHERE action = 'EXPIRED'`); err == nil {
		t.Fatal("subscription events are immutable")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE admin_audit_events SET reason = 'changed'`); err == nil {
		t.Fatal("admin audit events are immutable")
	}
}

func TestAmounts(t *testing.T) {
	for in, want := range map[string]string{"25": "25.00", "25.5": "25.50", "0.01": "0.01", " 10.10 ": "10.10"} {
		if got, ok := ParseAmount(in); !ok || got != want {
			t.Fatalf("ParseAmount(%q) = %q, %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "0", "-1", "1.234", "1e3", "abc", "1,000"} {
		if _, ok := ParseAmount(bad); ok {
			t.Fatalf("ParseAmount(%q) must fail", bad)
		}
	}
	if CompareAmounts("99.99", "100.00") >= 0 || CompareAmounts("100", "100.00") != 0 || SumAmounts("30.00", "100", "0.5") != "130.50" {
		t.Fatal("amount arithmetic")
	}
	start := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	if End(PeriodQuarterly, start) != time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC) || End(PeriodYearly, start) != time.Date(2027, 11, 30, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("periods: %v %v", End(PeriodQuarterly, start), End(PeriodYearly, start))
	}
}
