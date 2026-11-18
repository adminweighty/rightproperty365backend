// Package ledgertest holds the FR-029 check shared by the ledger tests and
// the API tests: no report figure may add two currencies.
package ledgertest

import (
	"encoding/json"
	"fmt"
	"math/big"
)

// MoneyKeys are the JSON keys of the ledger API that hold an amount.
var MoneyKeys = []string{"amount", "signed_amount", "in", "out", "net", "opening_balance", "closing_balance", "running_balance",
	"outstanding", "settled", "receivable_outstanding", "payable_outstanding"}

// totalObjects walks decoded JSON and collects every object holding a money key.
func totalObjects(v any, out *[]map[string]any) {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range MoneyKeys {
			if _, ok := x[k]; ok {
				*out = append(*out, x)
				break
			}
		}
		for _, c := range x {
			totalObjects(c, out)
		}
	case []any:
		for _, c := range x {
			totalObjects(c, out)
		}
	}
}

// PerCurrencyProblem walks a JSON response and reports the first object
// that holds an amount without a three-letter currency, an amount that is
// not a decimal string, or a figure (anywhere in the response) equal to the
// sum of a USD figure and a ZAR figure of the same key: a silent
// cross-currency total. nil when the response is clean.
func PerCurrencyProblem(raw []byte) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	var objs []map[string]any
	totalObjects(doc, &objs)
	figures := map[string]map[string][]*big.Rat{} // currency -> key -> values
	var all []*big.Rat
	for _, o := range objs {
		cur, ok := o["currency"].(string)
		if !ok || len(cur) != 3 {
			return fmt.Errorf("a total object without a currency: %v", o)
		}
		for _, k := range MoneyKeys {
			v, present := o[k]
			if !present || v == nil {
				continue
			}
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("%s is not a decimal string: %v", k, v)
			}
			r, ok := new(big.Rat).SetString(s)
			if !ok {
				return fmt.Errorf("%s = %q is not a decimal", k, s)
			}
			if figures[cur] == nil {
				figures[cur] = map[string][]*big.Rat{}
			}
			figures[cur][k] = append(figures[cur][k], r)
			all = append(all, r)
		}
	}
	for k, usd := range figures["USD"] {
		for _, u := range usd {
			for _, z := range figures["ZAR"][k] {
				if u.Sign() == 0 || z.Sign() == 0 {
					continue
				}
				sum := new(big.Rat).Add(u, z)
				for _, v := range all {
					if v.Cmp(sum) == 0 {
						return fmt.Errorf("%s %s is USD %s + ZAR %s", k, v.FloatString(2), u.FloatString(2), z.FloatString(2))
					}
				}
			}
		}
	}
	return nil
}
