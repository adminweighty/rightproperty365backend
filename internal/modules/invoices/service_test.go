package invoices

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/payments"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/modules/tax"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

func TestApplyTax(t *testing.T) {
	for _, tc := range []struct {
		sub, rate  string
		inclusive  bool
		tax, total string
	}{
		{"30.00", "15", false, "4.50", "34.50"},
		{"34.50", "15", true, "4.50", "34.50"},
		{"10.00", "12.5", false, "1.25", "11.25"},
		{"0.10", "5", false, "0.01", "0.11"}, // 0.005 rounds half up
		{"99.99", "0", false, "0.00", "99.99"},
	} {
		tax, total := ApplyTax(tc.sub, tc.rate, tc.inclusive)
		if tax != tc.tax || total != tc.total {
			t.Errorf("%s at %s%% (inclusive %v): %s / %s, want %s / %s", tc.sub, tc.rate, tc.inclusive, tax, total, tc.tax, tc.total)
		}
	}
}

// fixedTax is a tax rule source with at most one rule.
type fixedTax struct{ rule *tax.Rule }

func (f fixedTax) TaxFor(_ context.Context, market, appliesTo string, _ time.Time) (tax.Rule, bool, error) {
	if f.rule == nil || f.rule.MarketCode != market || appliesTo != tax.AppliesSubscriptions {
		return tax.Rule{}, false, nil
	}
	return *f.rule, true, nil
}

type rig struct {
	t      *testing.T
	now    time.Time
	subs   *subscriptions.Service
	svc    *Service
	store  Store
	admin  Admin
	market string
	// user makes a subscriber (a real users row for Postgres).
	user func() uuid.UUID
}

func (r *rig) clock() time.Time { return r.now }

