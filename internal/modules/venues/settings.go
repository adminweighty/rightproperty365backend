package venues

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// SettingsInput is the body of PUT /v1/me/listings/{id}/hourly (a full
// replace; optional fields fall back to their defaults).
type SettingsInput struct {
	Enabled               *bool          `json:"enabled"`
	Currency              *string        `json:"currency"`
	RatePerHour           *string        `json:"rate_per_hour"`
	IncrementMinutes      *int           `json:"increment_minutes"`
	MinMinutes            *int           `json:"min_minutes"`
	MaxMinutes            *int           `json:"max_minutes"`
	CleaningBufferMinutes *int           `json:"cleaning_buffer_minutes"`
	Capacity              *int           `json:"capacity"`
	Deposit               *string        `json:"deposit"`
	OpeningHours          []OpeningRange `json:"opening_hours"`
	Exceptions            []Exception    `json:"exceptions"`
	Fees                  []Fee          `json:"fees"`
	CancellationPolicy    *string        `json:"cancellation_policy"`
	FreeCancellationHours *int           `json:"free_cancellation_hours"`
	LeadTimeHours         *int           `json:"lead_time_hours"`
	HorizonDays           *int           `json:"horizon_days"`
}

// Limits of the settings.
const (
	MaxCleaningBuffer   = 240
	MaxCapacity         = 50
	MaxRangesPerDay     = 6
	MaxExceptions       = 366
	MaxFees             = 20
	MaxPolicyLength     = 2000
	MaxFeeLabelLength   = 80
	MaxNoteLength       = 120
	MaxLeadTimeHours    = 720
	MaxHorizonDays      = 730
	MaxFreeCancelHours  = 720
	DefaultHorizonDays  = 90
	clockGranularity    = 15
	maxDurationMinutes  = 24 * 60
	percentDecimals     = 3
	minIncrementMinutes = 15
)

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	feeCodeRe  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,31}$`)
	htmlTagRe  = regexp.MustCompile(`<[/!?]?[A-Za-z][^>]*>|<!--`)
)

// plainText validates free text: trimmed, at most max characters, no
// control characters (line breaks allowed when multiline), no HTML.
func plainText(raw string, max int, multiline bool) (string, string) {
	v := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	switch {
	case utf8.RuneCountInString(v) > max:
		return v, fmt.Sprintf("must be at most %d characters", max)
	case !utf8.ValidString(v):
		return v, "contains characters that are not allowed"
	case htmlTagRe.MatchString(v):
		return v, "must be plain text without HTML"
	}
	for _, r := range v {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t')) {
			return v, "contains characters that are not allowed"
		}
	}
	return v, ""
}

func validDate(s string) bool {
	_, err := time.Parse(dateLayout, s)
	return err == nil
}

// apply validates the input against the market's currencies and returns
// the settings it describes.
func (in SettingsInput) apply(listingID uuid.UUID, currencies []string) (Settings, error) {
	f := map[string]string{}
	s := DefaultSettings(listingID, "")
	s.LeadTimeHours, s.FreeCancellationHours, s.HorizonDays = 0, 0, DefaultHorizonDays
	if in.Enabled != nil {
		s.Enabled = *in.Enabled
	}
	switch {
	case in.Currency == nil || strings.TrimSpace(*in.Currency) == "":
		f["currency"] = "is required"
	case !currencyRe.MatchString(strings.ToUpper(strings.TrimSpace(*in.Currency))):
		f["currency"] = "must be an ISO 4217 code such as USD"
	default:
		s.Currency = strings.ToUpper(strings.TrimSpace(*in.Currency))
		if !slices.Contains(currencies, s.Currency) {
			f["currency"] = "must be the currency of the listing's country: " + strings.Join(currencies, ", ")
		}
	}
	decimals := MinorUnit(s.Currency)
	money := func(path string, raw string, positive bool) (string, bool) {
		r, ok := parseDecimal(strings.TrimSpace(raw), decimals)
		switch {
		case !ok:
			f[path] = fmt.Sprintf("must be a decimal amount as a string with at most %d decimals, for example \"50.00\"", decimals)
			return "", false
		case positive && r.Sign() <= 0:
			f[path] = "must be more than zero"
			return "", false
		}
		return formatAmount(r, s.Currency), true
	}
	if in.RatePerHour == nil {
		f["rate_per_hour"] = "is required"
	} else if v, ok := money("rate_per_hour", *in.RatePerHour, true); ok {
		s.RatePerHour = v
	}
	if in.Deposit != nil {
		if v, ok := money("deposit", *in.Deposit, false); ok {
			s.Deposit = &v
		}
	}

	ints := func(path string, v *int, dst *int, lo, hi int, required bool) {
		if v == nil {
			if required {
				f[path] = "is required"
			}
			return
		}
		if *v < lo || *v > hi {
			f[path] = fmt.Sprintf("must be between %d and %d", lo, hi)
			return
		}
		*dst = *v
	}
	if in.IncrementMinutes == nil {
		f["increment_minutes"] = "is required"
	} else if !slices.Contains([]int{15, 30, 60}, *in.IncrementMinutes) {
		f["increment_minutes"] = "must be 15, 30 or 60"
	} else {
		s.IncrementMinutes = *in.IncrementMinutes
	}
	ints("min_minutes", in.MinMinutes, &s.MinMinutes, minIncrementMinutes, maxDurationMinutes, true)
	ints("max_minutes", in.MaxMinutes, &s.MaxMinutes, minIncrementMinutes, maxDurationMinutes, true)
	if f["increment_minutes"] == "" && in.IncrementMinutes != nil {
		for path, v := range map[string]*int{"min_minutes": in.MinMinutes, "max_minutes": in.MaxMinutes} {
			if v != nil && f[path] == "" && *v%s.IncrementMinutes != 0 {
				f[path] = fmt.Sprintf("must be a multiple of the increment (%d minutes)", s.IncrementMinutes)
			}
		}
	}
	if f["min_minutes"] == "" && f["max_minutes"] == "" && s.MaxMinutes < s.MinMinutes {
		f["max_minutes"] = "must be at least min_minutes"
	}
	ints("cleaning_buffer_minutes", in.CleaningBufferMinutes, &s.CleaningBufferMinutes, 0, MaxCleaningBuffer, false)
	ints("capacity", in.Capacity, &s.Capacity, 1, MaxCapacity, false)
	ints("free_cancellation_hours", in.FreeCancellationHours, &s.FreeCancellationHours, 0, MaxFreeCancelHours, false)
	ints("lead_time_hours", in.LeadTimeHours, &s.LeadTimeHours, 0, MaxLeadTimeHours, false)
	ints("horizon_days", in.HorizonDays, &s.HorizonDays, 1, MaxHorizonDays, false)

	if in.CancellationPolicy != nil {
		v, msg := plainText(*in.CancellationPolicy, MaxPolicyLength, true)
		if msg != "" {
			f["cancellation_policy"] = msg
		}
		s.CancellationPolicy = v
	}
	if s.Enabled && s.CancellationPolicy == "" && f["cancellation_policy"] == "" {
		f["cancellation_policy"] = "is required to take bookings"
	}

	// Clock times: on the increment (15 minutes when the increment is wrong).
	step := s.IncrementMinutes
	if step == 0 {
		step = clockGranularity
	}
	clock := func(path, raw string) (int, string, bool) {
		v := strings.TrimSpace(raw)
		m, ok := clockMinutes(v)
		switch {
		case !ok:
			f[path] = "must be a time HH:MM between 00:00 and 24:00"
			return 0, "", false
		case m%step != 0:
			f[path] = fmt.Sprintf("must be on a %d-minute boundary", step)
			return 0, "", false
		}
		return m, v, true
	}

	s.OpeningHours = []OpeningRange{}
	perDay := map[int][][2]int{}
	for i, r := range in.OpeningHours {
		p := fmt.Sprintf("opening_hours[%d]", i)
		if r.Weekday < 1 || r.Weekday > 7 {
			f[p+".weekday"] = "must be 1 (Monday) to 7 (Sunday)"
			continue
		}
		a, opens, ok1 := clock(p+".opens", r.Opens)
		b, closes, ok2 := clock(p+".closes", r.Closes)
		if !ok1 || !ok2 {
			continue
		}
		if a >= b || a == 24*60 {
			f[p+".closes"] = "must be after opens (a range cannot pass midnight; add the next day's range)"
			continue
		}
		for _, o := range perDay[r.Weekday] {
			if a < o[1] && o[0] < b {
				f[p] = "overlaps another range of the same day"
			}
		}
		perDay[r.Weekday] = append(perDay[r.Weekday], [2]int{a, b})
		if len(perDay[r.Weekday]) > MaxRangesPerDay {
			f[p] = fmt.Sprintf("at most %d ranges per day", MaxRangesPerDay)
		}
		s.OpeningHours = append(s.OpeningHours, OpeningRange{Weekday: r.Weekday, Opens: opens, Closes: closes})
	}
	sort.SliceStable(s.OpeningHours, func(i, j int) bool {
		if s.OpeningHours[i].Weekday != s.OpeningHours[j].Weekday {
			return s.OpeningHours[i].Weekday < s.OpeningHours[j].Weekday
		}
		return s.OpeningHours[i].Opens < s.OpeningHours[j].Opens
	})
	if s.Enabled && len(s.OpeningHours) == 0 && len(in.OpeningHours) == 0 {
		f["opening_hours"] = "give at least one opening range to take bookings"
	}

	s.Exceptions = []Exception{}
	if len(in.Exceptions) > MaxExceptions {
		f["exceptions"] = fmt.Sprintf("at most %d exceptions", MaxExceptions)
	}
	seen := map[string]bool{}
	for i, e := range in.Exceptions {
		p := fmt.Sprintf("exceptions[%d]", i)
		e.Date = strings.TrimSpace(e.Date)
		switch {
		case !validDate(e.Date):
			f[p+".date"] = "must be a date YYYY-MM-DD"
			continue
		case seen[e.Date]:
			f[p+".date"] = "has another exception already"
			continue
		}
		seen[e.Date] = true
		out := Exception{Date: e.Date, Closed: e.Closed}
		if e.Note != nil {
			v, msg := plainText(*e.Note, MaxNoteLength, false)
			if msg != "" {
				f[p+".note"] = msg
			}
			if v != "" {
				out.Note = &v
			}
		}
		if !e.Closed {
			if e.Opens == nil || e.Closes == nil {
				f[p+".opens"] = "opens and closes are required unless closed is true"
				continue
			}
			a, opens, ok1 := clock(p+".opens", *e.Opens)
			b, closes, ok2 := clock(p+".closes", *e.Closes)
			if !ok1 || !ok2 {
				continue
			}
			if a >= b {
				f[p+".closes"] = "must be after opens"
				continue
			}
			out.Opens, out.Closes = &opens, &closes
		}
		s.Exceptions = append(s.Exceptions, out)
	}
	sort.SliceStable(s.Exceptions, func(i, j int) bool { return s.Exceptions[i].Date < s.Exceptions[j].Date })

	s.Fees = []Fee{}
	if len(in.Fees) > MaxFees {
		f["fees"] = fmt.Sprintf("at most %d fees", MaxFees)
	}
	for i, fee := range in.Fees {
		p := fmt.Sprintf("fees[%d]", i)
		out := Fee{Code: strings.ToUpper(strings.TrimSpace(fee.Code)), Kind: strings.TrimSpace(fee.Kind),
			EffectiveFrom: strings.TrimSpace(fee.EffectiveFrom)}
		switch {
		case !feeCodeRe.MatchString(out.Code):
			f[p+".code"] = "must be 2 to 32 capital letters, digits or underscores, starting with a letter (TAX for a tax)"
		case out.Code == KindRate:
			f[p+".code"] = "RATE is the hourly rate's own line"
		}
		label, msg := plainText(fee.Label, MaxFeeLabelLength, false)
		if msg == "" && label == "" {
			msg = "is required"
		}
		if msg != "" {
			f[p+".label"] = msg
		}
		out.Label = label
		if c := strings.ToUpper(strings.TrimSpace(fee.Currency)); c != "" && s.Currency != "" && c != s.Currency {
			f[p+".currency"] = "must be " + s.Currency + ": a quote never mixes currencies"
		}
		switch out.Kind {
		case KindPercent:
			r, ok := parseDecimal(strings.TrimSpace(fee.Amount), percentDecimals)
			if !ok || r.Cmp(mustRat("100")) > 0 {
				f[p+".amount"] = "must be a percentage from 0 to 100 as a string, for example \"15\""
			} else {
				out.Amount = trimDecimal(r)
			}
		case KindFixed, KindPerHour:
			if v, ok := money(p+".amount", fee.Amount, false); ok {
				out.Amount = v
			}
		default:
			f[p+".kind"] = "must be PERCENT, FIXED or PER_HOUR"
		}
		if !validDate(out.EffectiveFrom) {
			f[p+".effective_from"] = "must be a date YYYY-MM-DD"
		}
		if fee.EffectiveTo != nil {
			to := strings.TrimSpace(*fee.EffectiveTo)
			switch {
			case !validDate(to):
				f[p+".effective_to"] = "must be a date YYYY-MM-DD or null"
			case validDate(out.EffectiveFrom) && to < out.EffectiveFrom:
				f[p+".effective_to"] = "must be on or after effective_from"
			default:
				out.EffectiveTo = &to
			}
		}
		s.Fees = append(s.Fees, out)
	}
	// One fee per code at a time: periods of the same code must not overlap.
	for i, a := range s.Fees {
		for j := i + 1; j < len(s.Fees); j++ {
			b := s.Fees[j]
			if a.Code == b.Code && a.EffectiveFrom != "" && b.EffectiveFrom != "" && periodsOverlap(a, b) {
				f[fmt.Sprintf("fees[%d].effective_from", j)] = "overlaps another period of fee " + a.Code
			}
		}
	}
	if len(f) > 0 {
		return Settings{}, &ValidationError{Fields: f}
	}
	return s, nil
}

func periodsOverlap(a, b Fee) bool {
	aEnd, bEnd := "9999-12-31", "9999-12-31"
	if a.EffectiveTo != nil {
		aEnd = *a.EffectiveTo
	}
	if b.EffectiveTo != nil {
		bEnd = *b.EffectiveTo
	}
	return a.EffectiveFrom <= bEnd && b.EffectiveFrom <= aEnd
}
