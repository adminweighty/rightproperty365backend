package identity

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type nopMailer struct{}

func (nopMailer) SendVerification(context.Context, string, string, string) error     { return nil }
func (nopMailer) SendPasswordReset(context.Context, string, string, string) error    { return nil }
func (nopMailer) SendAccountExists(context.Context, string, string) error            { return nil }
func (nopMailer) SendSignInCode(context.Context, string, string, string, bool) error { return nil }

func newMarketingService(t *testing.T, store Store, now time.Time) *Service {
	t.Helper()
	svc, err := NewService(store, nopMailer{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithHasher(Hasher{MemoryKiB: 64, Time: 1, Threads: 1}), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func newMarketingUser(t *testing.T, store Store, email string, optIn, verified bool, now time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	u := NewUser{ID: id, Name: "Marketing Tester", Email: email, PasswordHash: "x", MarketingOptIn: optIn, MarketingSource: "web",
		PrivacyVersion: "2026-01", AccountRoles: []AccountRole{{Role: AccountRoleConsumer, Status: AccountRoleActive}}, CreatedAt: now}
	if verified {
		at := now
		u.EmailVerifiedAt = &at
	}
	if err := store.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return id
}

// marketingStoreContract is run on the memory store and, behind
// RP365_TEST_DATABASE_URL, on Postgres.
func marketingStoreContract(t *testing.T, store Store, suffix string) {
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)

	// Registration with opt-in writes a REGISTRATION record; without, none.
	in := newMarketingUser(t, store, "mkt-in-"+suffix+"@example.test", true, true, now)
	out := newMarketingUser(t, store, "mkt-out-"+suffix+"@example.test", false, true, now)
	m, err := store.MarketingState(ctx, in, MarketingHistoryLimit)
	if err != nil || !m.OptIn || len(m.History) != 1 {
		t.Fatalf("opted-in registration = %+v %v", m, err)
	}
	if e := m.History[0]; !e.OptIn || e.Context != MarketingContextRegistration || e.Source != "web" ||
		e.ConsentTextVersion != CurrentMarketingConsentVersion || !e.ChangedAt.Equal(now) {
		t.Fatalf("registration record = %+v", e)
	}
	if m, err := store.MarketingState(ctx, out, MarketingHistoryLimit); err != nil || m.OptIn || len(m.History) != 0 {
		t.Fatalf("no-choice registration = %+v %v", m, err)
	}

	// A change writes; the same value writes nothing.
	ev := MarketingConsentEvent{ID: uuid.Must(uuid.NewV7()), UserID: out, OptIn: true, ChangedAt: now.Add(time.Minute),
		Source: "app", Context: MarketingContextPreferences, ConsentTextVersion: CurrentMarketingConsentVersion}
	if changed, err := store.ChangeMarketingOptIn(ctx, ev); err != nil || !changed {
		t.Fatalf("change = %v %v", changed, err)
	}
	ev.ID = uuid.Must(uuid.NewV7())
	if changed, err := store.ChangeMarketingOptIn(ctx, ev); err != nil || changed {
		t.Fatalf("same value = %v %v", changed, err)
	}
	// Newest first, at most the limit.
	for i := range 25 {
		e := MarketingConsentEvent{ID: uuid.Must(uuid.NewV7()), UserID: out, OptIn: i%2 == 1, ChangedAt: now.Add(time.Duration(i+2) * time.Minute),
			Source: "web", Context: MarketingContextPreferences}
		if e.OptIn {
			e.ConsentTextVersion = CurrentMarketingConsentVersion
		}
		if _, err := store.ChangeMarketingOptIn(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	m, err = store.MarketingState(ctx, out, MarketingHistoryLimit)
	if err != nil || len(m.History) != MarketingHistoryLimit || m.OptIn != m.History[0].OptIn {
		t.Fatalf("history = %d %v", len(m.History), err)
	}
	for i := 1; i < len(m.History); i++ {
		if !m.History[i-1].ChangedAt.After(m.History[i].ChangedAt) {
			t.Fatalf("not newest first at %d", i)
		}
	}
	if !m.History[0].ChangedAt.Equal(now.Add(26 * time.Minute)) {
		t.Fatalf("newest = %s", m.History[0].ChangedAt)
	}

	if e, err := store.MarketingEligibility(ctx, in); err != nil || e.Status != StatusActive || !e.EmailVerified || !e.OptIn {
		t.Fatalf("eligibility = %+v %v", e, err)
	}
	if _, err := store.MarketingState(ctx, uuid.New(), MarketingHistoryLimit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	if _, err := store.ChangeMarketingOptIn(ctx, MarketingConsentEvent{ID: uuid.New(), UserID: uuid.New(), OptIn: true,
		ChangedAt: now, Source: "api", Context: MarketingContextPreferences}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown change = %v", err)
	}
}

func TestMemoryMarketingStoreContract(t *testing.T) {
	marketingStoreContract(t, NewMemoryStore(), "mem")
}

// A pending registration that changes the choice is recorded; one that
// keeps it is not.
func TestPendingRegistrationRecordsAChange(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	id := newMarketingUser(t, store, "pending@example.test", false, false, now)
	again := func(optIn bool, at time.Time) {
		if err := store.UpdatePendingRegistration(context.Background(), NewUser{ID: id, Name: "P", PasswordHash: "y", MarketingOptIn: optIn,
			MarketingSource: "app", PrivacyVersion: "2026-01", CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	again(false, now.Add(time.Minute))
	if n := len(store.MarketingConsents(id)); n != 0 {
		t.Fatalf("records = %d", n)
	}
	again(true, now.Add(2*time.Minute))
	again(true, now.Add(3*time.Minute))
	got := store.MarketingConsents(id)
	if len(got) != 1 || !got[0].OptIn || got[0].Source != "app" || got[0].Context != MarketingContextRegistration {
		t.Fatalf("records = %+v", got)
	}
}

func TestSetMarketingPreferences(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	svc := newMarketingService(t, store, now)
	id := newMarketingUser(t, store, "svc@example.test", false, true, now)

	stale, empty := "marketing-2025-01", ""
	for _, v := range []*string{&stale, &empty} {
		var verr *ValidationError
		if _, err := svc.SetMarketingPreferences(ctx, id, MarketingPreferenceInput{OptIn: true, ConsentTextVersion: v, Source: "web"}); !errors.As(err, &verr) ||
			verr.Fields["consent_text_version"] == "" {
			t.Fatalf("version %q: %v", *v, err)
		}
	}
	var verr *ValidationError
	if _, err := svc.SetMarketingPreferences(ctx, id, MarketingPreferenceInput{OptIn: true, ConsentTextVersion: &stale}); !errors.As(err, &verr) ||
		verr.Fields["consent_text_version"] != msgConsentVersionStale {
		t.Fatalf("stale message: %v", err)
	}
	if n := len(store.MarketingConsents(id)); n != 0 {
		t.Fatalf("a refused change wrote %d records", n)
	}
	// Opt-out ignores the version, even a stale one; it is a no-op here.
	m, err := svc.SetMarketingPreferences(ctx, id, MarketingPreferenceInput{OptIn: false, ConsentTextVersion: &stale, Source: "app"})
	if err != nil || m.OptIn || len(m.History) != 0 {
		t.Fatalf("no-op opt-out = %+v %v", m, err)
	}
	// An unknown source is recorded as api; the version defaults to current.
	m, err = svc.SetMarketingPreferences(ctx, id, MarketingPreferenceInput{OptIn: true, Source: "curl"})
	if err != nil || !m.OptIn || len(m.History) != 1 || m.History[0].Source != "api" ||
		m.History[0].ConsentTextVersion != CurrentMarketingConsentVersion || m.History[0].Context != MarketingContextPreferences {
		t.Fatalf("opt-in = %+v %v", m, err)
	}
}

// MarketingAllowed: active, verified and opted in; false in every other case.
func TestMarketingAllowed(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	svc := newMarketingService(t, store, now)

	yes := newMarketingUser(t, store, "yes@example.test", true, true, now)
	notOptedIn := newMarketingUser(t, store, "no@example.test", false, true, now)
	unverified := newMarketingUser(t, store, "unverified@example.test", true, false, now)
	suspended := newMarketingUser(t, store, "suspended@example.test", true, true, now)
	if _, err := store.SuspendAccount(suspended, "test", nil, now); err != nil {
		t.Fatal(err)
	}
	deleted := newMarketingUser(t, store, "deleted@example.test", true, true, now)
	if _, err := store.AnonymiseAccount(deleted, "Deleted user", "deleted-"+deleted.String()+"@deleted.invalid", now); err != nil {
		t.Fatal(err)
	}
	optedOut := newMarketingUser(t, store, "optedout@example.test", true, true, now)
	if _, err := svc.SetMarketingPreferences(ctx, optedOut, MarketingPreferenceInput{OptIn: false, Source: "app"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"active, verified, opted in", yes, true},
		{"never opted in", notOptedIn, false},
		{"email not verified", unverified, false},
		{"suspended", suspended, false},
		{"deleted", deleted, false},
		{"opted out later", optedOut, false},
		{"unknown account", uuid.New(), false},
	} {
		if got := svc.MarketingAllowed(ctx, c.id); got != c.want {
			t.Errorf("%s: MarketingAllowed = %v, want %v", c.name, got, c.want)
		}
	}
}

// draftConsentText is the wording migration 0041 inserts for marketing-2026-01.
const draftConsentText = "Send me news, tips and offers from RightProperty365 by email. I can stop them at any time."

// The current version names a wording row: DRAFT, with the migration's text.
func consentTextContract(t *testing.T, store Store) {
	t.Helper()
	got, err := store.MarketingConsentText(context.Background(), CurrentMarketingConsentVersion)
	if err != nil || got.Version != CurrentMarketingConsentVersion || got.Text != draftConsentText || got.Status != ConsentTextDraft {
		t.Fatalf("current text = %+v %v", got, err)
	}
	if _, err := store.MarketingConsentText(context.Background(), "marketing-1999-01"); !errors.Is(err, ErrConsentTextNotFound) {
		t.Fatalf("unknown version = %v", err)
	}
}

func TestMemoryConsentText(t *testing.T) {
	consentTextContract(t, NewMemoryStore())
	svc := newMarketingService(t, NewMemoryStore(), time.Now())
	if _, err := svc.MarketingConsentText(context.Background(), strings.Repeat("v", 41)); !errors.Is(err, ErrConsentTextNotFound) {
		t.Fatalf("long version = %v", err)
	}
}
