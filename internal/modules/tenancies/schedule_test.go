package tenancies

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDueDates(t *testing.T) {
	for _, c := range []struct {
		name, freq     string
		day            int
		start, through string
		want           []string
	}{
		{"monthly on the 1st from the 1st", FrequencyMonthly, 1, "2026-10-01", "2027-01-31", []string{"2026-10-01", "2026-11-01", "2026-12-01", "2027-01-01"}},
		{"monthly: the first due day on or after the start", FrequencyMonthly, 1, "2026-10-15", "2027-01-01", []string{"2026-11-01", "2026-12-01", "2027-01-01"}},
		{"monthly: later the same month", FrequencyMonthly, 20, "2026-10-15", "2026-12-31", []string{"2026-10-20", "2026-11-20", "2026-12-20"}},
		{"day 31: short months use their last day, no drift", FrequencyMonthly, 31, "2027-01-01", "2027-06-30",
			[]string{"2027-01-31", "2027-02-28", "2027-03-31", "2027-04-30", "2027-05-31", "2027-06-30"}},
		{"day 31 in a leap February", FrequencyMonthly, 31, "2028-01-31", "2028-03-31", []string{"2028-01-31", "2028-02-29", "2028-03-31"}},
		{"day 30 in a leap February", FrequencyMonthly, 30, "2028-02-01", "2028-03-31", []string{"2028-02-29", "2028-03-30"}},
		{"day 29 in a common February", FrequencyMonthly, 29, "2027-02-01", "2027-03-31", []string{"2027-02-28", "2027-03-29"}},
		{"day 31 starting on 30 April: April's last day is the 30th", FrequencyMonthly, 31, "2027-04-30", "2027-05-31", []string{"2027-04-30", "2027-05-31"}},
		{"day 31 starting on 29 April", FrequencyMonthly, 31, "2027-04-29", "2027-05-31", []string{"2027-04-30", "2027-05-31"}},
		{"weekly on Monday (ISO 1) from a Thursday", FrequencyWeekly, 1, "2026-11-19", "2026-12-14", []string{"2026-11-23", "2026-11-30", "2026-12-07", "2026-12-14"}},
		{"weekly on Thursday from a Thursday", FrequencyWeekly, 4, "2026-11-19", "2026-12-03", []string{"2026-11-19", "2026-11-26", "2026-12-03"}},
		{"weekly on Sunday (ISO 7)", FrequencyWeekly, 7, "2026-11-19", "2026-11-30", []string{"2026-11-22", "2026-11-29"}},
		{"quarterly on day 31", FrequencyQuarterly, 31, "2026-11-19", "2027-12-31", []string{"2026-11-30", "2027-02-28", "2027-05-31", "2027-08-31", "2027-11-30"}},
		{"quarterly from the next month", FrequencyQuarterly, 15, "2026-11-19", "2027-06-30", []string{"2026-12-15", "2027-03-15", "2027-06-15"}},
		{"annually on day 29 from a leap year", FrequencyAnnually, 29, "2028-02-01", "2031-12-31", []string{"2028-02-29", "2029-02-28", "2030-02-28", "2031-02-28"}},
		{"through is inclusive", FrequencyMonthly, 5, "2026-01-05", "2026-03-05", []string{"2026-01-05", "2026-02-05", "2026-03-05"}},
		{"through before the first due", FrequencyMonthly, 20, "2026-10-21", "2026-11-19", nil},
		{"through before the start", FrequencyMonthly, 1, "2026-10-01", "2026-09-30", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := DueDates(c.freq, c.day, c.start, c.through)
			if !slices.Equal(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestScheduleThroughAndPeriods(t *testing.T) {
	today := "2026-11-19"
	if got := RollingHorizon(today); got != "2027-11-30" {
		t.Fatalf("rolling horizon %s", got)
	}
	if got := RollingHorizon("2027-01-31"); got != "2028-01-31" {
		t.Fatalf("rolling horizon from January %s", got)
	}
	base := Tenancy{RentFrequency: FrequencyMonthly, RentDueDay: 31, StartDate: "2026-12-01", Status: StatusActive}
	for _, c := range []struct {
		name         string
		end, endedOn string
		want         string
		rolling      bool
	}{
		{"no end: rolling", "", "", "2027-11-30", true},
		{"agreed end", "2027-05-31", "", "2027-05-31", false},
		{"ended: the actual end", "2027-05-31", "2027-02-10", "2027-02-10", false},
		{"an end more than 10 years away is rolling", "2040-01-01", "", "2027-11-30", true},
	} {
		tn := base
		tn.EndDate, tn.EndedOn = c.end, c.endedOn
		got, rolling := ScheduleThrough(tn, today)
		if got != c.want || rolling != c.rolling {
			t.Fatalf("%s: %s %v", c.name, got, rolling)
		}
	}
	// Periods run to the day before the next due, cut at the end.
	tn := base
	tn.EndDate = "2027-02-15"
	if got := PeriodEnd(tn, 1, "2026-12-31"); got != "2027-01-30" {
		t.Fatalf("period of the first due ends %s", got)
	}
	if got := PeriodEnd(tn, 3, "2027-02-28"); got != "2027-02-28" {
		t.Fatalf("a due after the end: its period is the day itself, got %s", got)
	}
	if got := PeriodEnd(tn, 2, "2027-01-31"); got != "2027-02-15" {
		t.Fatalf("cut at the end: %s", got)
	}
	if RuleText(FrequencyMonthly, 31) != "Monthly on day 31 (the last day in shorter months)" || RuleText(FrequencyWeekly, 1) != "Weekly on Monday" ||
		RuleText(FrequencyQuarterly, 15) != "Quarterly on day 15" || !strings.HasPrefix(RuleText(FrequencyAnnually, 1), "Annually") {
		t.Fatal("rule text")
	}
}

// apply keeps a plan's result, as a store would.
func applyPlan(existing []Due, add []Due, remove []string) []Due {
	var out []Due
	for _, d := range existing {
		if !slices.Contains(remove, d.ID.String()) {
			out = append(out, d)
		}
	}
	out = append(out, add...)
	slices.SortFunc(out, func(a, b Due) int { return strings.Compare(a.DueDate, b.DueDate) })
	return out
}

func ids(list []Due) []string {
	var out []string
	for _, d := range list {
		out = append(out, d.ID.String())
	}
	return out
}

func TestPlanScheduleIdempotentAndRolling(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	tn := Tenancy{ID: newID(), RentFrequency: FrequencyMonthly, RentDueDay: 1, RentAmount: "950.00", RentCurrency: "USD",
		StartDate: "2026-10-01", Status: StatusActive}
	// Activation: October 2026 to November 2027 (the end of the month 12 months ahead).
	add, remove := planSchedule(tn, nil, "2026-11-19", now)
	if len(add) != 14 || len(remove) != 0 || add[0].DueDate != "2026-10-01" || add[13].DueDate != "2027-11-01" || add[0].Sequence != 1 ||
		add[13].Sequence != 14 || add[5].Amount != "950.00" || add[5].Currency != "USD" {
		t.Fatalf("activation: %d dues, first %+v, last %+v", len(add), add[0], add[len(add)-1])
	}
	dues := applyPlan(nil, add, nil)
	// Re-running the same day, or later the same month, adds nothing.
	for _, day := range []string{"2026-11-19", "2026-11-30"} {
		if add, remove := planSchedule(tn, dues, day, now); len(add) != 0 || len(remove) != 0 {
			t.Fatalf("%s: not idempotent: +%d -%d", day, len(add), len(remove))
		}
	}
	// The month turns: one due more (December 2027), then nothing again.
	add, remove = planSchedule(tn, dues, "2026-12-01", now)
	if len(add) != 1 || add[0].DueDate != "2027-12-01" || add[0].Sequence != 15 || len(remove) != 0 {
		t.Fatalf("monthly extension: %+v", add)
	}
	dues = applyPlan(dues, add, nil)
	if add, _ := planSchedule(tn, dues, "2026-12-15", now); len(add) != 0 {
		t.Fatal("extension not idempotent")
	}
	// Ending early removes the later dues that have no allocation and no
	// waiver; a paid one and a waived one stay.
	waivedAt := now
	dues[4].Allocations = []DueAllocation{{ReceiptID: newID(), Amount: "950.00", ReceivedOn: "2026-11-02"}}
	dues[5].WaivedAt, dues[5].WaiveReason = &waivedAt, "goodwill"
	ended := tn
	ended.Status, ended.EndedOn = StatusEnded, "2027-01-15"
	add, remove = planSchedule(ended, dues, "2026-12-15", now)
	if len(add) != 0 || len(remove) != len(dues)-6 || slices.Contains(remove, dues[4].ID) || slices.Contains(remove, dues[5].ID) {
		t.Fatalf("end: +%d -%d", len(add), len(remove))
	}
	left := applyPlan(dues, nil, ids(dues[6:]))
	if len(left) != 6 || left[3].DueDate != "2027-01-01" {
		t.Fatalf("after the end: %v", ids(left))
	}
	// A DRAFT has no schedule.
	draft := tn
	draft.Status = StatusDraft
	if add, _ := planSchedule(draft, nil, "2026-11-19", now); len(add) != 0 {
		t.Fatal("a draft has dues")
	}
	// An agreed end: generated through it at once, and the worker adds nothing later.
	fixed := tn
	fixed.EndDate = "2027-03-31"
	add, _ = planSchedule(fixed, nil, "2026-11-19", now)
	if len(add) != 6 || add[5].DueDate != "2027-03-01" {
		t.Fatalf("fixed term: %d", len(add))
	}
	if more, _ := planSchedule(fixed, applyPlan(nil, add, nil), "2027-06-01", now); len(more) != 0 {
		t.Fatal("a fixed term grew")
	}
}
