package payments

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/internal/platform/paygateway"
	"rightproperty365.com/backend/migrations"
)

const testSecret = "payments-test-secret-0123456789"

// rig is one subscription waiting for payment, with the services around it.
type rig struct {
	t       *testing.T
	now     time.Time
	mu      sync.Mutex
	svc     *Service
	subs    *subscriptions.Service
	store   Store
	fake    *paygateway.Fake
	sub     subscriptions.Subscription
	payer   *auth.Principal
	cleanup func()
}

func (r *rig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

// newRig builds the rig on subStore and store. subscriber is the user who
// pays (a users row in Postgres).
func newRig(t *testing.T, subStore subscriptions.Store, store Store, subscriber uuid.UUID) *rig {
	t.Helper()
	ctx := context.Background()
	r := &rig{t: t, now: time.Now().UTC().Truncate(time.Second), store: store, payer: &auth.Principal{UserID: subscriber, Name: "Payer"}}
	plan := subscriptions.Plan{ID: uuid.New(), Name: "Payments test plan", SubscriberKind: subscriptions.KindIndividual, EligibleRoles: []string{"AGENT"},
		Features: []subscriptions.Feature{}, Benefits: []string{}, Prices: []subscriptions.Price{{CurrencyCode: "USD", Period: subscriptions.PeriodQuarterly, Amount: "30.00"}},
		Status: subscriptions.PlanActive, CreatedAt: r.now, UpdatedAt: r.now}
	if err := subStore.CreatePlan(ctx, plan, adminaudit.New(uuid.Nil, "Test", adminaudit.ActionPlanCreated, adminaudit.TargetPlan, plan.ID.String(),
		plan.Name, "", nil, nil, r.now)); err != nil {
		t.Fatal(err)
	}
	number, err := subStore.NextInvoiceNumber(ctx, r.now.Year())
	if err != nil {
		t.Fatal(err)
	}
	r.sub = subscriptions.Subscription{ID: uuid.New(), InvoiceNumber: number, PlanID: plan.ID, PlanName: plan.Name, Period: subscriptions.PeriodQuarterly,
		Status: subscriptions.StatusPendingPayment, SubscriberKind: subscriptions.KindIndividual, SubscriberID: subscriber, SubscriberName: "Payer",
		MarketCode: "ZW", CurrencyCode: "USD", Amount: "30.00", CreatedAt: r.now, UpdatedAt: r.now}
	if err := subStore.CreateSubscription(ctx, r.sub, subscriptions.NewEvent(r.sub.ID, subscriptions.ActionRequested, subscriptions.ActorSubscriber,
		uuid.Nil, "Payer", "", r.now), nil); err != nil {
		t.Fatal(err)
	}
	if r.fake, err = paygateway.NewFake("development", []byte(testSecret), r.clock); err != nil {
		t.Fatal(err)
	}
	r.subs = subscriptions.NewService(subscriptions.Deps{Store: subStore, Now: r.clock})
	r.svc = NewService(Deps{Store: store, Subscriptions: r.subs, Gateway: r.fake, PublicWebURL: "http://web.test", Now: r.clock})
	return r
}

func (r *rig) checkout() Payment {
	r.t.Helper()
	res, err := r.svc.Checkout(context.Background(), r.payer, r.sub.ID, "http://api.test")
	if err != nil {
		r.t.Fatal(err)
	}
	return res.Payment
}

func (r *rig) deliver(ev paygateway.Event) (bool, error) {
	body := paygateway.EventBody(ev)
	h := http.Header{}
	h.Set(paygateway.FakeSignatureHeader, r.fake.Sign(body, r.clock()))
	return r.svc.Webhook(context.Background(), paygateway.FakeName, h, body)
}

func (r *rig) subscription() subscriptions.Subscription {
	r.t.Helper()
	x, err := r.subs.Store().Subscription(context.Background(), r.sub.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return x
}

func (r *rig) activations() int {
	r.t.Helper()
	events, err := r.subs.Store().Events(context.Background(), r.sub.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Action == subscriptions.ActionActivated {
			n++
		}
	}
	return n
}

// flowContract is the checkout, webhook, refund and reconciliation rules
// every store must give; the memory and the Postgres stores run it.
func flowContract(t *testing.T, newOne func() *rig) {
	ctx := context.Background()

	t.Run("checkout is idempotent and reading never activates", func(t *testing.T) {
		r := newOne()
		p := r.checkout()
		if p.Status != StatusOpen || p.HostedURL == "" || p.ProviderRef == "" || p.ExpiresAt == nil {
			t.Fatalf("checkout: %+v", p)
		}
		again, err := r.svc.Checkout(ctx, r.payer, r.sub.ID, "http://api.test")
		if err != nil || !again.Reused || again.Payment.ID != p.ID || again.Payment.HostedURL != p.HostedURL {
			t.Fatalf("second checkout: %+v %v", again, err)
		}
		if _, err := r.svc.Checkout(ctx, &auth.Principal{UserID: uuid.New()}, r.sub.ID, "http://api.test"); !errors.Is(err, subscriptions.ErrNotFound) {
			t.Fatalf("somebody else's: %v", err)
		}
		list, err := r.svc.Mine(ctx, r.payer, r.sub.ID)
		if err != nil || len(list) != 1 || list[0].Status != StatusOpen {
			t.Fatalf("mine: %+v %v", list, err)
		}
		if x := r.subscription(); x.Status != subscriptions.StatusPendingPayment {
			t.Fatalf("reading activated: %s", x.Status)
		}
		// Past its expiry a new checkout is made, and the old one is EXPIRED.
		r.advance(paygateway.FakeCheckoutTTL)
		fresh, err := r.svc.Checkout(ctx, r.payer, r.sub.ID, "http://api.test")
		if err != nil || fresh.Reused || fresh.Payment.ID == p.ID {
			t.Fatalf("after expiry: %+v %v", fresh, err)
		}
		if old, _ := r.store.Payment(ctx, p.ID); old.Status != StatusExpired {
			t.Fatalf("old checkout: %s", old.Status)
		}
	})

	t.Run("concurrent duplicate webhooks activate once", func(t *testing.T) {
		r := newOne()
		p := r.checkout()
		ev, err := r.fake.Complete(p.ProviderRef, true)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 20)
		dups := make(chan bool, 20)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				e := ev
				if i%4 == 3 {
					e.ID = ev.ID + "-redelivered-as-new-" + uuid.NewString() // a provider sending a second event for the same payment
				}
				dup, err := r.deliver(e)
				errs <- err
				dups <- dup
			}(i)
		}
		wg.Wait()
		close(errs)
		close(dups)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		notDup := 0
		for d := range dups {
			if !d {
				notDup++
			}
		}
		if notDup < 1 || notDup > 4 {
			t.Fatalf("processed deliveries: %d", notDup)
		}
		x := r.subscription()
		if x.Status != subscriptions.StatusActive || x.Payment == nil || x.Payment.RecordedByKind != subscriptions.RecordedByGateway ||
			x.Payment.Reference != ev.PaymentRef || x.Payment.Provider != paygateway.FakeName {
			t.Fatalf("activated: %+v %+v", x, x.Payment)
		}
		if n := r.activations(); n != 1 {
			t.Fatalf("activations: %d", n)
		}
		got, _ := r.store.Payment(ctx, p.ID)
		if got.Status != StatusSucceeded || got.PaidAmount != "30.00" || got.PaymentRef != ev.PaymentRef {
			t.Fatalf("payment: %+v", got)
		}
		// A later redelivery is a no-op.
		if dup, err := r.deliver(ev); err != nil || !dup {
			t.Fatalf("redelivery: %v %v", dup, err)
		}
	})

	t.Run("bad signature, mismatch and failure", func(t *testing.T) {
		r := newOne()
		p := r.checkout()
		ev := paygateway.Event{ID: "evt-mismatch-" + uuid.NewString(), Type: paygateway.EventPaymentSucceeded, ProviderRef: p.ProviderRef,
			PaymentRef: "fake_pay_x", Amount: "29.99", Currency: "USD", OccurredAt: r.clock()}
		body := paygateway.EventBody(ev)
		h := http.Header{}
		h.Set(paygateway.FakeSignatureHeader, "t=1,v1=00")
		if _, err := r.svc.Webhook(ctx, paygateway.FakeName, h, body); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("bad signature: %v", err)
		}
		if _, err := r.svc.Webhook(ctx, "stripe", h, body); !errors.Is(err, ErrProviderNotFound) {
			t.Fatalf("unknown provider: %v", err)
		}
		if _, err := r.deliver(ev); err != nil {
			t.Fatal(err)
		}
		got, _ := r.store.Payment(ctx, p.ID)
		if got.Status != StatusReview || got.ReviewReason == "" {
			t.Fatalf("mismatch: %+v", got)
		}
		if x := r.subscription(); x.Status != subscriptions.StatusPendingPayment {
			t.Fatalf("a mismatch activated: %s", x.Status)
		}
		// A failed payment of a new checkout leaves the invoice pending.
		p2 := r.checkout()
		fail, _ := r.fake.Complete(p2.ProviderRef, false)
		if _, err := r.deliver(fail); err != nil {
			t.Fatal(err)
		}
		if got, _ := r.store.Payment(ctx, p2.ID); got.Status != StatusFailed || got.FailedAt == nil {
			t.Fatalf("failed: %+v", got)
		}
		if x := r.subscription(); x.Status != subscriptions.StatusPendingPayment {
			t.Fatalf("a failure activated: %s", x.Status)
		}
	})

	t.Run("refunds", func(t *testing.T) {
		r := newOne()
		p := r.checkout()
		ev, _ := r.fake.Complete(p.ProviderRef, true)
		if _, err := r.deliver(ev); err != nil {
			t.Fatal(err)
		}
		admin := &auth.Principal{UserID: r.payer.UserID, Name: "Finance"}
		out, err := r.svc.Refund(ctx, admin, r.sub.ID, RefundInput{Amount: "10", Reason: "Goodwill"}, "k-1")
		if err != nil || out.Refund.Status != RefundSucceeded || out.Payment.Status != StatusPartiallyRefunded || out.Payment.RefundedAmount != "10.00" ||
			out.Subscription.Status != subscriptions.StatusActive {
			t.Fatalf("partial: %+v %v", out, err)
		}
		same, err := r.svc.Refund(ctx, admin, r.sub.ID, RefundInput{Amount: "10", Reason: "Goodwill"}, "k-1")
		if err != nil || same.Refund.ID != out.Refund.ID || same.Payment.RefundedAmount != "10.00" {
			t.Fatalf("idempotent: %+v %v", same, err)
		}
		if _, err := r.svc.Refund(ctx, admin, r.sub.ID, RefundInput{Amount: "20.01", Reason: "Too much"}, ""); !errors.Is(err, ErrRefundExceeds) {
			t.Fatalf("exceeds: %v", err)
		}
		full, err := r.svc.Refund(ctx, admin, r.sub.ID, RefundInput{Amount: "20.00", Reason: "Cancelled by the customer"}, "")
		if err != nil || full.Payment.Status != StatusRefunded || full.Payment.RefundedAmount != "30.00" || full.Subscription.Status != subscriptions.StatusCancelled ||
			full.Subscription.CancelledBy != subscriptions.ActorAdmin || len(full.Refunds) != 2 {
			t.Fatalf("full: %+v %v", full, err)
		}
		// A provider's REFUNDED webhook for a refund already recorded changes nothing.
		dup := paygateway.Event{ID: "evt-refund-" + uuid.NewString(), Type: paygateway.EventRefunded, ProviderRef: p.ProviderRef,
			PaymentRef: ev.PaymentRef, RefundRef: full.Refund.ProviderRefundRef, Amount: "20.00", Currency: "USD", OccurredAt: r.clock()}
		if _, err := r.deliver(dup); err != nil {
			t.Fatal(err)
		}
		if got, _ := r.store.Payment(ctx, p.ID); got.RefundedAmount != "30.00" {
			t.Fatalf("refund webhook counted twice: %+v", got)
		}
		if _, err := r.svc.Refund(ctx, admin, r.sub.ID, RefundInput{Amount: "1", Reason: "Again"}, ""); !errors.Is(err, ErrNoGatewayPayment) {
			t.Fatalf("nothing left: %v", err)
		}
	})

	t.Run("the same Idempotency-Key on two subscriptions makes two refunds", func(t *testing.T) {
		paid := func() *rig {
			r := newOne()
			p := r.checkout()
			ev, _ := r.fake.Complete(p.ProviderRef, true)
			if _, err := r.deliver(ev); err != nil {
				t.Fatal(err)
			}
			return r
		}
		a, b := paid(), paid()
		admin := &auth.Principal{UserID: a.payer.UserID, Name: "Finance"}
		ra, err := a.svc.Refund(ctx, admin, a.sub.ID, RefundInput{Amount: "5", Reason: "Goodwill"}, "shared-key")
		if err != nil {
			t.Fatal(err)
		}
		rb, err := b.svc.Refund(ctx, admin, b.sub.ID, RefundInput{Amount: "5", Reason: "Goodwill"}, "shared-key")
		if err != nil {
			t.Fatal(err)
		}
		if ra.Refund.ID == rb.Refund.ID || rb.Payment.SubscriptionID != b.sub.ID || rb.Payment.RefundedAmount != "5.00" {
			t.Fatalf("key leaked across subscriptions: a=%+v b=%+v", ra.Refund, rb.Refund)
		}
	})

	t.Run("a refund made at the provider cancels on its webhook", func(t *testing.T) {
		r := newOne()
		p := r.checkout()
		ev, _ := r.fake.Complete(p.ProviderRef, true)
		if _, err := r.deliver(ev); err != nil {
			t.Fatal(err)
		}
		refund := paygateway.Event{ID: "evt-ext-refund-" + uuid.NewString(), Type: paygateway.EventRefunded, ProviderRef: p.ProviderRef,
			PaymentRef: ev.PaymentRef, RefundRef: "fake_re_dashboard", Amount: "30.00", Currency: "USD", OccurredAt: r.clock()}
		if _, err := r.deliver(refund); err != nil {
			t.Fatal(err)
		}
		x := r.subscription()
		if x.Status != subscriptions.StatusCancelled || x.CancelledBy != subscriptions.ActorSystem {
			t.Fatalf("cancelled: %s %s", x.Status, x.CancelledBy)
		}
		if got, _ := r.store.Payment(ctx, p.ID); got.Status != StatusRefunded {
			t.Fatalf("payment: %s", got.Status)
		}
	})

	t.Run("reconciliation", func(t *testing.T) {
		r := newOne()
		p := r.checkout()
		// Paid at the provider, the webhook lost.
		if _, err := r.fake.Complete(p.ProviderRef, true); err != nil {
			t.Fatal(err)
		}
		if res, err := r.svc.Reconcile(ctx); err != nil || res.Checked != 0 || r.subscription().Status != subscriptions.StatusPendingPayment {
			t.Fatalf("too early: %+v %v", res, err)
		}
		r.advance(16 * time.Minute)
		res, err := r.svc.Reconcile(ctx)
		if err != nil || res.Activated != 1 || r.subscription().Status != subscriptions.StatusActive || r.activations() != 1 {
			t.Fatalf("reconciled: %+v %v %s", res, err, r.subscription().Status)
		}
		if res, err := r.svc.Reconcile(ctx); err != nil || res.Activated != 0 || r.activations() != 1 {
			t.Fatalf("again: %+v %v", res, err)
		}
	})

	t.Run("open checkouts expire", func(t *testing.T) {
		r := newOne()
		p := r.checkout()
		r.advance(paygateway.FakeCheckoutTTL + time.Second)
		res, err := r.svc.Reconcile(ctx)
		if err != nil || res.Expired != 1 {
			t.Fatalf("expire: %+v %v", res, err)
		}
		if got, _ := r.store.Payment(ctx, p.ID); got.Status != StatusExpired || got.ExpiredAt == nil {
			t.Fatalf("expired: %+v", got)
		}
	})
}