// activeSubscription makes a plan and an ACTIVE subscription to renew.
func (r *rig) activeSubscription(subscriber uuid.UUID) subscriptions.Subscription {
	r.t.Helper()
	ctx := context.Background()
	st := r.subs.Store()
	plan := subscriptions.Plan{ID: uuid.New(), Name: "Invoice test plan", SubscriberKind: subscriptions.KindIndividual, EligibleRoles: []string{"AGENT"},
		Features: []subscriptions.Feature{}, Benefits: []string{},
		Prices: []subscriptions.Price{{CurrencyCode: "USD", Period: subscriptions.PeriodQuarterly, Amount: "30.00"},
			{CurrencyCode: "USD", Period: subscriptions.PeriodYearly, Amount: "100.00"}},
		Status: subscriptions.PlanActive, CreatedAt: r.now, UpdatedAt: r.now}
	if err := st.CreatePlan(ctx, plan, adminaudit.New(uuid.Nil, "Test", adminaudit.ActionPlanCreated, adminaudit.TargetPlan, plan.ID.String(),
		plan.Name, "", nil, nil, r.now)); err != nil {
		r.t.Fatal(err)
	}
	number, err := st.NextInvoiceNumber(ctx, r.now.Year())
	if err != nil {
		r.t.Fatal(err)
	}
	requester := subscriber
	x := subscriptions.Subscription{ID: uuid.New(), InvoiceNumber: number, PlanID: plan.ID, PlanName: plan.Name, Period: subscriptions.PeriodQuarterly,
		Status: subscriptions.StatusPendingPayment, SubscriberKind: subscriptions.KindIndividual, SubscriberID: subscriber, SubscriberName: "Payer",
		RequestedBy: &requester, RequestedByName: "Payer", MarketCode: r.market, CurrencyCode: "USD", Amount: "30.00", CreatedAt: r.now, UpdatedAt: r.now}
	if err := st.CreateSubscription(ctx, x, subscriptions.NewEvent(x.ID, subscriptions.ActionRequested, subscriptions.ActorSubscriber, uuid.Nil, "Payer", "", r.now), nil); err != nil {
		r.t.Fatal(err)
	}
	p := &auth.Principal{UserID: r.admin.UserID, Name: r.admin.Name}
	if _, err := r.subs.Approve(ctx, p, x.ID, subscriptions.ApproveInput{PaymentReference: "BANK-1", Amount: "30.00",
		ReceivedOn: r.now.Format(DayLayout)}); err != nil {
		r.t.Fatal(err)
	}
	got, err := st.Subscription(ctx, x.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

var numberRe = regexp.MustCompile(`^INV-([A-Z]{2})-(\d{4})-(\d{6,})$`)

func seq(t *testing.T, number string) int {
	t.Helper()
	m := numberRe.FindStringSubmatch(number)
	if m == nil {
		t.Fatalf("not an invoice number: %q", number)
	}
	n, _ := strconv.Atoi(m[3])
	return n
}

func (r *rig) subStatus(id uuid.UUID) subscriptions.Subscription {
	r.t.Helper()
	x, err := r.subs.Store().Subscription(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return x
}

func (r *rig) payments(id uuid.UUID) []Payment {
	r.t.Helper()
	list, err := r.store.Payments(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return list
}

func invoiceContract(t *testing.T, newRig func(t *testing.T) *rig) {
	ctx := context.Background()

	t.Run("draft, issue, part and full payment", func(t *testing.T) {
		r := newRig(t)
		cur := r.activeSubscription(r.user())
		inv, err := r.svc.Create(ctx, r.admin, DraftInput{RenewsSubscriptionID: cur.ID.String()})
		if err != nil || inv.Status != StatusDraft || inv.Number != "" || len(inv.Lines) != 1 || inv.Total != "30.00" || inv.Tax != nil ||
			!inv.ServiceFrom.Equal(day(*cur.EndsAt)) {
			t.Fatalf("draft: %+v %v", inv, err)
		}
		due := r.now.AddDate(0, 0, 7).Format(DayLayout)
		if inv, err = r.svc.Update(ctx, r.admin, inv.ID, DraftInput{DueOn: &due, Period: "YEARLY"}); err != nil || inv.Total != "100.00" || inv.DueOn.Format(DayLayout) != due {
			t.Fatalf("edit: %+v %v", inv, err)
		}
		// A second draft for the same subscription: it cannot be issued while the first renewal waits.
		other, err := r.svc.Create(ctx, r.admin, DraftInput{RenewsSubscriptionID: cur.ID.String()})
		if err != nil {
			t.Fatal(err)
		}
		inv, err = r.svc.Issue(ctx, r.admin, inv.ID)
		if err != nil || inv.Status != StatusIssued || inv.SubscriptionID == nil || inv.SendCount != 1 || inv.IssueOn == nil {
			t.Fatalf("issue: %+v %v", inv, err)
		}
		first := seq(t, inv.Number)
		renewal := r.subStatus(*inv.SubscriptionID)
		if renewal.Status != subscriptions.StatusPendingPayment || renewal.Amount != "100.00" || renewal.RenewalOf == nil || *renewal.RenewalOf != cur.ID {
			t.Fatalf("renewal: %+v", renewal)
		}
		if _, err := r.svc.Issue(ctx, r.admin, other.ID); !errors.Is(err, ErrRenewalExists) {
			t.Fatalf("second issue: %v", err)
		}
		if again, _ := r.store.Get(ctx, other.ID); again.Status != StatusDraft || again.Number != "" {
			t.Fatalf("the refused draft: %+v", again)
		}
		if err := r.svc.Discard(ctx, r.admin, other.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.store.Get(ctx, other.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("discarded: %v", err)
		}
		if _, err := r.svc.Update(ctx, r.admin, inv.ID, DraftInput{Terms: &due}); !errors.Is(err, ErrState) {
			t.Fatalf("edit issued: %v", err)
		}

		// Resend at most every 10 minutes.
		if _, err := r.svc.Resend(ctx, r.admin, inv.ID); !errors.Is(err, ErrResendTooSoon) {
			t.Fatalf("resend: %v", err)
		}
		r.now = r.now.Add(ResendInterval)
		if inv, err = r.svc.Resend(ctx, r.admin, inv.ID); err != nil || inv.SendCount != 2 {
			t.Fatalf("resend later: %+v %v", inv, err)
		}

		// A part payment; the rest cannot be paid online.
		pay := func(amount string) (Invoice, Payment, error) {
			return r.svc.RecordPayment(ctx, r.admin, inv.ID, PaymentInput{Amount: amount, Method: "bank_transfer", Reference: "TRF-9",
				ReceivedOn: r.now.Format(DayLayout)})
		}
		var verr *ValidationError
		if _, _, err := pay("-1"); !errors.As(err, &verr) {
			t.Fatalf("bad amount: %v", err)
		}
		if _, _, err := pay("100.01"); !errors.Is(err, ErrOverpayment) {
			t.Fatalf("overpayment: %v", err)
		}
		inv, p, err := pay("40")
		if err != nil || inv.Status != StatusPartPaid || inv.Balance() != "60.00" || !strings.HasPrefix(p.ReceiptNumber, "RCT-"+r.market+"-") {
			t.Fatalf("part payment: %+v %+v %v", inv, p, err)
		}
		if err := r.svc.CheckoutGuard(ctx, *inv.SubscriptionID); !errors.Is(err, payments.ErrPaidOffline) {
			t.Fatalf("guard: %v", err)
		}
		if _, err := r.svc.Void(ctx, r.admin, inv.ID, ReasonInput{Reason: "Wrong plan"}); !errors.Is(err, ErrHasPayments) {
			t.Fatalf("void with payments: %v", err)
		}
		if r.subStatus(*inv.SubscriptionID).Status != subscriptions.StatusPendingPayment {
			t.Fatal("a part payment activated the renewal")
		}
		// The rest: PAID, and the renewal is active from the end of the current period.
		if inv, _, err = pay("60.00"); err != nil || inv.Status != StatusPaid || inv.Balance() != "0.00" || len(r.payments(inv.ID)) != 2 {
			t.Fatalf("paid: %+v %v", inv, err)
		}
		renewal = r.subStatus(*inv.SubscriptionID)
		if renewal.Status != subscriptions.StatusActive || renewal.StartsAt == nil || !renewal.StartsAt.Equal(*cur.EndsAt) {
			t.Fatalf("activated: %+v", renewal)
		}
		// The activation event of that renewal settles nothing more.
		if err := r.svc.Settle(ctx, *inv.SubscriptionID); err != nil || len(r.payments(inv.ID)) != 2 {
			t.Fatalf("settle after paid: %v", err)
		}
		// Disputes come and go.
		if inv, err = r.svc.Dispute(ctx, r.admin, inv.ID, ReasonInput{Reason: "Chargeback opened"}); err != nil || inv.Status != StatusDisputed {
			t.Fatalf("dispute: %+v %v", inv, err)
		}
		if inv, err = r.svc.ResolveDispute(ctx, r.admin, inv.ID, ReasonInput{Reason: "Chargeback won"}); err != nil || inv.Status != StatusPaid {
			t.Fatalf("resolve: %+v %v", inv, err)
		}
		events, _ := r.store.Events(ctx, inv.ID)
		if len(events) < 8 {
			t.Fatalf("%d history entries", len(events))
		}

		// Void, reissue and the gateway: the numbers keep counting.
		cur2 := r.activeSubscription(r.user())
		inv2, err := r.svc.Create(ctx, r.admin, DraftInput{RenewsSubscriptionID: cur2.ID.String()})
		if err != nil {
			t.Fatal(err)
		}
		if inv2, err = r.svc.Issue(ctx, r.admin, inv2.ID); err != nil || seq(t, inv2.Number) <= first {
			t.Fatalf("issue 2: %+v %v", inv2, err)
		}
		if inv2, err = r.svc.Void(ctx, r.admin, inv2.ID, ReasonInput{Reason: "Wrong period"}); err != nil || inv2.Status != StatusVoid {
			t.Fatalf("void: %+v %v", inv2, err)
		}
		if r.subStatus(*inv2.SubscriptionID).Status != subscriptions.StatusCancelled {
			t.Fatal("the voided invoice's renewal is not cancelled")
		}
		re, err := r.svc.Reissue(ctx, r.admin, inv2.ID)
		if err != nil || re.Status != StatusDraft || re.ReplacesInvoiceID == nil || *re.ReplacesInvoiceID != inv2.ID || re.Total != inv2.Total {
			t.Fatalf("reissue: %+v %v", re, err)
		}
		if re, err = r.svc.Issue(ctx, r.admin, re.ID); err != nil || seq(t, re.Number) != seq(t, inv2.Number)+1 {
			t.Fatalf("reissued: %+v %v", re, err)
		}
		if _, err := r.subs.ActivatePaid(ctx, *re.SubscriptionID, subscriptions.GatewayPayment{Provider: "fake", Reference: "pay_1",
			Amount: re.Total, Currency: "USD", PaidAt: r.now}); err != nil {
			t.Fatal(err)
		}
		for range 2 { // the activation event, delivered twice
			if err := r.svc.Settle(ctx, *re.SubscriptionID); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := r.store.Get(ctx, re.ID)
		ps := r.payments(re.ID)
		if got.Status != StatusPaid || len(ps) != 1 || ps[0].Method != MethodGateway || ps[0].Amount != re.Total || ps[0].Reference != "pay_1" {
			t.Fatalf("gateway settlement: %+v %+v", got, ps)
		}
	})

	t.Run("overdue once, credit in full voids", func(t *testing.T) {
		r := newRig(t)
		cur := r.activeSubscription(r.user())
		inv, err := r.svc.Create(ctx, r.admin, DraftInput{RenewsSubscriptionID: cur.ID.String()})
		if err != nil {
			t.Fatal(err)
		}
		if inv, err = r.svc.Issue(ctx, r.admin, inv.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.svc.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if got, _ := r.store.Get(ctx, inv.ID); got.Status != StatusIssued {
			t.Fatalf("not yet due: %s", got.Status)
		}
		r.now = r.now.AddDate(0, 0, DefaultDueDays+1)
		for range 2 {
			if _, err := r.svc.Sweep(ctx); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := r.store.Get(ctx, inv.ID)
		events, _ := r.store.Events(ctx, inv.ID)
		overdue := 0
		for _, e := range events {
			if e.Action == ActionOverdue {
				overdue++
			}
		}
		if got.Status != StatusOverdue || overdue != 1 {
			t.Fatalf("overdue: %s, %d entries", got.Status, overdue)
		}
		if _, err := r.svc.CreditNote(ctx, r.admin, inv.ID, CreditInput{Amount: "30.01", Reason: "Discount"}); !errors.Is(err, ErrOverpayment) {
			t.Fatalf("credit too much: %v", err)
		}
		if got, err = r.svc.CreditNote(ctx, r.admin, inv.ID, CreditInput{Amount: "10", Reason: "Loyalty discount"}); err != nil || got.Balance() != "20.00" {
			t.Fatalf("credit: %+v %v", got, err)
		}
		if got, err = r.svc.CreditNote(ctx, r.admin, inv.ID, CreditInput{Amount: "20", Reason: "Waived"}); err != nil || got.Status != StatusVoid {
			t.Fatalf("credit in full: %+v %v", got, err)
		}
		credits, _ := r.store.Credits(ctx, inv.ID)
		if len(credits) != 2 || !strings.HasPrefix(credits[0].Number, "CRN-") || credits[0].Number == credits[1].Number {
			t.Fatalf("credit notes: %+v", credits)
		}
		if r.subStatus(*got.SubscriptionID).Status != subscriptions.StatusCancelled {
			t.Fatal("the renewal of a fully credited invoice is not cancelled")
		}
	})

	t.Run("tax line from the market's rule", func(t *testing.T) {
		r := newRig(t)
		r.svc.d.Tax = fixedTax{rule: &tax.Rule{MarketCode: r.market, TaxName: "VAT", Rate: "15", AppliesTo: tax.AppliesSubscriptions, IsSample: true}}
		cur := r.activeSubscription(r.user())
		inv, err := r.svc.Create(ctx, r.admin, DraftInput{RenewsSubscriptionID: cur.ID.String()})
		if err != nil || inv.Tax == nil || inv.Tax.Amount != "4.50" || inv.Total != "34.50" || !inv.Tax.IsSample {
			t.Fatalf("taxed draft: %+v %v", inv, err)
		}
		if inv, err = r.svc.Issue(ctx, r.admin, inv.ID); err != nil || r.subStatus(*inv.SubscriptionID).Amount != "34.50" {
			t.Fatalf("taxed issue: %+v %v", inv, err)
		}
	})
}

func newMemoryRig() func(t *testing.T) *rig {
	return func(t *testing.T) *rig {
		r := &rig{t: t, now: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), admin: Admin{UserID: uuid.New(), Name: "Finance"}, market: "ZW", user: uuid.New}
		r.subs = subscriptions.NewService(subscriptions.Deps{Store: subscriptions.NewMemoryStore(&adminaudit.MemoryLog{}), Now: r.clock})
		r.store = NewMemoryStore()
		r.svc = NewService(Deps{Store: r.store, Subscriptions: r.subs, Now: r.clock})
		return r
	}
}

func TestInvoicesMemory(t *testing.T) { invoiceContract(t, newMemoryRig()) }

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database. Skipped when
// unset; never read from .env.
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

func TestInvoicesPostgres(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	invoiceContract(t, func(t *testing.T) *rig {
		// Real users for the foreign keys; numbers continue across runs, so
		// the test only compares them.
		r := &rig{t: t, now: time.Now().UTC().Truncate(time.Second), market: "ZW"}
		r.user = func() uuid.UUID {
			id := uuid.New()
			if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Invoice Tester', 'ACTIVE', now())`,
				id, "invoices."+id.String()[:8]+"@example.test"); err != nil {
				t.Fatal(err)
			}
			return id
		}
		r.admin = Admin{UserID: r.user(), Name: "Finance"}
		r.subs = subscriptions.NewService(subscriptions.Deps{Store: subscriptions.NewPostgresStore(pool), Now: r.clock})
		r.store = NewPostgresStore(pool)
		r.svc = NewService(Deps{Store: r.store, Subscriptions: r.subs, Now: r.clock})
		return r
	})
}

// Two invoices issued at the same moment get consecutive numbers (the
// counter row is locked in each issuing transaction): no gap, no duplicate.
func TestInvoiceNumbersConcurrentPostgres(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	r := &rig{t: t, now: time.Now().UTC().Truncate(time.Second), market: "ZW"}
	r.user = func() uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Invoice Tester', 'ACTIVE', now())`,
			id, "invoices."+id.String()[:8]+"@example.test"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	r.admin = Admin{UserID: r.user(), Name: "Finance"}
	r.subs = subscriptions.NewService(subscriptions.Deps{Store: subscriptions.NewPostgresStore(pool), Now: r.clock})
	r.store = NewPostgresStore(pool)
	r.svc = NewService(Deps{Store: r.store, Subscriptions: r.subs, Now: r.clock})
	var drafts []uuid.UUID
	for range 2 {
		cur := r.activeSubscription(r.user())
		inv, err := r.svc.Create(ctx, r.admin, DraftInput{RenewsSubscriptionID: cur.ID.String()})
		if err != nil {
			t.Fatal(err)
		}
		drafts = append(drafts, inv.ID)
	}
	numbers := make(chan string, 2)
	errs := make(chan error, 2)
	for _, id := range drafts {
		go func() {
			inv, err := r.svc.Issue(ctx, r.admin, id)
			errs <- err
			numbers <- inv.Number
		}()
	}
	var got []int
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		got = append(got, seq(t, <-numbers))
	}
	if d := got[0] - got[1]; d != 1 && d != -1 {
		t.Fatalf("numbers %v are not consecutive", got)
	}
}
