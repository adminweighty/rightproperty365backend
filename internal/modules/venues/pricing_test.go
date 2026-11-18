package venues

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
)

func strp(s string) *string { return &s }

// The quote maths (FR-023): increments, fees with effective dates, PERCENT /
// FIXED / PER_HOUR, half-up rounding to the currency's minor unit, the
// deposit apart, and one currency only.
func TestPriceTable(t *testing.T) {
	harare := "Africa/Harare"
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tax := Fee{Code: "TAX", Label: "VAT 15%", Kind: KindPercent, Amount: "15", EffectiveFrom: "2026-01-01"}
	cases := []struct {
		name       string
		currency   string
		rate       string
		deposit    *string
		fees       []Fee
		start, end string
		zone       string
		lines      []Line
		subtotal   string
		total      string
		wantDep    *string
		wantErr    error
	}{
		{name: "four hours, no fees, a deposit", currency: "USD", rate: "50.00", deposit: strp("100"),
			start: "2026-12-05T08:00:00Z", end: "2026-12-05T12:00:00Z", zone: harare,
			lines:    []Line{{Code: "RATE", Kind: KindRate, Label: "4 h × USD 50.00", Amount: "200.00"}},
			subtotal: "200.00", total: "200.00", wantDep: strp("100.00")},
		{name: "a 30-minute increment and the tax", currency: "USD", rate: "50.00", fees: []Fee{tax},
			start: "2026-12-05T08:00:00Z", end: "2026-12-05T10:30:00Z", zone: harare,
			lines: []Line{{Code: "RATE", Kind: KindRate, Label: "2.5 h × USD 50.00", Amount: "125.00"},
				{Code: "TAX", Kind: KindPercent, Label: "VAT 15%", Amount: "18.75"}},
			subtotal: "125.00", total: "143.75"},
		{name: "a 15-minute increment", currency: "USD", rate: "40.00",
			start: "2026-12-05T08:00:00Z", end: "2026-12-05T09:15:00Z", zone: harare,
			lines:    []Line{{Code: "RATE", Kind: KindRate, Label: "1.25 h × USD 40.00", Amount: "50.00"}},
			subtotal: "50.00", total: "50.00"},
		{name: "fixed, per-hour and percent fees: the percent is of the subtotal", currency: "USD", rate: "50.00",
			fees: []Fee{tax, {Code: "CLEANING", Label: "Cleaning", Kind: KindFixed, Amount: "20.00", EffectiveFrom: "2026-01-01"},
				{Code: "SERVICE", Label: "Staff on site", Kind: KindPerHour, Amount: "2.50", EffectiveFrom: "2026-01-01"},
				{Code: "LEVY", Label: "Tourism levy 2%", Kind: KindPercent, Amount: "2", EffectiveFrom: "2026-01-01"}},
			start: "2026-12-05T08:00:00Z", end: "2026-12-05T11:30:00Z", zone: harare,
			lines: []Line{{Code: "RATE", Kind: KindRate, Label: "3.5 h × USD 50.00", Amount: "175.00"},
				{Code: "CLEANING", Kind: KindFixed, Label: "Cleaning", Amount: "20.00"},
				{Code: "SERVICE", Kind: KindPerHour, Label: "Staff on site", Amount: "8.75"},
				// 15% of 203.75 = 30.5625, 2% = 4.075: each rounded half-up on its own.
				{Code: "TAX", Kind: KindPercent, Label: "VAT 15%", Amount: "30.56"},
				{Code: "LEVY", Kind: KindPercent, Label: "Tourism levy 2%", Amount: "4.08"}},
			subtotal: "203.75", total: "238.39"},
		{name: "a fee outside its effective dates does not apply (venue-local date)", currency: "USD", rate: "50.00",
			fees: []Fee{{Code: "CLEANING", Label: "Cleaning", Kind: KindFixed, Amount: "20", EffectiveFrom: "2026-01-01", EffectiveTo: strp("2026-06-30")},
				{Code: "TAX", Label: "VAT 15%", Kind: KindPercent, Amount: "15", EffectiveFrom: "2026-07-01"}},
			// 22:30Z on 30 June is 00:30 on 1 July in Harare.
			start: "2026-06-30T22:30:00Z", end: "2026-07-01T00:30:00Z", zone: harare,
			lines: []Line{{Code: "RATE", Kind: KindRate, Label: "2 h × USD 50.00", Amount: "100.00"},
				{Code: "TAX", Kind: KindPercent, Label: "VAT 15%", Amount: "15.00"}},
			subtotal: "100.00", total: "115.00"},
		{name: "the last effective day still applies", currency: "USD", rate: "50.00",
			fees:  []Fee{{Code: "CLEANING", Label: "Cleaning", Kind: KindFixed, Amount: "20", EffectiveFrom: "2026-01-01", EffectiveTo: strp("2026-06-30")}},
			start: "2026-06-30T08:00:00Z", end: "2026-06-30T10:00:00Z", zone: harare,
			lines: []Line{{Code: "RATE", Kind: KindRate, Label: "2 h × USD 50.00", Amount: "100.00"},
				{Code: "CLEANING", Kind: KindFixed, Label: "Cleaning", Amount: "20.00"}},
			subtotal: "120.00", total: "120.00"},
		{name: "a fee not yet in force", currency: "USD", rate: "50.00", fees: []Fee{{Code: "TAX", Label: "VAT", Kind: KindPercent, Amount: "15", EffectiveFrom: "2027-01-01"}},
			start: "2026-12-05T08:00:00Z", end: "2026-12-05T10:00:00Z", zone: harare,
			lines:    []Line{{Code: "RATE", Kind: KindRate, Label: "2 h × USD 50.00", Amount: "100.00"}},
			subtotal: "100.00", total: "100.00"},
		{name: "rounding is half-up (5.025 -> 5.03), never banker's", currency: "USD", rate: "10.05",
			start: "2026-12-05T08:00:00Z", end: "2026-12-05T08:30:00Z", zone: harare,
			lines:    []Line{{Code: "RATE", Kind: KindRate, Label: "0.5 h × USD 10.05", Amount: "5.03"}},
			subtotal: "5.03", total: "5.03"},
		{name: "a zero-decimal currency (JPY)", currency: "JPY", rate: "1000", fees: []Fee{{Code: "TAX", Label: "Consumption tax 10%", Kind: KindPercent, Amount: "10", EffectiveFrom: "2026-01-01"}},
			start: "2026-12-05T01:00:00Z", end: "2026-12-05T02:15:00Z", zone: "Asia/Tokyo",
			lines: []Line{{Code: "RATE", Kind: KindRate, Label: "1.25 h × JPY 1000", Amount: "1250"},
				{Code: "TAX", Kind: KindPercent, Label: "Consumption tax 10%", Amount: "125"}},
			subtotal: "1250", total: "1375"},
		{name: "a three-decimal currency (KWD)", currency: "KWD", rate: "12.345", deposit: strp("5"),
			start: "2026-12-05T08:00:00Z", end: "2026-12-05T09:30:00Z", zone: "Asia/Kuwait",
			lines:    []Line{{Code: "RATE", Kind: KindRate, Label: "1.5 h × KWD 12.345", Amount: "18.518"}},
			subtotal: "18.518", total: "18.518", wantDep: strp("5.000")},
		{name: "one currency only", currency: "USD", rate: "50.00",
			fees:  []Fee{{Code: "CLEANING", Label: "Cleaning", Kind: KindFixed, Amount: "20", Currency: "EUR", EffectiveFrom: "2026-01-01"}},
			start: "2026-12-05T08:00:00Z", end: "2026-12-05T10:00:00Z", zone: harare, wantErr: ErrMixedCurrency},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Settings{Currency: c.currency, RatePerHour: c.rate, Deposit: c.deposit, Fees: c.fees}
			got, err := Price(s, at(c.start), at(c.end), c.zone)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Currency != c.currency || got.Subtotal != c.subtotal || got.TotalExcludingDeposit != c.total {
				t.Fatalf("%s: subtotal %s total %s", got.Currency, got.Subtotal, got.TotalExcludingDeposit)
			}
			if len(got.Lines) != len(c.lines) {
				t.Fatalf("lines %+v", got.Lines)
			}
			for i := range c.lines {
				if got.Lines[i] != c.lines[i] {
					t.Fatalf("line %d = %+v, want %+v", i, got.Lines[i], c.lines[i])
				}
			}
			if (got.Deposit == nil) != (c.wantDep == nil) || (got.Deposit != nil && *got.Deposit != *c.wantDep) {
				t.Fatalf("deposit = %v", got.Deposit)
			}
			// The lines always add up to the total.
			sum := new(big.Rat)
			for _, l := range got.Lines {
				sum.Add(sum, mustRat(l.Amount))
			}
			if formatAmount(sum, c.currency) != got.TotalExcludingDeposit {
				t.Fatalf("lines add up to %s, total %s", formatAmount(sum, c.currency), got.TotalExcludingDeposit)
			}
		})
	}
}