func TestFlowMemory(t *testing.T) {
	flowContract(t, func() *rig {
		audit := &adminaudit.MemoryLog{}
		r := newRig(t, subscriptions.NewMemoryStore(audit), NewMemoryStore(audit), uuid.New())
		return r
	})
}

func TestNotOffered(t *testing.T) {
	subStore := subscriptions.NewMemoryStore(nil)
	svc := NewService(Deps{Store: NewMemoryStore(nil), Subscriptions: subscriptions.NewService(subscriptions.Deps{Store: subStore})})
	if _, err := svc.Checkout(context.Background(), &auth.Principal{UserID: uuid.New()}, uuid.New(), ""); !errors.Is(err, ErrNotOffered) {
		t.Fatalf("checkout: %v", err)
	}
	if _, err := svc.Webhook(context.Background(), "fake", http.Header{}, nil); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("webhook: %v", err)
	}
	if res, err := svc.Reconcile(context.Background()); err != nil || res != (ReconcileResult{}) {
		t.Fatalf("reconcile: %+v %v", res, err)
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

func TestFlowPostgres(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	flowContract(t, func() *rig {
		// Every row a foreign key names: the payer (checkout audit, opened_by).
		user := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Payments Tester', 'ACTIVE', now())`,
			user, "payments."+user.String()[:8]+"@example.test"); err != nil {
			t.Fatal(err)
		}
		return newRig(t, subscriptions.NewPostgresStore(pool), NewPostgresStore(pool), user)
	})

	// The unique constraint: one row per provider event, whatever happens.
	var dupes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT provider, event_id FROM payment_events GROUP BY 1, 2 HAVING count(*) > 1) d`).
		Scan(&dupes); err != nil || dupes != 0 {
		t.Fatalf("duplicate events: %d %v", dupes, err)
	}
	// One OPEN checkout per subscription is enforced by the database too.
	var sub uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT subscription_id FROM payments WHERE status = 'OPEN' LIMIT 1`).Scan(&sub); err == nil {
		if _, err := pool.Exec(ctx, `INSERT INTO payments (id, subscription_id, invoice_number, subscriber_kind, subscriber_id, market_code, provider,
			idempotency_key, amount, currency_code, status, created_at, updated_at)
			SELECT $1, subscription_id, invoice_number, subscriber_kind, subscriber_id, market_code, provider, $2, amount, currency_code, 'OPEN', now(), now()
			FROM payments WHERE subscription_id = $3 AND status = 'OPEN'`, uuid.New(), "dup-"+uuid.NewString(), sub); err == nil {
			t.Fatal("a second OPEN checkout was accepted")
		}
	}

	// Account deletion keeps the payments and unlinks the person.
	user := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Payments Leaver', 'ACTIVE', now())`,
		user, "payments.leaver."+user.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, subscriptions.NewPostgresStore(pool), NewPostgresStore(pool), user)
	p := r.checkout()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ForgetUserTx(ctx, tx, user, "Deleted user", r.clock()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := r.store.Payment(ctx, p.ID)
	if err != nil || got.OpenedBy != nil || got.OpenedByName != "Deleted user" || got.Status != StatusExpired {
		t.Fatalf("forgotten: %+v %v", got, err)
	}
}
