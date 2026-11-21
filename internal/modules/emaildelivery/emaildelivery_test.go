package emaildelivery

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// testSecret is a random signing secret made for the test run.
func testSecret(t *testing.T) (string, []byte) {
	t.Helper()
	key := make([]byte, 24)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return "whsec_" + base64.StdEncoding.EncodeToString(key), key
}

func TestVerify(t *testing.T) {
	_, key := testSecret(t)
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	body := []byte(`{"type":"email.delivered"}`)
	sig := Sign(key, "msg_1", now, body)
	ts := strconv.FormatInt(now.Unix(), 10)
	if err := Verify(key, "msg_1", ts, "v1,bm9wZQ== "+sig, body, now.Add(time.Minute)); err != nil {
		t.Fatalf("a good signature among several: %v", err)
	}
	for name, c := range map[string]struct {
		id, ts, sig string
		body        []byte
		at          time.Time
	}{
		"other body":    {"msg_1", ts, sig, []byte(`{"type":"email.bounced"}`), now},
		"other id":      {"msg_2", ts, sig, body, now},
		"too old":       {"msg_1", ts, sig, body, now.Add(6 * time.Minute)},
		"from future":   {"msg_1", ts, sig, body, now.Add(-6 * time.Minute)},
		"no signature":  {"msg_1", ts, "", body, now},
		"bad timestamp": {"msg_1", "soon", sig, body, now},
	} {
		if err := Verify(key, c.id, c.ts, c.sig, c.body, c.at); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := DecodeSecret("whsec_short"); err == nil {
		t.Fatal("a short secret decoded")
	}
}

func TestParseResend(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	e, ok, err := ParseResend("msg_1", []byte(`{"type":"email.bounced","created_at":"2026-11-21T08:59:00Z",
		"data":{"email_id":"re_1","to":["Bob@Example.com"],"bounce":{"type":"Permanent","subType":"General"}}}`), now)
	if err != nil || !ok || e.Type != StateBounced || !e.Hard || e.BounceType != "Permanent" || e.ProviderMessageID != "re_1" ||
		e.RecipientSHA256 != mail.RecipientHash("bob@example.com") || !e.OccurredAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("%+v %v %v", e, ok, err)
	}
	e, _, _ = ParseResend("msg_2", []byte(`{"type":"email.bounced","data":{"email_id":"re_2","bounce":{"type":"Transient"}}}`), now)
	if e.Hard {
		t.Fatal("a transient bounce suppresses")
	}
	e, _, _ = ParseResend("msg_3", []byte(`{"type":"email.complained","data":{"email_id":"re_3"}}`), now)
	if !e.Hard || e.Type != StateComplained {
		t.Fatalf("complaint %+v", e)
	}
	if _, ok, err := ParseResend("msg_4", []byte(`{"type":"email.opened","data":{"email_id":"re_4"}}`), now); ok || err != nil {
		t.Fatal("an open is ignored without error")
	}
	for _, body := range []string{`not json`, `{"type":"email.delivered","data":{}}`} {
		if _, _, err := ParseResend("msg_5", []byte(body), now); !errors.Is(err, ErrBadPayload) {
			t.Fatalf("%s: %v", body, err)
		}
	}
	if _, _, err := ParseResend("bad id!", []byte(`{"type":"email.delivered","data":{"email_id":"x"}}`), now); !errors.Is(err, ErrBadPayload) {
		t.Fatal("a bad webhook id")
	}
}

