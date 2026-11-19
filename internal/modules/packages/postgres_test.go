package packages

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
// Every run uses a fresh organisation, users, listings and package codes.
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

type fixtures struct {
	org, owner, admin uuid.UUID
	suffix            string
}

// seedRows inserts every row the packages and listings point at: an agency
// in ZW, a direct owner and an administrator.
func seedRows(t *testing.T, pool *pgxpool.Pool) fixtures {
	t.Helper()
	ctx := context.Background()
	fx := fixtures{org: uuid.New(), owner: uuid.New(), admin: uuid.New()}
	fx.suffix = strings.ToUpper(strings.ReplaceAll(fx.org.String(), "-", "")[:8])
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'AGENCY', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
		fx.org, "Packages Estates "+fx.suffix)
	for _, u := range []struct {
		id   uuid.UUID
		name string
	}{{fx.owner, "Package Owner"}, {fx.admin, "Package Admin"}} {
		exec(`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, $3, 'ACTIVE', now())`,
			u.id, strings.ToLower(strings.ReplaceAll(u.name, " ", "."))+"."+strings.ToLower(fx.suffix)+"@example.test", u.name)
	}
	return fx
}

// listing inserts a listing of the organisation in ZW with the status and,
// for PUBLISHED, an expiry.
func listing(t *testing.T, pool *pgxpool.Pool, org uuid.UUID, status string, expires *time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO listings (id, market_id, title, status, price_amount, currency_code, organisation_id,
			published_at, expires_at)
		VALUES ($1, (SELECT id FROM markets WHERE code = 'ZW'), 'Quota test', $2::text::listing_status, 100, 'USD', $3, $4, $5)`,
		id, status, org, expires, expires); err != nil {
		t.Fatal(err)
	}
	return id
}

func testPackage(fx fixtures, code string, limit int, def bool, now time.Time) Package {
	return Package{ID: uuid.New(), MarketCode: "ZW", Code: code + fx.suffix, Name: code, ListingLimit: limit, Active: true, Default: def,
		Sort: 10, CreatedBy: &fx.admin, CreatedAt: now, UpdatedAt: now,
		Prices: []Price{{Term: TermMonthly, Amount: "25.00", Currency: "USD"}, {Term: TermAnnual, Amount: "250.00", Currency: "USD", Sample: true}}}
}

func audit(fx fixtures, action, target string, now time.Time) adminaudit.Event {
	return adminaudit.New(fx.admin, "Package Admin", action, adminaudit.TargetListingPackage, target, "test", "", nil, nil, now)
}

func TestPostgresStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	fx := seedRows(t, pool)
	s := NewPostgresStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	today := Day(now)
	// Leave ZW's real default alone: this test's packages are made default
	// only while it runs, then the original default (if any) is restored.
	var originalDefault *uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM listing_packages WHERE market_code = 'ZW' AND is_default`).Scan(&originalDefault)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM listing_package_assignments WHERE organisation_id = $1 OR user_id = $2`, fx.org, fx.owner)
		_, _ = pool.Exec(ctx, `DELETE FROM listing_packages WHERE code LIKE '%' || $1`, fx.suffix)
		if originalDefault != nil {
			_, _ = pool.Exec(ctx, `UPDATE listing_packages SET is_default = true WHERE id = $1`, *originalDefault)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM listings WHERE organisation_id = $1`, fx.org)
	})

	// Create, read, duplicate code.
	small := testPackage(fx, "SMALL", 1, false, now)
	if err := s.CreatePackage(ctx, small, audit(fx, adminaudit.ActionPackageCreated, small.ID.String(), now)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Package(ctx, small.ID)
	if err != nil || got.Code != small.Code || len(got.Prices) != 2 || got.Prices[0].Term != TermMonthly || got.Prices[0].Amount != "25.00" ||
		got.Prices[1].Term != TermAnnual || !got.Prices[1].Sample || got.CreatedBy == nil || *got.CreatedBy != fx.admin {
		t.Fatalf("read back: %+v %v", got, err)
	}
	dup := testPackage(fx, "SMALL", 5, false, now)
	if err := s.CreatePackage(ctx, dup, audit(fx, adminaudit.ActionPackageCreated, dup.ID.String(), now)); !errors.Is(err, ErrCodeTaken) {
		t.Fatalf("duplicate code: %v", err)
	}
	if _, err := s.Package(ctx, uuid.New()); !errors.Is(err, ErrPackageNotFound) {
		t.Fatalf("unknown package: %v", err)
	}

	// The default moves; one per market.
	def := testPackage(fx, "DEF", 3, true, now)
	if err := s.CreatePackage(ctx, def, audit(fx, adminaudit.ActionPackageCreated, def.ID.String(), now)); err != nil {
		t.Fatal(err)
	}
	changed, err := s.ChangePackage(ctx, small.ID, func(p *Package) (adminaudit.Event, error) {
		p.Default, p.Name, p.UpdatedAt = true, "Small", now
		p.Prices = []Price{{Term: TermQuarterly, Amount: "60.00", Currency: "USD"}}
		return audit(fx, adminaudit.ActionPackageUpdated, p.ID.String(), now), nil
	})
	if err != nil || !changed.Default || changed.Name != "Small" || len(changed.Prices) != 1 {
		t.Fatalf("change: %+v %v", changed, err)
	}
	if d, _ := s.Package(ctx, def.ID); d.Default {
		t.Fatal("the old default kept its flag")
	}
	list, err := s.Packages(ctx, PackageFilter{Markets: []string{"ZW"}, IncludeInactive: true})
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for _, p := range list {
		if p.Default {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("defaults in ZW: %d", defaults)
	}
	if none, _ := s.Packages(ctx, PackageFilter{Markets: []string{}}); len(none) != 0 {
		t.Fatalf("an empty market list matches nothing: %d", len(none))
	}

	// Resolve: the default, then an assignment, a scheduled one, replacement.
	acct := Account{Kind: KindOrganisation, ID: fx.org}
	r, err := s.Resolve(ctx, acct, "ZW", now)
	if err != nil || r.Basis != BasisDefault || r.Package == nil || r.Package.ID != small.ID {
		t.Fatalf("default: %+v %v", r, err)
	}
	a1 := Assignment{ID: uuid.New(), Account: acct, MarketCode: "ZW", PackageID: def.ID, StartsOn: today, Source: SourceAdmin,
		Note: "first", AssignedBy: &fx.admin, AssignedByName: "Package Admin", CreatedAt: now}
	assignAudit := func([]Assignment) adminaudit.Event {
		return adminaudit.New(fx.admin, "Package Admin", adminaudit.ActionPackageAssigned, adminaudit.TargetOrganisation, fx.org.String(), "x", "", nil, nil, now)
	}
	if _, err := s.Assign(ctx, a1, assignAudit); err != nil {
		t.Fatal(err)
	}
	later := today.AddDate(0, 0, 10)
	a2 := a1
	a2.ID, a2.StartsOn, a2.PackageID, a2.Note = uuid.New(), later, small.ID, "later"
	res, err := s.Assign(ctx, a2, assignAudit)
	if err != nil || len(res.Replaced) != 1 || res.Replaced[0].ID != a1.ID {
		t.Fatalf("schedule: %+v %v", res, err)
	}
	r, err = s.Resolve(ctx, acct, "ZW", now)
	if err != nil || r.Basis != BasisAssigned || r.Package.ID != def.ID || r.Assignment.EndsOn == nil ||
		!r.Assignment.EndsOn.Equal(later.AddDate(0, 0, -1)) || r.Scheduled == nil || r.Scheduled.ID != a2.ID || r.ScheduledPackage.ID != small.ID {
		t.Fatalf("assigned + scheduled: %+v %v", r, err)
	}
	// Assigning from today cancels both (the first started today).
	a3 := a1
	a3.ID, a3.Note = uuid.New(), "replacement"
	if res, err := s.Assign(ctx, a3, assignAudit); err != nil || len(res.Replaced) != 2 {
		t.Fatalf("replace: %+v %v", res, err)
	}
	hist, err := s.Assignments(ctx, acct)
	if err != nil || len(hist) != 3 {
		t.Fatalf("history: %d %v", len(hist), err)
	}
	statuses := map[uuid.UUID]string{}
	for _, a := range hist {
		statuses[a.ID] = a.Status(today)
	}
	if statuses[a1.ID] != AssignmentCancelled || statuses[a2.ID] != AssignmentCancelled || statuses[a3.ID] != AssignmentCurrent {
		t.Fatalf("statuses: %v", statuses)
	}
	uses, err := s.PackageUses(ctx, []uuid.UUID{def.ID, small.ID}, now)
	if err != nil || uses[def.ID].Total != 2 || uses[def.ID].Current != 1 || uses[small.ID].Total != 1 || uses[small.ID].Current != 0 {
		t.Fatalf("uses: %+v %v", uses, err)
	}
	// An inactive package cannot be assigned.
	if _, err := s.ChangePackage(ctx, def.ID, func(p *Package) (adminaudit.Event, error) {
		p.Active = false
		return audit(fx, adminaudit.ActionPackageUpdated, p.ID.String(), now), nil
	}); err != nil {
		t.Fatal(err)
	}
	a4 := a1
	a4.ID = uuid.New()
	if _, err := s.Assign(ctx, a4, assignAudit); !errors.Is(err, ErrInactive) {
		t.Fatalf("inactive: %v", err)
	}

	// End: back to the default; nothing left to end.
	endAudit := func([]Assignment) adminaudit.Event {
		return adminaudit.New(fx.admin, "Package Admin", adminaudit.ActionPackageAssignmentEnded, adminaudit.TargetOrganisation, fx.org.String(), "x", "", nil, nil, now)
	}
	if err := s.EndAssignments(ctx, acct, "ZW", now, &fx.admin, now, endAudit); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Resolve(ctx, acct, "ZW", now); r.Basis != BasisDefault {
		t.Fatalf("after end: %+v", r)
	}
	if err := s.EndAssignments(ctx, acct, "ZW", now, &fx.admin, now, endAudit); !errors.Is(err, ErrAssignmentNotFound) {
		t.Fatalf("end again: %v", err)
	}

	// Delete: refused while named, allowed when not.
	err = s.DeletePackage(ctx, def.ID, func(p Package, n int) (adminaudit.Event, error) {
		if n > 0 {
			return adminaudit.Event{}, &InUseError{Assignments: n}
		}
		return audit(fx, adminaudit.ActionPackageDeleted, p.ID.String(), now), nil
	})
	var inUse *InUseError
	if !errors.As(err, &inUse) || inUse.Assignments != 2 {
		t.Fatalf("delete in use: %v", err)
	}
	spare := testPackage(fx, "SPARE", 9, false, now)
	if err := s.CreatePackage(ctx, spare, audit(fx, adminaudit.ActionPackageCreated, spare.ID.String(), now)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePackage(ctx, spare.ID, func(p Package, n int) (adminaudit.Event, error) {
		return audit(fx, adminaudit.ActionPackageDeleted, p.ID.String(), now), nil
	}); err != nil {
		t.Fatal(err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_events WHERE actor_user_id = $1`, fx.admin).Scan(&audits); err != nil || audits < 8 {
		t.Fatalf("audit rows: %d %v", audits, err)
	}

	// Counting: PUBLISHED and unexpired only; markets and names.
	future, past := now.Add(24*time.Hour), now.Add(-time.Hour)
	listing(t, pool, fx.org, "PUBLISHED", &future)
	listing(t, pool, fx.org, "PUBLISHED", &past)
	listing(t, pool, fx.org, "PAUSED", &future)
	listing(t, pool, fx.org, "PENDING_REVIEW", nil)
	live, err := s.LiveListings(ctx, acct, "ZW", now)
	if err != nil || len(live) != 1 || !live[0].Equal(future) {
		t.Fatalf("live: %v %v", live, err)
	}
	if markets, err := s.AccountMarkets(ctx, acct, now); err != nil || len(markets) != 1 || markets[0] != "ZW" {
		t.Fatalf("markets: %v %v", markets, err)
	}
	if name, err := s.AccountName(ctx, acct); err != nil || !strings.HasPrefix(name, "Packages Estates") {
		t.Fatalf("name: %q %v", name, err)
	}
	if _, err := s.AccountName(ctx, Account{Kind: KindDirectOwner, ID: uuid.New()}); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("unknown user: %v", err)
	}

	// Account deletion: the direct owner's assignment is cancelled and the
	// administrator unlinked.
	ownerAcct := Account{Kind: KindDirectOwner, ID: fx.owner}
	ownerSmall := testPackage(fx, "OWNER", 2, false, now)
	if err := s.CreatePackage(ctx, ownerSmall, audit(fx, adminaudit.ActionPackageCreated, ownerSmall.ID.String(), now)); err != nil {
		t.Fatal(err)
	}
	ao := Assignment{ID: uuid.New(), Account: ownerAcct, MarketCode: "ZW", PackageID: ownerSmall.ID, StartsOn: today, Source: SourceAdmin,
		AssignedBy: &fx.owner, AssignedByName: "Package Owner", CreatedAt: now}
	if _, err := s.Assign(ctx, ao, assignAudit); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ForgetUserTx(ctx, tx, fx.owner, "Deleted account", now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Resolve(ctx, ownerAcct, "ZW", now); r.Basis == BasisAssigned {
		t.Fatalf("a deleted owner keeps an assignment: %+v", r)
	}
	if hist, _ := s.Assignments(ctx, ownerAcct); len(hist) != 1 || hist[0].AssignedBy != nil || hist[0].AssignedByName != "Deleted account" {
		t.Fatalf("forgotten: %+v", hist)
	}
}

// TestConcurrentApprovalsNeverExceedTheLimit runs two go-lives at once for
// an account at limit - 1, each in its own transaction as the listing
// approval does (EnforceTx, then the status change): exactly one succeeds.
func TestConcurrentApprovalsNeverExceedTheLimit(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	fx := seedRows(t, pool)
	s := NewPostgresStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM listing_package_assignments WHERE organisation_id = $1`, fx.org)
		_, _ = pool.Exec(ctx, `DELETE FROM listing_packages WHERE code LIKE '%' || $1`, fx.suffix)
		_, _ = pool.Exec(ctx, `DELETE FROM listings WHERE organisation_id = $1`, fx.org)
	})
	two := testPackage(fx, "TWO", 2, false, now)
	if err := s.CreatePackage(ctx, two, audit(fx, adminaudit.ActionPackageCreated, two.ID.String(), now)); err != nil {
		t.Fatal(err)
	}
	acct := Account{Kind: KindOrganisation, ID: fx.org}
	if _, err := s.Assign(ctx, Assignment{ID: uuid.New(), Account: acct, MarketCode: "ZW", PackageID: two.ID, StartsOn: Day(now),
		Source: SourceAdmin, AssignedByName: "Package Admin", CreatedAt: now}, func([]Assignment) adminaudit.Event {
		return adminaudit.New(fx.admin, "Package Admin", adminaudit.ActionPackageAssigned, adminaudit.TargetOrganisation, fx.org.String(), "x", "", nil, nil, now)
	}); err != nil {
		t.Fatal(err)
	}
	expires := now.Add(90 * 24 * time.Hour)
	listing(t, pool, fx.org, "PUBLISHED", &expires) // limit - 1 live
	pending := []uuid.UUID{listing(t, pool, fx.org, "PENDING_REVIEW", nil), listing(t, pool, fx.org, "PENDING_REVIEW", nil)}

	for round := 0; round < 3; round++ {
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]error, len(pending))
		for i, id := range pending {
			wg.Add(1)
			go func(i int, id uuid.UUID) {
				defer wg.Done()
				<-start
				results[i] = func() error {
					tx, err := pool.Begin(ctx)
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback(ctx) }()
					if _, err := tx.Exec(ctx, `SELECT 1 FROM listings WHERE id = $1 FOR UPDATE`, id); err != nil {
						return err
					}
					if err := EnforceTx(ctx, tx, acct, "ZW", id, now); err != nil {
						return err
					}
					// Hold the lock a moment so the other approval really waits.
					time.Sleep(50 * time.Millisecond)
					if _, err := tx.Exec(ctx, `UPDATE listings SET status = 'PUBLISHED', published_at = $2, expires_at = $3 WHERE id = $1`,
						id, now, expires); err != nil {
						return err
					}
					return tx.Commit(ctx)
				}()
			}(i, id)
		}
		close(start)
		wg.Wait()
		ok, refused := 0, 0
		for _, err := range results {
			var q *QuotaError
			switch {
			case err == nil:
				ok++
			case errors.As(err, &q):
				refused++
				if q.Limit != 2 || q.Used != 2 || q.Package.ID != two.ID {
					t.Fatalf("quota error: %+v", q)
				}
			default:
				t.Fatal(err)
			}
		}
		if ok != 1 || refused != 1 {
			t.Fatalf("round %d: %d approved, %d refused (want exactly one each)", round, ok, refused)
		}
		live, err := s.LiveListings(ctx, acct, "ZW", now)
		if err != nil || len(live) != 2 {
			t.Fatalf("round %d: %d live, want 2 (%v)", round, len(live), err)
		}
		// Back to limit - 1 for the next round.
		if _, err := pool.Exec(ctx, `UPDATE listings SET status = 'PENDING_REVIEW', published_at = NULL, expires_at = NULL
			WHERE id = ANY($1)`, pending); err != nil {
			t.Fatal(err)
		}
	}
}
