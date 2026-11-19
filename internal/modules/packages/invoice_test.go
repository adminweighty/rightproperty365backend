package packages

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// invoiceOps are the INVOICE grant and end of one store.
type invoiceOps struct {
	grant       func(g InvoiceGrant) bool
	end         func(subscriptionID uuid.UUID, at time.Time) bool
	assignments func(acct Account) []Assignment
}

// invoiceContract: a paid subscription's grant replaces what overlaps,
// is idempotent, needs an active package of the code in the market, and
// ending it cancels (started today) or ends it yesterday.
func invoiceContract(t *testing.T, ops invoiceOps, acct Account, code string, sub uuid.UUID, now time.Time) {
	t.Helper()
	from, to := InvoiceDates(now, now.AddDate(0, 3, 0))
	if !to.Equal(Day(now.AddDate(0, 3, 0))) {
		t.Fatalf("dates: %s %s", from, to)
	}
	g := InvoiceGrant{Account: acct, MarketCode: "ZW", PackageCode: code, StartsOn: from, EndsOn: to, SubscriptionID: sub, Label: "Subscription RPS-T", At: now}
	missing := g
	missing.PackageCode, missing.SubscriptionID = "NO_SUCH_PACKAGE", uuid.New()
	if ops.grant(missing) {
		t.Fatal("a code the market does not have granted something")
	}
	if !ops.grant(g) {
		t.Fatal("not granted")
	}
	if ops.grant(g) {
		t.Fatal("granted twice")
	}
	var mine *Assignment
	for _, a := range ops.assignments(acct) {
		if a.SubscriptionID != nil && *a.SubscriptionID == sub {
			a := a
			mine = &a
		}
	}
	if mine == nil || mine.Source != SourceInvoice || !mine.StartsOn.Equal(from) || mine.EndsOn == nil || !mine.EndsOn.Equal(to) || !mine.InForce(Day(now)) {
		t.Fatalf("assignment: %+v", mine)
	}
	if !ops.end(sub, now) {
		t.Fatal("not ended")
	}
	if ops.end(sub, now) {
		t.Fatal("ended twice")
	}
	for _, a := range ops.assignments(acct) {
		if a.ID == mine.ID && (a.CancelledAt == nil || a.InForce(Day(now))) {
			t.Fatalf("still in force: %+v", a)
		}
	}
}

func TestInvoiceGrantMemory(t *testing.T) {
	audit := &adminaudit.MemoryLog{}
	s := NewMemoryStore(audit)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	s.PutPackage(Package{ID: uuid.New(), MarketCode: "ZW", Code: "BUSINESS", Name: "Business", ListingLimit: 40, Active: true, CreatedAt: now, UpdatedAt: now})
	acct := Account{Kind: KindDirectOwner, ID: uuid.New()}
	invoiceContract(t, invoiceOps{
		grant: s.GrantInvoice,
		end:   func(id uuid.UUID, at time.Time) bool { return s.EndInvoice(id, "Subscription RPS-T", at) },
		assignments: func(a Account) []Assignment {
			list, _ := s.Assignments(context.Background(), a)
			return list
		},
	}, acct, "BUSINESS", uuid.New(), now)
	if len(audit.All()) != 2 {
		t.Fatalf("audit: %+v", audit.All())
	}
}

func TestInvoiceGrantPostgres(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	fx := seedRows(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	store := NewPostgresStore(pool)
	pkg := testPackage(fx, "INV", 40, false, now)
	if err := store.CreatePackage(ctx, pkg, audit(fx, adminaudit.ActionPackageCreated, pkg.ID.String(), now)); err != nil {
		t.Fatal(err)
	}
	sub := insertSubscription(t, pool, fx.org, now)
	inTx := func(fn func(context.Context, *pgxpool.Pool) bool) bool { return fn(ctx, pool) }
	invoiceContract(t, invoiceOps{
		grant: func(g InvoiceGrant) bool {
			return inTx(func(ctx context.Context, pool *pgxpool.Pool) bool {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				ok, err := GrantInvoiceTx(ctx, tx, g)
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				return ok
			})
		},
		end: func(id uuid.UUID, at time.Time) bool {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			ok, err := EndInvoiceTx(ctx, tx, id, "Subscription RPS-T", at)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			return ok
		},
		assignments: func(a Account) []Assignment {
			list, err := store.Assignments(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			return list
		},
	}, Account{Kind: KindOrganisation, ID: fx.org}, pkg.Code, sub, now)
}

// insertSubscription inserts a plan and a pending subscription of the
// organisation (every row the assignment's subscription_id points at).
func insertSubscription(t *testing.T, pool *pgxpool.Pool, org uuid.UUID, now time.Time) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	plan, sub := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO subscription_plans (id, name, subscriber_kind, eligible_roles, created_at, updated_at)
		VALUES ($1, 'Invoice test plan', 'ORGANISATION', ARRAY['AGENCY_OWNER'], $2, $2)`, plan, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO subscriptions (id, invoice_number, plan_id, plan_name, period, status, subscriber_kind, subscriber_id,
			subscriber_name, organisation_id, market_code, currency_code, amount, created_at, updated_at)
		VALUES ($1, 'RPS-2026-' || lpad(nextval('subscription_invoice_seq')::text, 6, '0'), $2, 'Invoice test plan', 'QUARTERLY', 'PENDING_PAYMENT',
			'ORGANISATION', $3, 'Packages Estates', $3, 'ZW', 'USD', 30, $4, $4)`, sub, plan, org, now); err != nil {
		t.Fatal(err)
	}
	return sub
}