// contract runs the store rules. accept records an accepted message (its
// provider ID and address) in the email log the store reads.
func contract(t *testing.T, store Store, accept func(providerID, address string, at time.Time), auditCount func() int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	run := uuid.NewString()[:8]
	pid := func(n int) string { return fmt.Sprintf("re_%s_%d", run, n) }
	addr := func(n int) string { return fmt.Sprintf("person%d.%s@example.com", n, run) }
	for n := 1; n <= 4; n++ {
		accept(pid(n), addr(n), now.Add(-10*time.Minute))
	}
	ev := func(id, typ string, n int, hard bool) Event {
		return Event{WebhookID: "wh_" + run + "_" + id, Type: typ, ProviderMessageID: pid(n), Hard: hard, BounceType: map[bool]string{true: "Permanent", false: ""}[hard && typ == StateBounced],
			OccurredAt: now.Add(-5 * time.Minute), ReceivedAt: now.Add(-5 * time.Minute)}
	}
	apply := func(e Event, want bool) {
		t.Helper()
		got, err := store.Apply(ctx, e)
		if err != nil || got != want {
			t.Fatalf("apply %s: %v %v", e.WebhookID, got, err)
		}
	}
	apply(ev("1", StateDelivered, 1, false), true)
	apply(ev("1", StateDelivered, 1, false), false) // the same webhook again
	apply(ev("2", StateBounced, 2, true), true)
	apply(ev("3", StateDelivered, 2, false), true) // does not undo the bounce
	apply(ev("4", StateComplained, 3, true), true)
	apply(ev("5", StateBounced, 4, false), true) // transient: no suppression

	for n, want := range map[int]bool{1: false, 2: true, 3: true, 4: false} {
		got, err := store.IsSuppressed(ctx, mail.RecipientHash(addr(n)))
		if err != nil || got != want {
			t.Fatalf("suppressed %d: %v %v", n, got, err)
		}
	}
	list, err := store.Suppressions(ctx, mail.RecipientHash(addr(3)), 10)
	if err != nil || len(list) != 1 || list[0].Reason != ReasonComplaint {
		t.Fatalf("suppressions %+v %v", list, err)
	}
	c, err := store.Counts(ctx, now.Add(-time.Hour))
	if err != nil || c.Delivered < 1 || c.Bounced < 2 || c.Complained < 1 || c.Accepted < 4 {
		t.Fatalf("counts %+v %v", c, err)
	}
	b, cm, a, err := store.Window(ctx, now.Add(-time.Hour), now.Add(time.Second))
	if err != nil || b < 2 || cm < 1 || a < 4 {
		t.Fatalf("window %d %d %d %v", b, cm, a, err)
	}
	probs, err := store.Problems(ctx, 200)
	found := 0
	for _, p := range probs {
		if p.RecipientSHA256 == mail.RecipientHash(addr(2)) && p.DeliveryState == StateBounced {
			found++
		}
	}
	if err != nil || found != 1 {
		t.Fatalf("problems %d %v", found, err)
	}

	before := auditCount()
	hash := mail.RecipientHash(addr(2))
	audit := adminaudit.New(uuid.Nil, "Admin", ActionSuppressionClear, AuditTarget, hash, hash[:12], "Customer asked", nil, nil, now)
	if err := store.Clear(ctx, hash, uuid.Nil, now, audit); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(ctx, hash, uuid.Nil, now, audit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("clear twice: %v", err)
	}
	if got, _ := store.IsSuppressed(ctx, hash); got || auditCount() != before+1 {
		t.Fatalf("cleared: %v audits %d", got, auditCount()-before)
	}

	kind := AlertBounceSpike
	a1 := Alert{ID: uuid.New(), Kind: kind, WindowStart: now.Add(-time.Hour), WindowEnd: now, Events: 9, Accepted: 10, CreatedAt: now}
	if added, err := store.AddAlert(ctx, a1); err != nil || !added {
		t.Fatalf("alert: %v %v", added, err)
	}
	a2 := a1
	a2.ID, a2.CreatedAt = uuid.New(), now.Add(time.Minute)
	if added, _ := store.AddAlert(ctx, a2); added {
		t.Fatal("a second alert in the same window")
	}
	alerts, err := store.Alerts(ctx, now.Add(-time.Minute), 10)
	if err != nil || len(alerts) < 1 {
		t.Fatalf("alerts %v %v", alerts, err)
	}
}

