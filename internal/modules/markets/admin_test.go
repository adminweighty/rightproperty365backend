package markets

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

type fakeChecker struct {
	packages map[string]bool
	legal    map[string]string
}

func (c fakeChecker) PackageActive(_ context.Context, market, code string) (bool, error) {
	return c.packages[market+"/"+code], nil
}
func (c fakeChecker) LegalSource(_ context.Context, market, kind string) (string, error) {
	return c.legal[kind], nil
}

type adminRig interface {
	Store
	AdminStore
}

// adminContract provisions a market, prepares it and activates it.
func adminContract(t *testing.T, store adminRig, code string, admin uuid.UUID, audits func() int) {
	t.Helper()
	ctx := context.Background()
	checker := fakeChecker{packages: map[string]bool{}, legal: map[string]string{"TERMS": "GLOBAL"}}
	a := NewAdmin(store, checker, nil)
	root := &auth.Principal{UserID: admin, Name: "Root", SessionScope: auth.ScopeAdmin, PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformSuperAdmin}}}
	local := &auth.Principal{UserID: admin, Name: "Local", SessionScope: auth.ScopeAdmin, PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformMarketAdmin, MarketCode: code}}}
	start := audits()

	if _, err := a.Create(ctx, local, CreateInput{Code: code, Name: "Test", CurrencyCode: "USD", Timezone: "Africa/Harare"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a market admin creates: %v", err)
	}
	var verr *ValidationError
	if _, err := a.Create(ctx, root, CreateInput{Code: "zzz", Name: "", CurrencyCode: "us", Timezone: "Mars/Base"}); !errors.As(err, &verr) || len(verr.Fields) != 4 {
		t.Fatalf("validation: %v", err)
	}
	m, err := a.Create(ctx, root, CreateInput{Code: code, Name: "  Test   Land ", CurrencyCode: "usd", Timezone: "Africa/Harare"})
	if err != nil || m.AdminStatus != AdminPlanned || m.Name != "Test Land" || m.CurrencyCode != "USD" {
		t.Fatalf("create: %+v %v", m, err)
	}
	if _, err := a.Create(ctx, root, CreateInput{Code: code, Name: "Again", CurrencyCode: "USD", Timezone: "Africa/Harare"}); !errors.Is(err, ErrExists) {
		t.Fatalf("twice: %v", err)
	}
	// PLANNED: not public.
	public, _ := store.List(ctx)
	if slices.ContainsFunc(public, func(x Market) bool { return x.Code == code }) {
		t.Fatal("a planned market is public")
	}

	// Not ready: no package, no privacy policy.
	active := AdminActive
	if _, r, err := a.Update(ctx, root, code, UpdateInput{Status: &active}); err == nil || r.Ready || len(r.Problems) != 2 {
		t.Fatalf("unready activation: %+v %v", r, err)
	}
	// The market admin prepares it but cannot activate it.
	extra := []string{"zar", "USD", "BWP", "zar"}
	notice := "  Launching   soon "
	pkg := "STARTER"
	got, _, err := a.Update(ctx, local, code, UpdateInput{ExtraCurrencies: &extra, Notice: &notice, DefaultPackageCode: &pkg})
	if err != nil || !slices.Equal(got.ExtraCurrencies, []string{"BWP", "ZAR"}) || got.Notice == nil || *got.Notice != "Launching soon" {
		t.Fatalf("prepare: %+v %v", got, err)
	}
	if _, _, err := a.Update(ctx, local, code, UpdateInput{Status: &active}); !errors.As(err, &verr) || verr.Fields["status"] == "" {
		t.Fatalf("a market admin activates: %v", err)
	}
	if _, _, err := a.Update(ctx, local, "ZW", UpdateInput{Notice: &notice}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another market: %v", err)
	}

	// Ready: active, public, with its currencies and notice.
	checker.packages[code+"/STARTER"] = true
	checker.legal["PRIVACY"] = "MARKET"
	m, r, err := a.Update(ctx, root, code, UpdateInput{Status: &active})
	if err != nil || !r.Ready || len(r.Warnings) != 1 || m.AdminStatus != AdminActive {
		t.Fatalf("activate: %+v %+v %v", m, r, err)
	}
	public, _ = store.List(ctx)
	i := slices.IndexFunc(public, func(x Market) bool { return x.Code == code })
	if i < 0 || !slices.Equal(public[i].Currencies, []string{"USD", "BWP", "ZAR"}) || public[i].Notice == nil {
		t.Fatalf("public: %+v", public)
	}
	// Suspended: hidden again; never PLANNED again.
	suspended, planned := AdminSuspended, AdminPlanned
	if _, _, err := a.Update(ctx, root, code, UpdateInput{Status: &suspended}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Update(ctx, root, code, UpdateInput{Status: &planned}); !errors.As(err, &verr) {
		t.Fatalf("back to planned: %v", err)
	}
	public, _ = store.List(ctx)
	if slices.ContainsFunc(public, func(x Market) bool { return x.Code == code }) {
		t.Fatal("a suspended market is public")
	}
	if n := audits() - start; n != 4 {
		t.Fatalf("%d audit events, want 4 (create, prepare, activate, suspend)", n)
	}
}

func TestMemoryMarketAdmin(t *testing.T) {
	s := NewMemoryStore()
	log := &adminaudit.MemoryLog{}
	s.SetAudit(log)
	adminContract(t, s, "QZ", uuid.New(), func() int { return len(log.All()) })
}

func TestPostgresMarketAdmin(t *testing.T) {
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
	admin := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, created_at, updated_at) VALUES ($1, $2, 'Market Admin', 'ACTIVE', now(), now())`,
		admin, "markets-"+admin.String()[:12]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	// A user-assigned code no earlier run took.
	code := ""
	for _, c := range []string{"QM", "QN", "QO", "QP", "QQ", "QR", "QS", "QT", "QU", "QV", "QW", "QX", "QY", "XA", "XB", "XC", "XD", "XE", "XF", "XG", "XH", "XI", "XJ"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM markets WHERE code = $1`, c).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			code = c
			break
		}
	}
	if code == "" {
		t.Skip("every test code is used on this database")
	}
	adminContract(t, NewPostgresStore(pool), code, admin, func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_events WHERE actor_user_id = $1`, admin).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
}
