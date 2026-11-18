package inapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// storeContract is what every Store must do; it runs on the memory store
// and (with RP365_TEST_DATABASE_URL) on Postgres.
func storeContract(t *testing.T, s Store, alice, bob uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	session := uuid.New()
	token := "tok-" + uuid.NewString()

	// Devices: register, re-register (same row), move to another user.
	d1, created, err := s.UpsertDevice(ctx, Device{UserID: alice, SessionID: &session, Token: token, Platform: PlatformIOS, AppVersion: "1.0",
		Locale: "en-ZW", PushConsent: true, LastSeenAt: now})
	if err != nil || !created {
		t.Fatalf("register: %v %v", created, err)
	}
	d1b, created, err := s.UpsertDevice(ctx, Device{UserID: alice, SessionID: &session, Token: token, Platform: PlatformIOS, AppVersion: "1.1",
		PushConsent: true, LastSeenAt: now.Add(time.Minute)})
	if err != nil || created || d1b.ID != d1.ID || d1b.AppVersion != "1.1" {
		t.Fatalf("re-register: %+v %v %v", d1b, created, err)
	}
	if err := s.DeleteDevice(ctx, bob, d1.ID); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("another user's device: %v", err)
	}
	if err := s.DeleteDeviceByToken(ctx, &bob, token); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("another user's token: %v", err)
	}

	// Notifications: one per (event, user).
	ev := uuid.New()
	draft := Draft{SourceEventID: ev, UserID: alice, Type: TypeNewLead, Title: "New property enquiry", Body: "PR-1 · Borrowdale",
		Link: "/agent/requests/1", Reference: "PR-1"}
	n1, created, err := s.CreateNotification(ctx, draft, true, now)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	n2, created, err := s.CreateNotification(ctx, draft, true, now.Add(time.Second))
	if err != nil || created || n2.ID != n1.ID {
		t.Fatalf("redelivery created another: %v %v", created, err)
	}
	other := draft
	other.UserID = bob
	if _, created, err := s.CreateNotification(ctx, other, false, now); err != nil || !created {
		t.Fatalf("same event, other recipient: %v %v", created, err)
	}
	if c, _ := s.UnreadCount(ctx, alice); c != 1 {
		t.Fatalf("unread = %d", c)
	}
	if _, err := s.MarkRead(ctx, bob, n1.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong user mark read: %v", err)
	}
	items, total, err := s.List(ctx, alice, true, 0, PageSize)
	if err != nil || total != 1 || len(items) != 1 || items[0].Reference != "PR-1" {
		t.Fatalf("list: %v %d %v", items, total, err)
	}

	// Push: claimed once, recorded, counted in the burst window.
	jobs, err := s.ClaimPush(ctx, now, 10, time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].Token != token || jobs[0].Attempts != 1 || jobs[0].Notification.ID != n1.ID {
		t.Fatalf("claim: %+v %v", jobs, err)
	}
	if again, _ := s.ClaimPush(ctx, now, 10, time.Minute); len(again) != 0 {
		t.Fatalf("a leased delivery was claimed twice")
	}
	batch := uuid.New()
	if err := s.RecordPush(ctx, PushOutcome{DeliveryIDs: []uuid.UUID{jobs[0].DeliveryID}, Status: StatusProviderAccepted, MessageID: "m-1",
		BatchID: batch, At: now}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PushBatchesSince(ctx, d1.ID, now.Add(-time.Minute)); n != 1 {
		t.Fatalf("batches = %d", n)
	}
	if err := s.RecordEmail(ctx, n1.ID, StatusProviderAccepted, 1, "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEmail(ctx, n1.ID, StatusDead, 2, "rejected", now); err != nil {
		t.Fatal(err)
	}
	h, err := s.Health(ctx, now)
	if err != nil || h.Counts[ChannelPush][StatusProviderAccepted] < 1 || h.Counts[ChannelEmail][StatusDead] < 1 {
		t.Fatalf("health: %+v %v", h, err)
	}

	// A token moving to bob: alice no longer has it.
	if _, _, err := s.UpsertDevice(ctx, Device{UserID: bob, Token: token, Platform: PlatformAndroid, AppVersion: "2.0", PushConsent: true,
		LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if devs, _ := s.Devices(ctx, alice); len(devs) != 0 {
		t.Fatalf("alice still has the moved token: %v", devs)
	}
	if devs, _ := s.Devices(ctx, bob); len(devs) != 1 {
		t.Fatalf("bob devices: %v", devs)
	}
	// Logout of the session that registered a device removes it.
	s2 := uuid.New()
	if _, _, err := s.UpsertDevice(ctx, Device{UserID: alice, SessionID: &s2, Token: "tok-" + uuid.NewString(), Platform: PlatformIOS,
		AppVersion: "1", PushConsent: true, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DeleteSessionDevices(ctx, alice, s2); err != nil || n != 1 {
		t.Fatalf("session devices: %d %v", n, err)
	}
	// An invalid token is removed whoever holds it.
	if err := s.DeleteDeviceByToken(ctx, nil, token); err != nil {
		t.Fatal(err)
	}
	if devs, _ := s.Devices(ctx, bob); len(devs) != 0 {
		t.Fatalf("bob devices after removal: %v", devs)
	}

	// Preferences: changes are recorded; a no-op is not.
	off := false
	if err := s.SetPreferences(ctx, alice, []PreferenceChange{{Type: TypeNewLead, Email: &off}}, SourceWeb, now); err != nil {
		t.Fatal(err)
	}
	prefs, err := s.Preferences(ctx, alice)
	if err != nil || prefs[TypeNewLead].Email || !prefs[TypeNewLead].Push || prefs[TypeNewLead].Source != SourceWeb || prefs[TypeNewLead].ChangedAt == nil {
		t.Fatalf("prefs: %+v %v", prefs, err)
	}
	if err := s.SetPreferences(ctx, alice, []PreferenceChange{{Type: TypeNewLead, Email: &off}}, SourceApp, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if prefs, _ := s.Preferences(ctx, alice); prefs[TypeNewLead].Source != SourceWeb {
		t.Fatalf("a no-op changed the source: %+v", prefs[TypeNewLead])
	}
	if c, err := s.MarkAllRead(ctx, alice, now); err != nil || c != 1 {
		t.Fatalf("mark all read: %d %v", c, err)
	}
}

func TestMemoryStoreContract(t *testing.T) {
	s := NewMemoryStore()
	alice, bob := uuid.New(), uuid.New()
	storeContract(t, s, alice, bob)
	if len(s.History()) != 1 {
		t.Fatalf("history = %+v", s.History())
	}
	s.DeleteUser(alice)
	if n, _ := s.UnreadCount(context.Background(), alice); n != 0 {
		t.Fatal("deletion kept notifications")
	}
	if p, _ := s.Preferences(context.Background(), alice); len(p) != 0 {
		t.Fatal("deletion kept preferences")
	}
}
