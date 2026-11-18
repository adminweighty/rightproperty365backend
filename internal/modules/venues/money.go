package venues

import (
	"math/big"
	"regexp"
	"strings"
)

// Money is exact: every amount is a *big.Rat while it is worked on and a
// decimal string with exactly the currency's minor-unit decimals when it is
// stored or sent. A float never touches an amount.

// minorUnits are the ISO 4217 exponents that are not 2.
var minorUnits = map[string]int{
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0, "KRW": 0, "PYG": 0,
	"RWF": 0, "UGX": 0, "UYI": 0, "VND": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0,
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
}

// MinorUnit returns how many decimals an amount in the currency has (2 for
// most currencies, 0 for JPY, 3 for KWD).
func MinorUnit(currency string) int {
	if n, ok := minorUnits[strings.ToUpper(currency)]; ok {
		return n
	}
	return 2
}

var decimalRe = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,6})?$`)

// parseDecimal reads a non-negative decimal string with at most maxDecimals
// decimals. ok is false for anything else (signs, exponents, spaces, too
// many decimals).
func parseDecimal(raw string, maxDecimals int) (*big.Rat, bool) {
	if !decimalRe.MatchString(raw) {
		return nil, false
	}
	if i := strings.IndexByte(raw, '.'); i >= 0 && len(raw)-i-1 > maxDecimals {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(raw)
	return r, ok
}

// mustRat reads a stored decimal (already validated); "" and garbage are 0.
func mustRat(raw string) *big.Rat {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(raw))
	if !ok {
		return new(big.Rat)
	}
	return r
}

// roundHalfUp rounds r to decimals places, halves away from zero.
func roundHalfUp(r *big.Rat, decimals int) *big.Rat {
	if r.Sign() < 0 {
		neg := new(big.Rat).Neg(r)
		return new(big.Rat).Neg(roundHalfUp(neg, decimals))
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	// floor((num * scale * 2 + den) / (2 * den)) == floor(r*scale + 1/2)
	num := new(big.Int).Mul(r.Num(), scale)
	num.Mul(num, big.NewInt(2))
	num.Add(num, r.Denom())
	den := new(big.Int).Mul(r.Denom(), big.NewInt(2))
	q := new(big.Int).Quo(num, den)
	return new(big.Rat).SetFrac(q, scale)
}

// formatAmount writes r rounded half-up to the currency's minor unit.
func formatAmount(r *big.Rat, currency string) string {
	d := MinorUnit(currency)
	return roundHalfUp(r, d).FloatString(d)
}

// NormaliseAmount rewrites a stored or entered amount with exactly the
// currency's decimals ("50" -> "50.00"); it does not round past them.
func NormaliseAmount(raw, currency string) string { return formatAmount(mustRat(raw), currency) }

// trimDecimal writes r with up to four decimals and no trailing zeros
// ("2.5", "4", "0.25").
func trimDecimal(r *big.Rat) string {
	s := roundHalfUp(r, 4).FloatString(4)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
