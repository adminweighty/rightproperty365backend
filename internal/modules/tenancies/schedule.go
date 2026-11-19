package tenancies

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The rent schedule rule (API 0.36.1, contract "The schedule rule").
//
//   - The first due is the first due day on or after the start date; every
//     due is the full rent amount (no pro-rating).
//   - WEEKLY: every 7 days on ISO weekday due_day (1 = Monday ... 7 = Sunday).
//   - MONTHLY / QUARTERLY / ANNUALLY: day due_day of every 1st / 3rd / 12th
//     month counted from the month of the first due. A month without that
//     day uses its last day (due_day 31 in February: the 28th or 29th). Each
//     occurrence is computed from the first one, so the clamp never drifts
//     (31 Jan, 28 Feb, 31 Mar).
//   - The schedule runs through the tenancy's end: ended_on once it has
//     ended, else the agreed end_date, else (rolling) the last day of the
//     month 12 months after the current month. An agreed end more than
//     MaxScheduleYears away is treated as rolling. The worker re-runs the
//     rule daily, so a rolling schedule gains a month when the month turns.

// MaxScheduleYears is how far ahead an agreed end date is generated at once;
// a later end date is treated as rolling.
const MaxScheduleYears = 10

// RollingMonths is the rolling horizon: through the end of the month this
// many months after the current one.
const RollingMonths = 12

func parseDay(s string) time.Time {
	t, _ := time.Parse(time.DateOnly, s)
	return t
}

func fmtDay(t time.Time) string { return t.Format(time.DateOnly) }

// daysIn returns the number of days of a month.
func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// clampDay returns day d of the month (year, month), or its last day when it is shorter.
func clampDay(year int, month time.Month, d int) time.Time {
	// Normalise the month first (month may be beyond 12).
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	return time.Date(first.Year(), first.Month(), min(d, daysIn(first.Year(), first.Month())), 0, 0, 0, 0, time.UTC)
}

func monthStep(frequency string) int {
	switch frequency {
	case FrequencyQuarterly:
		return 3
	case FrequencyAnnually:
		return 12
	}
	return 1
}

// Occurrence returns the k-th due date (k = 0 is the first) of a rent rule
// starting on start ("YYYY-MM-DD").
func Occurrence(frequency string, dueDay int, start string, k int) string {
	s := parseDay(start)
	if frequency == FrequencyWeekly {
		iso := int(s.Weekday())
		if iso == 0 {
			iso = 7
		}
		first := s.AddDate(0, 0, (dueDay-iso+7)%7)
		return fmtDay(first.AddDate(0, 0, 7*k))
	}
	anchor := time.Date(s.Year(), s.Month(), 1, 0, 0, 0, 0, time.UTC)
	if clampDay(s.Year(), s.Month(), dueDay).Before(s) {
		anchor = anchor.AddDate(0, 1, 0)
	}
	return fmtDay(clampDay(anchor.Year(), anchor.Month()+time.Month(k*monthStep(frequency)), dueDay))
}

// DueDates returns the due dates of a rent rule from start through through
// (both inclusive), oldest first.
func DueDates(frequency string, dueDay int, start, through string) []string {
	var out []string
	if start == "" || through == "" || through < start {
		return out
	}
	for k := 0; ; k++ {
		d := Occurrence(frequency, dueDay, start, k)
		if d > through {
			return out
		}
		out = append(out, d)
	}
}

// RollingHorizon is the last day of the month RollingMonths after today's.
func RollingHorizon(today string) string {
	t := parseDay(today)
	first := time.Date(t.Year(), t.Month()+RollingMonths+1, 1, 0, 0, 0, 0, time.UTC)
	return fmtDay(first.AddDate(0, 0, -1))
}

// End returns the tenancy's end for the schedule: the actual end once it has
// ended, else the agreed end date ("" when none).
func (t Tenancy) End() string {
	if t.EndedOn != "" {
		return t.EndedOn
	}
	return t.EndDate
}

// ScheduleThrough returns the last date the schedule of an activated
// tenancy runs to, and whether it is rolling.
func ScheduleThrough(t Tenancy, today string) (string, bool) {
	if t.EndedOn != "" {
		return t.EndedOn, false
	}
	if t.EndDate != "" && t.EndDate <= fmtDay(parseDay(today).AddDate(MaxScheduleYears, 0, 0)) {
		return t.EndDate, false
	}
	return RollingHorizon(today), true
}

var weekdays = []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// RuleText says the rule in words, such as "Monthly on day 31 (the last day
// in shorter months)" or "Weekly on Monday".
func RuleText(frequency string, dueDay int) string {
	if frequency == FrequencyWeekly {
		if dueDay >= 1 && dueDay <= 7 {
			return "Weekly on " + weekdays[dueDay-1]
		}
		return "Weekly"
	}
	out := fmt.Sprintf("%s on day %d", FrequencyLabel(frequency), dueDay)
	if dueDay >= 29 {
		out += " (the last day in shorter months)"
	}
	return out
}

// PeriodEnd is the last day of the period of the due with that sequence (1
// = the first): the day before the next due date, cut at the tenancy's end
// (never before the due date itself).
func PeriodEnd(t Tenancy, sequence int, dueDate string) string {
	next := parseDay(Occurrence(t.RentFrequency, t.RentDueDay, t.StartDate, sequence)).AddDate(0, 0, -1)
	out := fmtDay(next)
	if end := t.End(); end != "" && end < out {
		out = end
	}
	if out < dueDate {
		out = dueDate
	}
	return out
}

// planSchedule compares a tenancy's dues with its rule (as of today): the
// dues to add (missing due dates) and the dues to remove (after the end,
// with no allocation and no waiver). A DRAFT has no schedule.
func planSchedule(t Tenancy, existing []Due, today string, now time.Time) (add []Due, remove []uuid.UUID) {
	if t.Status == StatusDraft {
		return nil, nil
	}
	through, _ := ScheduleThrough(t, today)
	want := DueDates(t.RentFrequency, t.RentDueDay, t.StartDate, through)
	wanted := map[string]bool{}
	for _, d := range want {
		wanted[d] = true
	}
	have := map[string]bool{}
	for _, d := range existing {
		have[d.DueDate] = true
		if !wanted[d.DueDate] && len(d.Allocations) == 0 && d.WaivedAt == nil {
			remove = append(remove, d.ID)
		}
	}
	for i, d := range want {
		if have[d] {
			continue
		}
		add = append(add, Due{ID: newID(), TenancyID: t.ID, Sequence: i + 1, DueDate: d, Amount: t.RentAmount, Currency: t.RentCurrency,
			CreatedAt: now})
	}
	return add, remove
}
