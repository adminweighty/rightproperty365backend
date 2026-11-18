package inapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/push"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// fakePusher records messages and answers with fail (nil: accepted).
type fakePusher struct {
	mu   sync.Mutex
	sent []push.Message
	fail func(n int) error
}

func (p *fakePusher) Name() string { return "fake" }

func (p *fakePusher) Send(_ context.Context, m push.Message) (push.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, m)
	if p.fail != nil {
		if err := p.fail(len(p.sent)); err != nil {
			return push.Result{}, err
		}
	}
	return push.Result{MessageID: "msg-" + uuid.NewString()}, nil
}

type rig struct {
	t      *testing.T
	store  *MemoryStore
	svc    *Service
	sender *Sender
	pusher *fakePusher
	clock  *clock
	user   uuid.UUID
	device Device
}

func newRig(t *testing.T) *rig {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := &rig{t: t, store: NewMemoryStore(), pusher: &fakePusher{}, clock: c, user: uuid.New()}
	r.svc = NewService(r.store, "fake", logger, c.now)
	r.sender = NewSender(r.store, r.pusher, logger, c.now)
	r.device = r.register(r.user, "tok-"+uuid.NewString(), nil)
	return r
}

func (r *rig) register(user uuid.UUID, token string, session *uuid.UUID) Device {
	r.t.Helper()
	yes := true
	d, _, err := r.svc.RegisterDevice(context.Background(), user, session, DeviceInput{Token: token, Platform: PlatformIOS, AppVersion: "1.0", PushConsent: &yes})
	if err != nil {
		r.t.Fatal(err)
	}
	return d
}

func (r *rig) notify(typ string) Notification {
	r.t.Helper()
	n, _, err := r.svc.Notify(context.Background(), Draft{SourceEventID: uuid.New(), UserID: r.user, Type: typ, Title: "New property enquiry",
		Body: "PR-ZW-10001 · Borrowdale", Link: "/agent/requests/1", Reference: "PR-ZW-10001"})
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *rig) push(n Notification) DeliveryRecord {
	r.t.Helper()
	for _, d := range r.store.Deliveries(n.ID) {
		if d.Channel == ChannelPush {
			return d
		}
	}
	r.t.Fatalf("no push delivery for %s", n.ID)
	return DeliveryRecord{}
}

