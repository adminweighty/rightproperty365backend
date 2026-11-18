package ledgertest

import "testing"

func TestPerCurrencyProblem(t *testing.T) {
	cases := map[string]struct {
		raw string
		bad bool
	}{
		"clean":          {`{"currencies":[{"currency":"USD","in":"100.00"},{"currency":"ZAR","in":"30000.00"}]}`, false},
		"usd plus zar":   {`{"currencies":[{"currency":"USD","in":"100.00"},{"currency":"ZAR","in":"30000.00"}],"total":{"currency":"USD","in":"30100.00"}}`, true},
		"no currency":    {`{"total":{"in":"1.00"}}`, true},
		"a float":        {`{"total":{"currency":"USD","in":1.5}}`, true},
		"nested no code": {`{"items":[{"entry":{"currency":"USD","amount":"1.00"},"running_balance":"1.00"}]}`, true},
	}
	for name, c := range cases {
		if err := PerCurrencyProblem([]byte(c.raw)); (err != nil) != c.bad {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
