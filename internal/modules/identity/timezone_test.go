package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// timeZoneStoreContract is run on the memory store and, behind
// RP365_TEST_DATABASE_URL, on Postgres (API 0.62.0).
func timeZoneStoreContract(t *testing.T, store Store, suffix string) {
	t.Helper()
	ctx := context.Background()
	id := newMarketingUser(t, store, "tz-"+suffix+"@example.test", false, true, time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	load := func() User {
		u, err := store.UserByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if v := load().timeZoneView(); v.Mode != TimeZoneMarket || v.Zone != nil || v.MarketCode != HomeMarketCode {
		t.Fatalf("a new account: %+v", v)
	}
	if err := store.SetTimeZone(ctx, id, TimeZoneNamed, "Europe/London"); err != nil {
		t.Fatal(err)
	}
	if u := load(); u.TimeZoneMode != TimeZoneNamed || u.TimeZone != "Europe/London" {
		t.Fatalf("named: %q %q", u.TimeZoneMode, u.TimeZone)
	}
	if err := store.SetTimeZone(ctx, id, TimeZoneDevice, ""); err != nil {
		t.Fatal(err)
	}
	if u := load(); u.TimeZoneMode != TimeZoneDevice || u.TimeZone != "" {
		t.Fatalf("device: %q %q", u.TimeZoneMode, u.TimeZone)
	}
	if err := store.SetTimeZone(ctx, id, TimeZoneMarket, ""); err != nil {
		t.Fatal(err)
	}
	if u := load(); u.timeZoneView().Mode != TimeZoneMarket {
		t.Fatalf("market: %q", u.TimeZoneMode)
	}
	if err := store.SetTimeZone(ctx, uuid.New(), TimeZoneMarket, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestMemoryTimeZoneStoreContract(t *testing.T) {
	timeZoneStoreContract(t, NewMemoryStore(), uuid.NewString()[:8])
}

func TestPostgresTimeZoneStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	timeZoneStoreContract(t, NewPostgresStore(pool), uuid.NewString()[:8])
	// The table refuses a named mode without a zone and a zone without it.
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE users SET time_zone_mode = 'ZONE', time_zone = NULL WHERE email LIKE 'tz-%@example.test'`); err == nil {
		t.Fatal("ZONE without a zone was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET time_zone_mode = 'DEVICE', time_zone = 'Europe/London' WHERE email LIKE 'tz-%@example.test'`); err == nil {
		t.Fatal("DEVICE with a zone was accepted")
	}
}

func TestValidTimeZone(t *testing.T) {
	for _, z := range []string{"Africa/Harare", "Europe/London", "America/New_York", "Pacific/Kiritimati", "Pacific/Pago_Pago",
		"Asia/Calcutta", "UTC", "Etc/GMT+12", "America/Argentina/Buenos_Aires"} {
		if !ValidTimeZone(z) {
			t.Errorf("%s refused", z)
		}
	}
	for _, z := range []string{"", "Local", "Mars/Olympus", "Europe/London ", "../etc/passwd", "europe/london_x", string(make([]byte, 65))} {
		if ValidTimeZone(z) {
			t.Errorf("%q accepted", z)
		}
	}
}

// The service validates the combination before anything is stored.
func TestSetTimeZoneValidation(t *testing.T) {
	store := NewMemoryStore()
	svc := newMarketingService(t, store, time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	id := newMarketingUser(t, store, "tzv@example.test", false, true, time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	zone := func(s string) *string { return &s }
	for _, c := range []struct {
		mode  string
		zone  *string
		field string
	}{
		{"", nil, "mode"}, {"UTC", nil, "mode"}, {"market", nil, "mode"},
		{TimeZoneNamed, nil, "zone"}, {TimeZoneNamed, zone("Local"), "zone"}, {TimeZoneNamed, zone("Mars/Olympus"), "zone"},
		{TimeZoneDevice, zone("Europe/London"), "zone"}, {TimeZoneMarket, zone(""), "zone"},
	} {
		_, err := svc.SetTimeZone(context.Background(), id, c.mode, c.zone)
		var v *ValidationError
		if !errors.As(err, &v) || v.Fields[c.field] == "" {
			t.Errorf("%q %v: %v, want a %s error", c.mode, c.zone, err, c.field)
		}
	}
	u, err := svc.SetTimeZone(context.Background(), id, TimeZoneNamed, zone("Pacific/Kiritimati"))
	if err != nil || u.View().TimeZone.Zone == nil || *u.View().TimeZone.Zone != "Pacific/Kiritimati" {
		t.Fatalf("named: %+v, %v", u.View().TimeZone, err)
	}
}