func (r *rig) run() {
	r.t.Helper()
	if _, err := r.sender.ProcessOnce(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

// An accepted push: safe text, the data, the badge; recorded as
// PROVIDER_ACCEPTED (never "delivered"), read_at untouched.
func TestPushAccepted(t *testing.T) {
	r := newRig(t)
	n := r.notify(TypeNewLead)
	r.run()
	if len(r.pusher.sent) != 1 {
		t.Fatalf("%d pushes", len(r.pusher.sent))
	}
	m := r.pusher.sent[0]
	if m.Token != r.device.Token || m.Title != n.Title || m.Body != n.Body || m.Data["type"] != TypeNewLead || m.Data["link"] != n.Link ||
		m.Data["notification_id"] != n.ID.String() || m.Badge == nil || *m.Badge != 1 {
		t.Fatalf("message = %+v", m)
	}
	d := r.push(n)
	if d.Status != StatusProviderAccepted || d.MessageID == "" || d.Attempts != 1 {
		t.Fatalf("delivery = %+v", d)
	}
	r.run()
	if len(r.pusher.sent) != 1 {
		t.Fatal("an accepted push was sent again")
	}
}

// FR-065: during an outage the inbox still has the item; retries back off
// exponentially and succeed once the provider recovers; a provider that
// never recovers dead-letters after MaxPushAttempts.
func TestPushRetriesThenDeadLetters(t *testing.T) {
	r := newRig(t)
	down := true
	r.pusher.fail = func(int) error {
		if down {
			return &push.Error{Kind: push.Transient, Status: 503, Msg: "FCM is unavailable"}
		}
		return nil
	}
	n := r.notify(TypeNewLead)
	r.run()
	if d := r.push(n); d.Status != StatusFailed || d.NextAttemptAt == nil || d.NextAttemptAt.Sub(r.clock.t) != 30*time.Second {
		t.Fatalf("first failure: %+v", d)
	}
	if c, _ := r.svc.UnreadCount(context.Background(), r.user); c != 1 {
		t.Fatal("the inbox must still show the item during an outage")
	}
	r.run() // not due yet
	if len(r.pusher.sent) != 1 {
		t.Fatal("retried before the backoff")
	}
	r.clock.advance(31 * time.Second)
	r.run()
	if d := r.push(n); d.Attempts != 2 || d.NextAttemptAt.Sub(r.clock.t) != time.Minute {
		t.Fatalf("second failure: %+v", d)
	}
	down = false
	r.clock.advance(61 * time.Second)
	r.run()
	if d := r.push(n); d.Status != StatusProviderAccepted || d.Attempts != 3 {
		t.Fatalf("after recovery: %+v", d)
	}

	// Never recovering: DEAD after MaxPushAttempts tries.
	down = true
	n2 := r.notify(TypeNewLead)
	for range MaxPushAttempts + 2 {
		r.run()
		r.clock.advance(2 * time.Hour)
	}
	if d := r.push(n2); d.Status != StatusDead || d.Attempts != MaxPushAttempts {
		t.Fatalf("never recovering: %+v", d)
	}
}

// A permanent failure is dead-lettered at once; an UNREGISTERED token
// removes the device.
func TestPushPermanentAndInvalidToken(t *testing.T) {
	r := newRig(t)
	r.pusher.fail = func(int) error { return &push.Error{Kind: push.Permanent, Status: 400, Msg: "rejected"} }
	n := r.notify(TypeNewLead)
	r.run()
	if d := r.push(n); d.Status != StatusDead || d.Attempts != 1 {
		t.Fatalf("permanent: %+v", d)
	}
	r.pusher.fail = func(int) error {
		return &push.Error{Kind: push.InvalidToken, Status: 404, Code: "UNREGISTERED", Msg: "the device token is no longer registered"}
	}
	n2 := r.notify(TypeNewLead)
	r.run()
	if d := r.push(n2); d.Status != StatusDead {
		t.Fatalf("unregistered: %+v", d)
	}
	if devs, _ := r.svc.Devices(context.Background(), r.user); len(devs) != 0 {
		t.Fatalf("an UNREGISTERED token must be removed: %v", devs)
	}
	// No device, no new push.
	n3 := r.notify(TypeNewLead)
	for _, d := range r.store.Deliveries(n3.ID) {
		if d.Channel == ChannelPush {
			t.Fatal("a push was queued for a removed device")
		}
	}
}

// Burst control: more than BurstLimit notifications for one device in the
// window become one summary push; after BurstLimit pushes the device waits.
func TestPushBurstCollapsing(t *testing.T) {
	r := newRig(t)
	for range 8 {
		r.notify(TypeNewLead)
	}
	r.run()
	if len(r.pusher.sent) != 1 || r.pusher.sent[0].Data["type"] != TypeSummary || !strings.Contains(r.pusher.sent[0].Body, "8 new notifications") {
		t.Fatalf("pushes = %+v", r.pusher.sent)
	}
	// Four single pushes take the device to BurstLimit in the window...
	for range 4 {
		r.notify(TypeNewLead)
		r.run()
	}
	if len(r.pusher.sent) != 5 {
		t.Fatalf("%d pushes, want 5", len(r.pusher.sent))
	}
	// ...so the next ones wait until the window has passed.
	r.notify(TypeNewLead)
	r.notify(TypeNewLead)
	r.run()
	if len(r.pusher.sent) != 5 {
		t.Fatalf("burst limit not applied: %d", len(r.pusher.sent))
	}
	r.clock.advance(BurstWindow + time.Second)
	r.run()
	if len(r.pusher.sent) != 7 || r.pusher.sent[5].Data["type"] != TypeNewLead {
		t.Fatalf("after the window: %+v", r.pusher.sent[5:])
	}
}

// Push follows the preference (non-essential only); disabled push queues nothing.
func TestPushPreferencesAndDisabled(t *testing.T) {
	r := newRig(t)
	off := false
	if _, err := r.svc.SetPreferences(context.Background(), r.user, []PreferenceItem{{Type: TypeNewLead, Push: &off}}, SourceApp); err != nil {
		t.Fatal(err)
	}
	n := r.notify(TypeNewLead)
	if len(r.store.Deliveries(n.ID)) != 0 {
		t.Fatal("push queued although switched off")
	}
	acc := r.notify(TypeAccount)
	if len(r.store.Deliveries(acc.ID)) != 1 {
		t.Fatal("an essential type is always pushed")
	}
	disabled := NewService(r.store, "", slog.New(slog.NewJSONHandler(io.Discard, nil)), r.clock.now)
	n2, _, err := disabled.Notify(context.Background(), Draft{SourceEventID: uuid.New(), UserID: r.user, Type: TypeAccount, Title: "t", Link: "/account"})
	if err != nil || len(r.store.Deliveries(n2.ID)) != 0 {
		t.Fatalf("disabled push queued deliveries: %v", err)
	}
	if disabled.PushProvider() != push.ProviderDisabled {
		t.Fatalf("provider = %q", disabled.PushProvider())
	}
}

// FR-003: essential types cannot be switched off, in-app never; every
// change records when and from where; validation is all-or-nothing.
func TestPreferenceRules(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	off, on := false, true
	cases := []struct {
		items []PreferenceItem
		field string
	}{
		{[]PreferenceItem{{Type: TypeAccount, Email: &off}}, "items[0].email"},
		{[]PreferenceItem{{Type: TypeSubscriptionInvoice, Push: &off}}, "items[0].push"},
		{[]PreferenceItem{{Type: TypeSecurity, Email: &off}}, "items[0].email"},
		{[]PreferenceItem{{Type: TypeNewLead, InApp: &off}}, "items[0].in_app"},
		{[]PreferenceItem{{Type: "NOPE", Email: &off}}, "items[0].type"},
		{[]PreferenceItem{{Type: TypeNewLead, Email: &off}, {Type: TypeNewLead, Push: &off}}, "items[1].type"},
		{[]PreferenceItem{{Type: TypeNewEnquiry, Email: &off}, {Type: TypeAccount, Push: &off}}, "items[1].push"},
	}
	for _, c := range cases {
		_, err := r.svc.SetPreferences(ctx, r.user, c.items, SourceWeb)
		var v *ValidationError
		if !errors.As(err, &v) || v.Fields[c.field] == "" {
			t.Fatalf("%+v: %v", c.items, err)
		}
	}
	if len(r.store.History()) != 0 {
		t.Fatal("a refused PUT changed something")
	}
	// Essential true is accepted (it changes nothing).
	views, err := r.svc.SetPreferences(ctx, r.user, []PreferenceItem{{Type: TypeAccount, Email: &on}, {Type: TypeNewEnquiry, Email: &off}}, SourceApp)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		switch v.Type {
		case TypeNewEnquiry:
			if v.Email || !v.Push || v.Source != SourceApp || v.ChangedAt == nil {
				t.Fatalf("view = %+v", v)
			}
		case TypeAccount:
			if !v.Essential || !v.Email || !v.Push || v.EssentialReason == "" {
				t.Fatalf("essential view = %+v", v)
			}
		}
	}
	h := r.store.History()
	if len(h) != 1 || h[0].Type != TypeNewEnquiry || h[0].Source != SourceApp || !h[0].ChangedAt.Equal(r.clock.t) {
		t.Fatalf("history = %+v", h)
	}
	if ok, _ := r.svc.EmailAllowed(ctx, r.user, TypeNewEnquiry); ok {
		t.Fatal("email of a switched-off type allowed")
	}
	if ok, _ := r.svc.EmailAllowed(ctx, r.user, TypeAccount); !ok {
		t.Fatal("essential email refused")
	}
	if SourceFromClient("web") != SourceWeb || SourceFromClient("android") != SourceApp || SourceFromClient("") != SourceAPI || SourceFromClient("curl") != SourceAPI {
		t.Fatal("X-Client mapping")
	}
}

// FR-045: logout removes the session's devices and the given token; never
// another user's.
func TestDevicesOnLogout(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	session, other := uuid.New(), uuid.New()
	a := r.register(r.user, "tok-a", &session)
	b := r.register(r.user, "tok-b", &other)
	stranger := uuid.New()
	s := r.register(stranger, "tok-stranger", nil)
	if err := r.svc.OnLogout(ctx, r.user, session, "tok-stranger"); err != nil {
		t.Fatal(err)
	}
	ids := map[uuid.UUID]bool{}
	devs, _ := r.svc.Devices(ctx, r.user)
	for _, d := range devs {
		ids[d.ID] = true
	}
	if ids[a.ID] || !ids[b.ID] || !ids[r.device.ID] {
		t.Fatalf("after logout: %v", devs)
	}
	if devs, _ := r.svc.Devices(ctx, stranger); len(devs) != 1 || devs[0].ID != s.ID {
		t.Fatal("logout removed another user's token")
	}
	if err := r.svc.OnLogout(ctx, r.user, other, "tok-"+r.device.Token[4:]); err != nil {
		t.Fatal(err)
	}
	if devs, _ := r.svc.Devices(ctx, r.user); len(devs) != 0 {
		t.Fatalf("after second logout with its token: %v", devs)
	}
	// Validation of a registration.
	_, _, err := r.svc.RegisterDevice(ctx, r.user, nil, DeviceInput{Token: "", Platform: "PALM", AppVersion: ""})
	var v *ValidationError
	if !errors.As(err, &v) || v.Fields["token"] == "" || v.Fields["platform"] == "" || v.Fields["app_version"] == "" || v.Fields["push_consent"] == "" {
		t.Fatalf("validation: %v", err)
	}
}
