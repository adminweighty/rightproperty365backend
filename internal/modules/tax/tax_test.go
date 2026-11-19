package tax

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

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func TestTaxForByDateAndMarket(t *testing.T) {
	end := day(2027, 4, 1)
	rules := append(SeedRules(day(2026, 1, 1)),
		Rule{ID: uuid.New(), MarketCode: "ZW", TaxName: "VAT", Rate: "14.5", AppliesTo: AppliesSubscriptions, EffectiveFrom: day(2025, 1, 1),
			EffectiveTo: ptr(day(2026, 1, 1)), Status: StatusActive},
		Rule{ID: uuid.New(), MarketCode: "GB", TaxName: "VAT", Rate: "17.5", AppliesTo: AppliesSubscriptions, EffectiveFrom: day(2027, 4, 1),
			Status: StatusRetired},
		Rule{ID: uuid.New(), MarketCode: "ZW", TaxName: "VAT", Rate: "15", AppliesTo: AppliesVenueBookings, EffectiveFrom: day(2026, 6, 1),
			EffectiveTo: &end, Status: StatusActive},
	)
	cases := []struct {
		market, applies string
		at              time.Time
		want            string // "" = none
	}{
		{"ZW", AppliesSubscriptions, day(2026, 11, 19), "15"},
		{"GB", AppliesSubscriptions, day(2026, 11, 19), "20"},
		{"ZW", AppliesSubscriptions, day(2025, 6, 1), "14.5"}, // the older period
		{"ZW", AppliesSubscriptions, day(2026, 1, 1), "15"},   // effective_to is exclusive, effective_from inclusive
		{"ZW", AppliesSubscriptions, day(2024, 12, 31), ""},   // before any rule
		{"GB", AppliesSubscriptions, day(2027, 5, 1), "20"},   // a RETIRED rule never applies
		{"ZW", AppliesVenueBookings, day(2026, 5, 31), ""},    // not yet
		{"ZW", AppliesVenueBookings, day(2027, 3, 31), "15"},  // in force
		{"ZW", AppliesVenueBookings, day(2027, 4, 1), ""},     // ended
		{"ZW", AppliesListingFees, day(2026, 11, 19), ""},     // no rule for that use
		{"KE", AppliesSubscriptions, day(2026, 11, 19), ""},   // no rule for that market
		{"zw", AppliesSubscriptions, day(2026, 11, 19), ""},   // the pure function does not normalise
	}
	for _, c := range cases {
		r, ok := TaxFor(rules, c.market, c.applies, c.at)
		if (c.want == "") == ok || (ok && r.Rate != c.want) {
			t.Fatalf("TaxFor(%s, %s, %s) = %q %v; want %q", c.market, c.applies, c.at.Format(time.DateOnly), r.Rate, ok, c.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestParseRate(t *testing.T) {
	for in, want := range map[string]string{"15": "15", "15.00": "15", "020": "20", "12.5": "12.5", "0": "0", "0.0001": "0.0001", "100": "100", "100.0": "100"} {
		if got, ok := ParseRate(in); !ok || got != want {
			t.Fatalf("ParseRate(%q) = %q %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "-1", "100.01", "101", "1e2", "15%", "1.23456", "abc", "1,5"} {
		if _, ok := ParseRate(bad); ok {
			t.Fatalf("ParseRate(%q) accepted", bad)
		}
	}
}

// The development seed's rates are SAMPLE values matching the only rate
// hard-coded today (the sample showroom's 15% VAT in ZW; GB VAT 20%), ACTIVE
// only in development and staging: migration 0045 inserts no rule.
func TestSeedIsSampleAndMigrationInsertsNone(t *testing.T) {
	raw, err := migrations.FS.ReadFile("0045_markets_legal_tax.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "INSERT INTO tax_rules") {
		t.Fatal("migration 0045 must not insert tax rules")
	}
	rates := map[string]string{}
	for _, r := range SeedRules(time.Now()) {
		if !r.IsSample || r.Status != StatusActive || r.AppliesTo != AppliesSubscriptions || r.Inclusive || r.Notes != SampleNote {
			t.Fatalf("seed: %+v", r)
		}
		rates[r.MarketCode] = r.Rate
	}
	if rates["ZW"] != "15" || rates["GB"] != "20" || len(rates) != 2 {
		t.Fatalf("seed rates: %v", rates)
	}
}

func financeAdmin(id uuid.UUID) *auth.Principal {
	return &auth.Principal{UserID: id, Name: "Fiona Finance", SessionScope: auth.ScopeAdmin,
		PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformFinanceAdmin}}}
}

// storeContract runs the administration over a store. market is a country
// with no rules for LISTING_FEES (unique per run is not needed: the rules
// created are retired at the end).
func storeContract(t *testing.T, store Store, actor uuid.UUID, audits func() int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	s := NewService(store, nil, func() time.Time { return now })
	p := financeAdmin(actor)
	from := day(2030, 1, 1).Add(time.Duration(now.UnixNano()%1000) * time.Hour) // distinct per run
	to := from.Add(365 * 24 * time.Hour)
	before := audits()

	r, err := s.Create(ctx, p, Input{MarketCode: ptr("zw"), TaxName: ptr("VAT"), Rate: ptr("15.00"), AppliesTo: ptr("listing_fees"),
		EffectiveFrom: &from, EffectiveTo: Optional[time.Time]{Set: true, Value: &to}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Rate != "15" || r.MarketCode != "ZW" || r.AppliesTo != AppliesListingFees || r.Status != StatusActive {
		t.Fatalf("created: %+v", r)
	}
	// Overlap refused; adjacent accepted.
	mid := from.Add(24 * time.Hour)
	if _, err := s.Create(ctx, p, Input{MarketCode: ptr("ZW"), TaxName: ptr("VAT"), Rate: ptr("16"), AppliesTo: ptr(AppliesListingFees),
		EffectiveFrom: &mid}); !errors.Is(err, ErrOverlap) {
		t.Fatalf("overlap: %v", err)
	}
	next, err := s.Create(ctx, p, Input{MarketCode: ptr("ZW"), TaxName: ptr("VAT"), Rate: ptr("16"), AppliesTo: ptr(AppliesListingFees),
		EffectiveFrom: &to})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.TaxFor(ctx, "ZW", AppliesListingFees, mid); err != nil || !ok || got.ID != r.ID {
		t.Fatalf("lookup first period: %+v %v %v", got, ok, err)
	}
	if got, ok, _ := s.TaxFor(ctx, "zw", AppliesListingFees, to); !ok || got.ID != next.ID || got.Rate != "16" {
		t.Fatalf("lookup second period: %+v", got)
	}
	// Extending the first into the second is an overlap; a PATCH is audited.
	later := to.Add(time.Hour)
	if _, err := s.Update(ctx, p, r.ID, Input{EffectiveTo: Optional[time.Time]{Set: true, Value: &later}}); !errors.Is(err, ErrOverlap) {
		t.Fatalf("update overlap: %v", err)
	}
	if r, err = s.Update(ctx, p, r.ID, Input{Rate: ptr("15.5"), IsSample: ptr(true)}); err != nil || r.Rate != "15.5" || !r.IsSample {
		t.Fatalf("update: %+v %v", r, err)
	}
	if _, err := s.Update(ctx, p, r.ID, Input{MarketCode: ptr("GB")}); err == nil {
		t.Fatal("market change accepted")
	}
	for _, id := range []uuid.UUID{r.ID, next.ID} {
		if got, err := s.Retire(ctx, p, id); err != nil || got.Status != StatusRetired {
			t.Fatalf("retire: %v", err)
		}
	}
	if _, err := s.Retire(ctx, p, r.ID); !errors.Is(err, ErrRetired) {
		t.Fatalf("retire twice: %v", err)
	}
	if _, ok, _ := s.TaxFor(ctx, "ZW", AppliesListingFees, mid); ok {
		t.Fatal("a retired rule applies")
	}
	if _, err := s.Get(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not found: %v", err)
	}
	// Created x2, updated x1, retired x2.
	if got := audits() - before; got != 5 {
		t.Fatalf("audit records: %d", got)
	}
}

func TestStoreContractMemory(t *testing.T) {
	log := &adminaudit.MemoryLog{}
	store := NewMemoryStore(SeedRules(time.Now()), log)
	storeContract(t, store, uuid.New(), func() int { return len(log.All()) })
	// The seeded sample rules are still in force.
	s := NewService(store, nil, nil)
	if got, ok, _ := s.TaxFor(context.Background(), "GB", AppliesSubscriptions, day(2026, 11, 19)); !ok || got.Rate != "20" || !got.IsSample {
		t.Fatalf("seed GB: %+v", got)
	}
}

func TestStoreContractPostgres(t *testing.T) {
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
	// Every row a foreign key names: the audit actor is a users row.
	actor := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Fiona Finance', 'ACTIVE', now())`,
		actor, "tax."+actor.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_events WHERE target_type = 'TAX_RULE'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	storeContract(t, NewPostgresStore(pool), actor, count)
}