func TestRoundHalfUp(t *testing.T) {
	for raw, want := range map[string]string{"0.005": "0.01", "0.004": "0.00", "2.675": "2.68", "1.125": "1.13", "-1.125": "-1.13", "10": "10.00"} {
		if got := roundHalfUp(mustRat(raw), 2).FloatString(2); got != want {
			t.Fatalf("%s -> %s, want %s", raw, got, want)
		}
	}
	if MinorUnit("usd") != 2 || MinorUnit("JPY") != 0 || MinorUnit("BHD") != 3 {
		t.Fatal("minor units")
	}
}

// Settings validation: one currency (the market's), fees in it, amounts
// with the currency's decimals.
func TestSettingsValidation(t *testing.T) {
	i := func(v int) *int { return &v }
	b := func(v bool) *bool { return &v }
	base := func() SettingsInput {
		return SettingsInput{Enabled: b(true), Currency: strp("USD"), RatePerHour: strp("50"), IncrementMinutes: i(30), MinMinutes: i(120), MaxMinutes: i(480),
			CancellationPolicy: strp("Free up to 48 hours before."), OpeningHours: []OpeningRange{{Weekday: 1, Opens: "08:00", Closes: "18:00"}}}
	}
	s, err := base().apply(uuid.New(), []string{"USD"})
	if err != nil || s.RatePerHour != "50.00" || s.HorizonDays != DefaultHorizonDays || s.Capacity != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	cases := map[string]struct {
		mutate func(*SettingsInput)
		field  string
	}{
		"another country's currency": {func(in *SettingsInput) { in.Currency = strp("EUR") }, "currency"},
		"a fee in another currency": {func(in *SettingsInput) {
			in.Fees = []Fee{{Code: "TAX", Label: "VAT", Kind: KindPercent, Amount: "15", Currency: "EUR", EffectiveFrom: "2026-01-01"}}
		}, "fees[0].currency"},
		"too many decimals":              {func(in *SettingsInput) { in.RatePerHour = strp("50.001") }, "rate_per_hour"},
		"a float-looking exponent":       {func(in *SettingsInput) { in.RatePerHour = strp("5e1") }, "rate_per_hour"},
		"an increment of 20":             {func(in *SettingsInput) { in.IncrementMinutes = i(20) }, "increment_minutes"},
		"a minimum off the increment":    {func(in *SettingsInput) { in.MinMinutes = i(75) }, "min_minutes"},
		"max below min":                  {func(in *SettingsInput) { in.MaxMinutes = i(60) }, "max_minutes"},
		"a buffer over 240":              {func(in *SettingsInput) { in.CleaningBufferMinutes = i(241) }, "cleaning_buffer_minutes"},
		"a clock time off the increment": {func(in *SettingsInput) { in.OpeningHours[0].Opens = "08:15" }, "opening_hours[0].opens"},
		"a range past midnight":          {func(in *SettingsInput) { in.OpeningHours[0].Closes = "07:00" }, "opening_hours[0].closes"},
		"overlapping ranges": {func(in *SettingsInput) {
			in.OpeningHours = append(in.OpeningHours, OpeningRange{Weekday: 1, Opens: "17:00", Closes: "20:00"})
		}, "opening_hours[1]"},
		"an open exception without hours": {func(in *SettingsInput) { in.Exceptions = []Exception{{Date: "2026-12-24"}} }, "exceptions[0].opens"},
		"effective_to before from": {func(in *SettingsInput) {
			in.Fees = []Fee{{Code: "TAX", Label: "VAT", Kind: KindPercent, Amount: "15", EffectiveFrom: "2026-02-01", EffectiveTo: strp("2026-01-01")}}
		}, "fees[0].effective_to"},
		"overlapping periods of one fee": {func(in *SettingsInput) {
			in.Fees = []Fee{{Code: "TAX", Label: "VAT", Kind: KindPercent, Amount: "15", EffectiveFrom: "2026-01-01"},
				{Code: "TAX", Label: "VAT", Kind: KindPercent, Amount: "16", EffectiveFrom: "2026-06-01"}}
		}, "fees[1].effective_from"},
		"a percentage over 100": {func(in *SettingsInput) {
			in.Fees = []Fee{{Code: "TAX", Label: "VAT", Kind: KindPercent, Amount: "101", EffectiveFrom: "2026-01-01"}}
		}, "fees[0].amount"},
		"RATE as a fee code": {func(in *SettingsInput) {
			in.Fees = []Fee{{Code: "RATE", Label: "x", Kind: KindFixed, Amount: "1", EffectiveFrom: "2026-01-01"}}
		}, "fees[0].code"},
		"enabled without a policy": {func(in *SettingsInput) { in.CancellationPolicy = nil }, "cancellation_policy"},
	}
	for name, c := range cases {
		in := base()
		c.mutate(&in)
		_, err := in.apply(uuid.New(), []string{"USD"})
		var verr *ValidationError
		if !errors.As(err, &verr) || verr.Fields[c.field] == "" {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
