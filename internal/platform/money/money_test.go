package money

import (
	"math/big"
	"testing"
)

func TestMinorUnits(t *testing.T) {
	for cur, want := range map[string]int{"USD": 2, "ZAR": 2, "JPY": 0, "KWD": 3, "BHD": 3, "GBP": 2, "jpy": 0} {
		if got := MinorUnit(cur); got != want {
			t.Fatalf("%s: %d, want %d", cur, got, want)
		}
	}
}

func TestParseAmount(t *testing.T) {
	cases := []struct {
		raw, cur, want, problem string
	}{
		{"700", "USD", "700.00", ""},
		{"700.5", "USD", "700.50", ""},
		{"0.01", "USD", "0.01", ""},
		{"700.505", "USD", "", ProblemDecimals},
		{"5000", "JPY", "5000", ""},
		{"5000.0", "JPY", "", ProblemDecimals},
		{"1.5", "JPY", "", ProblemDecimals},
		{"1.250", "KWD", "1.250", ""},
		{"1.2345", "KWD", "", ProblemDecimals},
		{"0", "USD", "", ProblemZero},
		{"0.00", "USD", "", ProblemZero},
		{"-5", "USD", "", ProblemFormat},
		{"1e3", "USD", "", ProblemFormat},
		{"1,000", "USD", "", ProblemFormat},
		{"", "USD", "", ProblemFormat},
		{"12345678901234567", "USD", "", ProblemFormat},
	}
	for _, c := range cases {
		r, problem := ParseAmount(c.raw, c.cur)
		if problem != c.problem {
			t.Fatalf("%q %s: problem %q, want %q", c.raw, c.cur, problem, c.problem)
		}
		if c.problem == "" && Format(r, c.cur) != c.want {
			t.Fatalf("%q %s: %s, want %s", c.raw, c.cur, Format(r, c.cur), c.want)
		}
	}
}

func TestRoundHalfEven(t *testing.T) {
	for raw, want := range map[string]string{"0.125": "0.12", "0.135": "0.14", "-0.125": "-0.12", "-0.135": "-0.14", "2.5001": "2.50", "0.126": "0.13", "-0.126": "-0.13"} {
		r, _ := new(big.Rat).SetString(raw)
		if got := RoundHalfEven(r, 2).FloatString(2); got != want {
			t.Fatalf("%s: %s, want %s", raw, got, want)
		}
	}
	if Normalise("50", "KWD") != "50.000" || Normalise("50.0000", "JPY") != "50" {
		t.Fatal("normalise")
	}
}
