package marketday

import (
	"context"
	"testing"
	"time"
)

func TestDay(t *testing.T) {
	late := time.Date(2026, 11, 21, 22, 30, 0, 0, time.UTC) // 00:30 on the 22nd in Harare
	for _, c := range []struct{ zone, want string }{
		{"Africa/Harare", "2026-11-22"},
		{"America/New_York", "2026-11-21"},
		{"", "2026-11-21"},
		{"Nowhere/Unknown", "2026-11-21"},
		{"Local", "2026-11-21"},
	} {
		if got := Day(late, c.zone); got != c.want {
			t.Errorf("Day(%q) = %s, want %s", c.zone, got, c.want)
		}
	}
}

func TestToday(t *testing.T) {
	ctx := context.Background()
	late := time.Date(2026, 11, 21, 22, 30, 0, 0, time.UTC)
	zones := func(_ context.Context, code string) string {
		if code == "ZW" {
			return "Africa/Harare"
		}
		return ""
	}
	if got := Today(ctx, zones, "ZW", late); got != "2026-11-22" {
		t.Errorf("ZW = %s", got)
	}
	if got := Today(ctx, zones, "XX", late); got != "2026-11-21" {
		t.Errorf("unknown market = %s", got)
	}
	if got := Today(ctx, nil, "ZW", late); got != "2026-11-21" {
		t.Errorf("no zone lookup = %s", got)
	}
	if got := Today(ctx, zones, "", late); got != "2026-11-21" {
		t.Errorf("no market = %s", got)
	}
}
