// Package money holds exact decimal amounts and the ISO 4217 minor units.
//
// An amount is a *big.Rat while it is worked on and a decimal string with
// exactly the currency's minor-unit decimals when it is stored or sent
// ("700.00" USD, "5000" JPY, "1.250" KWD). A float never touches an amount,
// and nothing here ever adds two currencies.
package money

import (
	"math/big"
	"regexp"
	"strings"
)

// minorUnits are the ISO 4217 exponents that are not 2 (ISO 4217 list one,
// for the currencies of the countries in reference data).
var minorUnits = map[string]int{
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0, "KRW": 0, "PYG": 0,
	"RWF": 0, "UGX": 0, "UYI": 0, "VND": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0,
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
	"CLF": 4, "UYW": 4,
}

// MinorUnit returns how many decimals an amount in the currency has (2 for
// most currencies, 0 for JPY, 3 for KWD).
func MinorUnit(currency string) int {
	if n, ok := minorUnits[strings.ToUpper(strings.TrimSpace(currency))]; ok {
		return n
	}
	return 2
}

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// ValidCurrencyCode reports whether s has the shape of an ISO 4217 code.
func ValidCurrencyCode(s string) bool { return currencyRe.MatchString(s) }

var decimalRe = regexp.MustCompile(`^[0-9]{1,16}(\.[0-9]{1,8})?$`)

// Problems ParseAmount reports.
const (
	ProblemFormat   = "format"
	ProblemDecimals = "decimals"
	ProblemZero     = "zero"
)

// ParseAmount reads a positive decimal string ("700", "700.5", "700.50")
// with at most the currency's minor-unit decimals. Signs, exponents, spaces
// and commas are refused. problem is "" when ok, otherwise one of the
// Problem constants.
func ParseAmount(raw, currency string) (r *big.Rat, problem string) {
	raw = strings.TrimSpace(raw)
	if !decimalRe.MatchString(raw) {
		return nil, ProblemFormat
	}
	if i := strings.IndexByte(raw, '.'); i >= 0 && len(raw)-i-1 > MinorUnit(currency) {
		return nil, ProblemDecimals
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok {
		return nil, ProblemFormat
	}
	if r.Sign() <= 0 {
		return nil, ProblemZero
	}
	return r, ""
}

// Rat reads a stored decimal (already validated; "" and garbage are 0).
func Rat(raw string) *big.Rat {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(raw))
	if !ok {
		return new(big.Rat)
	}
	return r
}

// RoundHalfEven rounds r to decimals places, ties to the even neighbour.
func RoundHalfEven(r *big.Rat, decimals int) *big.Rat {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	num := new(big.Int).Mul(r.Num(), scale)
	den := r.Denom()
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	// q truncates toward zero; m has the sign of num.
	twice := new(big.Int).Mul(new(big.Int).Abs(m), big.NewInt(2))
	switch twice.Cmp(den) {
	case 1:
		q.Add(q, big.NewInt(int64(num.Sign())))
	case 0:
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(int64(num.Sign())))
		}
	}
	return new(big.Rat).SetFrac(q, scale)
}

// Format writes r with exactly the currency's minor-unit decimals, rounded
// half-even (amounts entered are exact, so this only ever pads).
func Format(r *big.Rat, currency string) string {
	d := MinorUnit(currency)
	return RoundHalfEven(r, d).FloatString(d)
}

// Normalise rewrites a stored or entered amount with exactly the currency's
// decimals ("50" -> "50.00").
func Normalise(raw, currency string) string { return Format(Rat(raw), currency) }
