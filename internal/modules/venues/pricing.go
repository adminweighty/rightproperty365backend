package venues

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Price works out the quote of the slot [start, end) under the settings:
//
//   - RATE: rate_per_hour x the real elapsed hours;
//   - then the FIXED fees (once) and the PER_HOUR fees (x hours);
//   - subtotal = the lines so far;
//   - then the PERCENT fees, each a percentage of the subtotal (a tax is a
//     fee whose code is TAX);
//   - total_excluding_deposit = every line; the deposit is apart.
//
// A fee applies when its effective dates (inclusive) cover the venue-local
// date of the start. Every line is rounded half-up to the currency's minor
// unit on its own, and the sums add the rounded lines, so the lines always
// add up to the totals shown. One currency only: a fee that names another
// currency is ErrMixedCurrency.
func Price(s Settings, start, end time.Time, zone string) (Pricing, error) {
	cur := strings.ToUpper(s.Currency)
	minutes := int64(end.Sub(start) / time.Minute)
	hours := new(big.Rat).SetFrac64(minutes, 60)
	day := start.In(location(zone)).Format(dateLayout)
	rate := mustRat(s.RatePerHour)

	out := Pricing{Currency: cur, Lines: []Line{}}
	subtotal := new(big.Rat)
	add := func(code, kind, label string, amount *big.Rat) *big.Rat {
		rounded := roundHalfUp(amount, MinorUnit(cur))
		out.Lines = append(out.Lines, Line{Code: code, Kind: kind, Label: label, Amount: formatAmount(rounded, cur)})
		return rounded
	}
	rateLabel := fmt.Sprintf("%s h × %s %s", trimDecimal(hours), cur, NormaliseAmount(s.RatePerHour, cur))
	subtotal.Add(subtotal, add(KindRate, KindRate, rateLabel, new(big.Rat).Mul(rate, hours)))

	var percents []Fee
	for _, f := range s.Fees {
		if f.Currency != "" && !strings.EqualFold(f.Currency, cur) {
			return Pricing{}, ErrMixedCurrency
		}
		if !f.appliesOn(day) {
			continue
		}
		switch f.Kind {
		case KindFixed:
			subtotal.Add(subtotal, add(f.Code, f.Kind, f.Label, mustRat(f.Amount)))
		case KindPerHour:
			subtotal.Add(subtotal, add(f.Code, f.Kind, f.Label, new(big.Rat).Mul(mustRat(f.Amount), hours)))
		case KindPercent:
			percents = append(percents, f)
		}
	}
	total := new(big.Rat).Set(subtotal)
	for _, f := range percents {
		pct := new(big.Rat).Quo(mustRat(f.Amount), big.NewRat(100, 1))
		total.Add(total, add(f.Code, f.Kind, f.Label, new(big.Rat).Mul(subtotal, pct)))
	}
	out.Subtotal, out.TotalExcludingDeposit = formatAmount(subtotal, cur), formatAmount(total, cur)
	if s.Deposit != nil {
		d := NormaliseAmount(*s.Deposit, cur)
		out.Deposit = &d
	}
	return out, nil
}

// appliesOn reports whether the fee is in force on the local date day.
func (f Fee) appliesOn(day string) bool {
	if f.EffectiveFrom != "" && day < f.EffectiveFrom {
		return false
	}
	return f.EffectiveTo == nil || day <= *f.EffectiveTo
}
