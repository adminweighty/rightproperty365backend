package subscriptions

import (
	"testing"
	"time"
)

func TestEndStaysInTargetMonth(t *testing.T) {
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 10, 0, 0, 0, time.UTC) }
	cases := []struct {
		period string
		start  time.Time
		want   time.Time
	}{
		{PeriodQuarterly, d(2026, 11, 16), d(2027, 2, 16)},
		{PeriodQuarterly, d(2026, 11, 30), d(2027, 2, 28)},
		{PeriodQuarterly, d(2027, 11, 30), d(2028, 2, 29)},
		{PeriodQuarterly, d(2026, 10, 31), d(2027, 1, 31)},
		{PeriodQuarterly, d(2026, 12, 31), d(2027, 3, 31)},
		{PeriodYearly, d(2028, 2, 29), d(2029, 2, 28)},
		{PeriodYearly, d(2026, 11, 16), d(2027, 11, 16)},
	}
	for _, c := range cases {
		if got := End(c.period, c.start); !got.Equal(c.want) {
			t.Errorf("End(%s, %s) = %s, want %s", c.period, c.start.Format("2006-01-02"), got.Format("2006-01-02"), c.want.Format("2006-01-02"))
		}
	}
}