func TestMemoryStoreContract(t *testing.T) {
	log := mail.NewMemoryDeliveryLog()
	audit := &adminaudit.MemoryLog{}
	store := NewMemoryStore(log.Records, audit)
	contract(t, store, func(pid, address string, at time.Time) {
		key := "k-" + pid
		if _, err := log.Begin(context.Background(), mail.Record{Key: key, Kind: "welcome", RecipientSHA256: mail.RecipientHash(address), Provider: "resend"}, at); err != nil {
			t.Fatal(err)
		}
		if err := log.Accepted(context.Background(), key, "resend", pid, at); err != nil {
			t.Fatal(err)
		}
	}, func() int { return len(audit.All()) })
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
	// Alerts of earlier runs must not block this run's.
	if _, err := pool.Exec(ctx, `DELETE FROM email_delivery_alerts`); err != nil {
		t.Fatal(err)
	}
	log := mail.NewPostgresDeliveryLog(pool)
	contract(t, NewPostgresStore(pool), func(pid, address string, at time.Time) {
		key := "k-" + pid
		if _, err := log.Begin(ctx, mail.Record{Key: key, Kind: "welcome", RecipientSHA256: mail.RecipientHash(address), Provider: "resend"}, at); err != nil {
			t.Fatal(err)
		}
		if err := log.Accepted(ctx, key, "resend", pid, at); err != nil {
			t.Fatal(err)
		}
	}, func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_events WHERE action = $1`, ActionSuppressionClear).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
}

func TestServiceRules(t *testing.T) {
	ctx := context.Background()
	log := mail.NewMemoryDeliveryLog()
	store := NewMemoryStore(log.Records, &adminaudit.MemoryLog{})
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	secret, key := testSecret(t)
	svc := NewService(Deps{Store: store, Secret: secret, Now: func() time.Time { return now }})
	if !svc.Configured() || NewService(Deps{Store: store}).Configured() || NewService(Deps{Store: store, Secret: "whsec_!!"}).Configured() {
		t.Fatal("configured")
	}
	if _, err := NewService(Deps{Store: store}).Webhook(ctx, "", "", "", nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	// Six permanent bounces in the hour, out of 10 accepted: a spike, once.
	for n := 0; n < 10; n++ {
		k := fmt.Sprintf("k%d", n)
		_, _ = log.Begin(ctx, mail.Record{Key: k, Kind: "welcome", RecipientSHA256: mail.RecipientHash(fmt.Sprintf("p%d@example.com", n)), Provider: "resend"}, now.Add(-30*time.Minute))
		_ = log.Accepted(ctx, k, "resend", fmt.Sprintf("re_%d", n), now.Add(-30*time.Minute))
	}
	for n := 0; n < 6; n++ {
		body := []byte(fmt.Sprintf(`{"type":"email.bounced","data":{"email_id":"re_%d","bounce":{"type":"Permanent"}}}`, n))
		id := fmt.Sprintf("msg_%d", n)
		if _, err := svc.Webhook(ctx, id, strconv.FormatInt(now.Unix(), 10), Sign(key, id, now, body), body); err != nil {
			t.Fatal(err)
		}
	}
	added, err := svc.Sweep(ctx)
	if err != nil || len(added) != 1 || added[0].Kind != AlertBounceSpike || added[0].Events != 6 || added[0].Accepted != 10 {
		t.Fatalf("sweep %+v %v", added, err)
	}
	if again, _ := svc.Sweep(ctx); len(again) != 0 {
		t.Fatal("a second alert in the window")
	}

	admin := &auth.Principal{UserID: uuid.New(), Name: "Support", SessionScope: auth.ScopeAdmin,
		PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformSupport}}}
	o, err := svc.Overview(ctx, admin)
	if err != nil || !o.Configured || o.Last24h.Bounced != 6 || len(o.Alerts) != 1 || len(o.Problems) != 6 {
		t.Fatalf("overview %+v %v", o, err)
	}
	if _, err := svc.Overview(ctx, &auth.Principal{UserID: uuid.New()}); !errors.Is(err, ErrForbidden) {
		t.Fatal("a customer sees the overview")
	}
	if _, err := svc.Suppressions(ctx, admin, "not an address"); err == nil {
		t.Fatal("a bad filter")
	}
	list, _ := svc.Suppressions(ctx, admin, "P0@Example.com")
	if len(list) != 1 {
		t.Fatalf("by address %v", list)
	}
	var verr *ValidationError
	if err := svc.Clear(ctx, admin, list[0].RecipientSHA256, "no"); !errors.As(err, &verr) {
		t.Fatal("a short reason")
	}
	if err := svc.Clear(ctx, admin, list[0].RecipientSHA256, "The customer fixed their mailbox"); err != nil {
		t.Fatal(err)
	}
	if Rate(1, 0) != nil || *Rate(1, 4) != "0.2500" {
		t.Fatal("rate")
	}
}
