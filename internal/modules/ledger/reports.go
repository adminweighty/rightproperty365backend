package ledger

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/money"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Reports (FR-029). Every figure is per currency: a total object always
// carries its currency, and no code path adds amounts of two currencies
// (each accumulator is keyed by currency before anything is summed). A
// consolidated conversion is not built (decision D-05).
//
// A REVERSAL counts against the bucket of the entry it reverses (a reversal
// of rent received lowers "in"; it does not raise "out"), so opening + in -
// out = closing always holds.

// ReportQuery is the query of a report. Dates are "YYYY-MM-DD".
type ReportQuery struct {
	From, To  string
	Currency  string
	Branch    *string
	By        string
	ContactID *uuid.UUID
	AsOf      string
	Side      string
}

// Report names (export.csv?report=).
const (
	ReportCashbook   = "cashbook"
	ReportEntries    = "entries"
	ReportCategories = "categories"
	ReportStatement  = "statement"
)

// Ageing buckets.
var bucketNames = []string{"0-30", "31-60", "61-90", "90+"}

func bucketOf(days int) string {
	switch {
	case days <= 30:
		return "0-30"
	case days <= 60:
		return "31-60"
	case days <= 90:
		return "61-90"
	}
	return "90+"
}

// Totals is the per-currency total of a set of entries.
type Totals struct {
	Currency   string `json:"currency"`
	MinorUnit  int    `json:"minor_unit"`
	In         string `json:"in"`
	Out        string `json:"out"`
	Net        string `json:"net"`
	EntryCount int    `json:"entry_count"`
}

// CashLine is one entry of a cashbook or a statement.
type CashLine struct {
	Entry          EntryView `json:"entry"`
	Currency       string    `json:"currency"`
	MinorUnit      int       `json:"minor_unit"`
	In             string    `json:"in"`
	Out            string    `json:"out"`
	RunningBalance string    `json:"running_balance"`
}

// CashGroup is a cashbook's totals for one method or branch, in one currency.
type CashGroup struct {
	Currency       string  `json:"currency"`
	MinorUnit      int     `json:"minor_unit"`
	Key            *string `json:"key"`
	Label          string  `json:"label"`
	OpeningBalance string  `json:"opening_balance"`
	In             string  `json:"in"`
	Out            string  `json:"out"`
	Net            string  `json:"net"`
	ClosingBalance string  `json:"closing_balance"`
	EntryCount     int     `json:"entry_count"`
}

// CashCurrency is a cashbook in one currency.
type CashCurrency struct {
	Currency       string      `json:"currency"`
	MinorUnit      int         `json:"minor_unit"`
	OpeningBalance string      `json:"opening_balance"`
	In             string      `json:"in"`
	Out            string      `json:"out"`
	Net            string      `json:"net"`
	ClosingBalance string      `json:"closing_balance"`
	EntryCount     int         `json:"entry_count"`
	Groups         []CashGroup `json:"groups"`
	Entries        []CashLine  `json:"entries"`
}

// Cashbook is GET .../reports/cashbook.
type Cashbook struct {
	From       string         `json:"from"`
	To         string         `json:"to"`
	By         *string        `json:"by"`
	Currencies []CashCurrency `json:"currencies"`
}

// CategoryTotals are one category's totals in one currency.
type CategoryTotals struct {
	Currency   string  `json:"currency"`
	MinorUnit  int     `json:"minor_unit"`
	Category   *string `json:"category"`
	Label      string  `json:"label"`
	In         string  `json:"in"`
	Out        string  `json:"out"`
	Net        string  `json:"net"`
	EntryCount int     `json:"entry_count"`
}

// CategoryCurrency is the categories report in one currency.
type CategoryCurrency struct {
	Currency   string           `json:"currency"`
	MinorUnit  int              `json:"minor_unit"`
	Categories []CategoryTotals `json:"categories"`
	Total      Totals           `json:"total"`
}

// CategoriesReport is GET .../reports/categories.
type CategoriesReport struct {
	From       string             `json:"from"`
	To         string             `json:"to"`
	Currencies []CategoryCurrency `json:"currencies"`
}

// StatementCurrency is a contact statement in one currency.
type StatementCurrency struct {
	Currency              string     `json:"currency"`
	MinorUnit             int        `json:"minor_unit"`
	OpeningBalance        string     `json:"opening_balance"`
	In                    string     `json:"in"`
	Out                   string     `json:"out"`
	Net                   string     `json:"net"`
	ClosingBalance        string     `json:"closing_balance"`
	EntryCount            int        `json:"entry_count"`
	ReceivableOutstanding string     `json:"receivable_outstanding"`
	PayableOutstanding    string     `json:"payable_outstanding"`
	Entries               []CashLine `json:"entries"`
}

// Statement is GET .../reports/statement.
type Statement struct {
	From       string              `json:"from"`
	To         string              `json:"to"`
	Contact    ContactRef          `json:"contact"`
	Currencies []StatementCurrency `json:"currencies"`
}

// Bucket is one ageing bucket in one currency.
type Bucket struct {
	Currency    string `json:"currency"`
	MinorUnit   int    `json:"minor_unit"`
	Bucket      string `json:"bucket"`
	Outstanding string `json:"outstanding"`
	Count       int    `json:"count"`
}

// AgedItem is one open receivable (or payable).
type AgedItem struct {
	Entry       EntryView `json:"entry"`
	Currency    string    `json:"currency"`
	MinorUnit   int       `json:"minor_unit"`
	Amount      string    `json:"amount"`
	Settled     string    `json:"settled"`
	Outstanding string    `json:"outstanding"`
	DaysOverdue int       `json:"days_overdue"`
	Bucket      string    `json:"bucket"`
}

// AgedCurrency is the ageing in one currency. For receivables it includes
// the book's rent arrears (API 0.36.1): Outstanding, Count and Buckets add
// them, RentOutstanding is their part and RentItems lists them.
type AgedCurrency struct {
	Currency        string         `json:"currency"`
	MinorUnit       int            `json:"minor_unit"`
	Outstanding     string         `json:"outstanding"`
	Count           int            `json:"count"`
	Buckets         []Bucket       `json:"buckets"`
	Items           []AgedItem     `json:"items"`
	RentOutstanding string         `json:"rent_outstanding"`
	RentItems       []RentAgedItem `json:"rent_items"`
}

// Receivables is GET .../reports/receivables.
type Receivables struct {
	AsOf       string         `json:"as_of"`
	Side       string         `json:"side"`
	Currencies []AgedCurrency `json:"currencies"`
}

// ---------------------------------------------------------------------------
// Arithmetic (per currency only)
// ---------------------------------------------------------------------------

// bucket returns the column an entry counts in and its (signed) value there.
func contribution(e Entry) (string, *big.Rat) {
	v := money.Rat(e.Amount)
	if e.Type == TypeReversal {
		col := In
		if e.Direction == In {
			col = Out
		}
		return col, v.Neg(v)
	}
	return e.Direction, v
}

// sums accumulates the in and out of ONE currency.
type sums struct {
	currency string
	in, out  *big.Rat
	count    int
}

func newSums(currency string) *sums {
	return &sums{currency: currency, in: new(big.Rat), out: new(big.Rat)}
}

func (s *sums) add(e Entry) (col string, v *big.Rat) {
	if e.Currency != s.currency {
		panic("ledger: an entry of " + e.Currency + " added to a " + s.currency + " total")
	}
	col, v = contribution(e)
	if col == In {
		s.in.Add(s.in, v)
	} else {
		s.out.Add(s.out, v)
	}
	s.count++
	return col, v
}

func (s *sums) net() *big.Rat { return new(big.Rat).Sub(s.in, s.out) }

func (s *sums) f(r *big.Rat) string { return money.Format(r, s.currency) }

func (s *sums) totals() Totals {
	return Totals{Currency: s.currency, MinorUnit: money.MinorUnit(s.currency), In: s.f(s.in), Out: s.f(s.out), Net: s.f(s.net()), EntryCount: s.count}
}

// byCurrency splits entries per currency, sorted by code.
func byCurrency(list []Entry) ([]string, map[string][]Entry) {
	m := map[string][]Entry{}
	var codes []string
	for _, e := range list {
		if _, ok := m[e.Currency]; !ok {
			codes = append(codes, e.Currency)
		}
		m[e.Currency] = append(m[e.Currency], e)
	}
	slices.Sort(codes)
	return codes, m
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// period defaults and checks a report's range: to defaults to today (the
// book's market date), from to the first of to's month.
func (s *Service) period(q *ReportQuery, f map[string]string, today string) {
	if q.To == "" {
		q.To = today
	} else if !validDate(q.To) {
		f["to"] = "must be a date (YYYY-MM-DD)"
	}
	if q.From == "" {
		t, err := time.Parse(time.DateOnly, q.To)
		if err != nil {
			t, _ = time.Parse(time.DateOnly, today)
		}
		q.From = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	} else if !validDate(q.From) {
		f["from"] = "must be a date (YYYY-MM-DD)"
	}
	if f["from"] == "" && f["to"] == "" && q.From > q.To {
		f["from"] = "must be on or before to"
	}
}

func (s *Service) checkCurrency(ctx context.Context, q *ReportQuery, f map[string]string) error {
	if q.Currency == "" {
		return nil
	}
	q.Currency = strings.ToUpper(strings.TrimSpace(q.Currency))
	curs, err := s.allowedCurrencies(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(curs, q.Currency) {
		f["currency"] = "must be an ISO 4217 currency code from reference data"
	}
	return nil
}

func (s *Service) all(ctx context.Context, book Book, f Filter) ([]Entry, error) {
	f.Ascending, f.Limit, f.Offset = true, 0, 0
	list, _, err := s.store.Entries(ctx, book, f)
	return list, err
}

func cashOnly(list []Entry) []Entry {
	out := make([]Entry, 0, len(list))
	for _, e := range list {
		if e.Cash() {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Cashbook
// ---------------------------------------------------------------------------

// Cashbook serves GET .../reports/cashbook.
func (s *Service) Cashbook(ctx context.Context, p *auth.Principal, bookID uuid.UUID, q ReportQuery) (Cashbook, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Cashbook{}, err
	}
	return s.cashbook(ctx, a, q)
}

func (s *Service) cashbook(ctx context.Context, a access, q ReportQuery) (Cashbook, error) {
	f := map[string]string{}
	s.period(&q, f, s.bookToday(ctx, a))
	if err := s.checkCurrency(ctx, &q, f); err != nil {
		return Cashbook{}, err
	}
	if q.By != "" && q.By != "method" && q.By != "branch" {
		f["by"] = "must be method or branch"
	}
	if len(f) > 0 {
		return Cashbook{}, &ValidationError{Fields: f}
	}
	list, err := s.all(ctx, a.book, Filter{Currency: q.Currency, Branch: q.Branch, To: q.To})
	if err != nil {
		return Cashbook{}, err
	}
	v, err := s.viewer(ctx, a)
	if err != nil {
		return Cashbook{}, err
	}
	out := Cashbook{From: q.From, To: q.To, By: strPtr(q.By), Currencies: []CashCurrency{}}
	codes, per := byCurrency(cashOnly(list))
	for _, cur := range codes {
		cc, err := cashCurrency(ctx, v, cur, per[cur], q)
		if err != nil {
			return Cashbook{}, err
		}
		out.Currencies = append(out.Currencies, cc)
	}
	return out, nil
}

// cashLines splits one currency's entries (ascending) at from: the opening
// sums, the period sums and the period's lines with a running balance.
func cashLines(ctx context.Context, v *viewer, cur string, list []Entry, from string) (opening, period *sums, lines []CashLine, err error) {
	opening, period = newSums(cur), newSums(cur)
	var running *big.Rat
	lines = []CashLine{}
	for _, e := range list {
		if e.EffectiveDate < from {
			opening.add(e)
			continue
		}
		if running == nil {
			running = opening.net()
		}
		col, val := period.add(e)
		in, outv := new(big.Rat), new(big.Rat)
		if col == In {
			in = val
			running = new(big.Rat).Add(running, val)
		} else {
			outv = val
			running = new(big.Rat).Sub(running, val)
		}
		ev, verr := v.view(ctx, e)
		if verr != nil {
			return nil, nil, nil, verr
		}
		lines = append(lines, CashLine{Entry: ev, Currency: cur, MinorUnit: money.MinorUnit(cur), In: money.Format(in, cur), Out: money.Format(outv, cur), RunningBalance: money.Format(running, cur)})
	}
	return opening, period, lines, nil
}

func cashCurrency(ctx context.Context, v *viewer, cur string, list []Entry, q ReportQuery) (CashCurrency, error) {
	opening, period, lines, err := cashLines(ctx, v, cur, list, q.From)
	if err != nil {
		return CashCurrency{}, err
	}
	closing := new(big.Rat).Add(opening.net(), period.net())
	cc := CashCurrency{Currency: cur, MinorUnit: money.MinorUnit(cur), OpeningBalance: money.Format(opening.net(), cur),
		In: period.f(period.in), Out: period.f(period.out), Net: period.f(period.net()), ClosingBalance: money.Format(closing, cur),
		EntryCount: period.count, Entries: lines}
	if q.By != "" {
		cc.Groups = cashGroups(cur, list, q)
	}
	return cc, nil
}

func cashGroups(cur string, list []Entry, q ReportQuery) []CashGroup {
	keyOf := func(e Entry) string {
		if q.By == "method" {
			return e.Method
		}
		return e.Branch
	}
	type acc struct{ opening, period *sums }
	groups := map[string]*acc{}
	var keys []string
	for _, e := range list {
		k := keyOf(e)
		if q.By == "branch" {
			// Branch labels are compared case-insensitively; the first spelling names the group.
			for _, have := range keys {
				if strings.EqualFold(have, k) {
					k = have
				}
			}
		}
		g, ok := groups[k]
		if !ok {
			g = &acc{opening: newSums(cur), period: newSums(cur)}
			groups[k] = g
			keys = append(keys, k)
		}
		if e.EffectiveDate < q.From {
			g.opening.add(e)
		} else {
			g.period.add(e)
		}
	}
	rank := func(k string) string {
		if q.By == "method" {
			return strconv.Itoa(slices.Index(Methods(), k) + 10)
		}
		if k == "" {
			return "\xff"
		}
		return strings.ToLower(k)
	}
	slices.SortFunc(keys, func(a, b string) int { return strings.Compare(rank(a), rank(b)) })
	out := make([]CashGroup, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		closing := new(big.Rat).Add(g.opening.net(), g.period.net())
		label := k
		if q.By == "method" {
			label = MethodLabel(k)
		} else if k == "" {
			label = "No branch"
		}
		out = append(out, CashGroup{Currency: cur, MinorUnit: money.MinorUnit(cur), Key: strPtr(k), Label: label,
			OpeningBalance: money.Format(g.opening.net(), cur), In: g.period.f(g.period.in), Out: g.period.f(g.period.out),
			Net: g.period.f(g.period.net()), ClosingBalance: money.Format(closing, cur), EntryCount: g.period.count})
	}
	return out
}

// ---------------------------------------------------------------------------
// Categories
// ---------------------------------------------------------------------------

// CategoriesReport serves GET .../reports/categories.
func (s *Service) CategoriesReport(ctx context.Context, p *auth.Principal, bookID uuid.UUID, q ReportQuery) (CategoriesReport, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return CategoriesReport{}, err
	}
	return s.categoriesReport(ctx, a, q)
}

func (s *Service) categoriesReport(ctx context.Context, a access, q ReportQuery) (CategoriesReport, error) {
	f := map[string]string{}
	s.period(&q, f, s.bookToday(ctx, a))
	if err := s.checkCurrency(ctx, &q, f); err != nil {
		return CategoriesReport{}, err
	}
	if len(f) > 0 {
		return CategoriesReport{}, &ValidationError{Fields: f}
	}
	list, err := s.all(ctx, a.book, Filter{Currency: q.Currency, Branch: q.Branch, From: q.From, To: q.To})
	if err != nil {
		return CategoriesReport{}, err
	}
	cats, err := s.categories(ctx, a.book)
	if err != nil {
		return CategoriesReport{}, err
	}
	order := map[string]int{}
	labels := map[string]string{}
	for i, c := range cats {
		order[c.Code], labels[c.Code] = i, c.Label
	}
	out := CategoriesReport{From: q.From, To: q.To, Currencies: []CategoryCurrency{}}
	codes, per := byCurrency(cashOnly(list))
	for _, cur := range codes {
		total := newSums(cur)
		byCat := map[string]*sums{}
		var keys []string
		for _, e := range per[cur] {
			total.add(e)
			if _, ok := byCat[e.Category]; !ok {
				byCat[e.Category] = newSums(cur)
				keys = append(keys, e.Category)
			}
			byCat[e.Category].add(e)
		}
		slices.SortFunc(keys, func(x, y string) int {
			ox, okx := order[x]
			oy, oky := order[y]
			switch {
			case x == "":
				return 1
			case y == "":
				return -1
			case okx && oky:
				return ox - oy
			}
			return strings.Compare(x, y)
		})
		cc := CategoryCurrency{Currency: cur, MinorUnit: money.MinorUnit(cur), Categories: []CategoryTotals{}, Total: total.totals()}
		for _, k := range keys {
			t := byCat[k].totals()
			label := labels[k]
			if k == "" {
				label = "Uncategorised"
			} else if label == "" {
				label = k
			}
			cc.Categories = append(cc.Categories, CategoryTotals{Currency: cur, MinorUnit: t.MinorUnit, Category: strPtr(k), Label: label,
				In: t.In, Out: t.Out, Net: t.Net, EntryCount: t.EntryCount})
		}
		out.Currencies = append(out.Currencies, cc)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Statement and receivables
// ---------------------------------------------------------------------------

// outstanding returns, for memo entries (receivables or payables), the
// amount settled on or before asOf and what is left, each in the entry's
// own currency.
func (s *Service) outstanding(ctx context.Context, book Book, list []Entry, asOf string) (map[uuid.UUID][2]*big.Rat, error) {
	settled, err := s.store.Settled(ctx, book, ids(list))
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][2]*big.Rat{}
	for _, e := range list {
		paid := new(big.Rat)
		for _, p := range settled[e.ID] {
			if p.Currency != e.Currency {
				continue // never happens: a settlement is in the same currency
			}
			if asOf == "" || p.EffectiveDate <= asOf {
				paid.Add(paid, money.Rat(p.Amount))
			}
		}
		left := new(big.Rat).Sub(money.Rat(e.Amount), paid)
		out[e.ID] = [2]*big.Rat{paid, left}
	}
	return out, nil
}

// Statement serves GET .../reports/statement?contact_id=.
func (s *Service) Statement(ctx context.Context, p *auth.Principal, bookID uuid.UUID, q ReportQuery) (Statement, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Statement{}, err
	}
	return s.statement(ctx, a, q)
}

func (s *Service) statement(ctx context.Context, a access, q ReportQuery) (Statement, error) {
	f := map[string]string{}
	s.period(&q, f, s.bookToday(ctx, a))
	if err := s.checkCurrency(ctx, &q, f); err != nil {
		return Statement{}, err
	}
	var contact ContactRef
	switch {
	case q.ContactID == nil:
		f["contact_id"] = "is required"
	case s.links == nil:
		contact = ContactRef{ID: *q.ContactID}
	default:
		got, ok, err := s.links.Contact(ctx, a.book, *q.ContactID)
		if err != nil {
			return Statement{}, err
		}
		if !ok {
			f["contact_id"] = "is not a contact of this book"
		}
		contact = got
	}
	if len(f) > 0 {
		return Statement{}, &ValidationError{Fields: f}
	}
	list, err := s.all(ctx, a.book, Filter{Currency: q.Currency, Branch: q.Branch, To: q.To, ContactID: q.ContactID})
	if err != nil {
		return Statement{}, err
	}
	var memo []Entry
	for _, e := range list {
		if (e.Type == TypeReceivable || e.Type == TypePayable) && e.ReversedBy == nil {
			memo = append(memo, e)
		}
	}
	left, err := s.outstanding(ctx, a.book, memo, q.To)
	if err != nil {
		return Statement{}, err
	}
	v, err := s.viewer(ctx, a)
	if err != nil {
		return Statement{}, err
	}
	out := Statement{From: q.From, To: q.To, Contact: contact, Currencies: []StatementCurrency{}}
	codes, per := byCurrency(list)
	for _, cur := range codes {
		opening, period, lines, err := cashLines(ctx, v, cur, cashOnly(per[cur]), q.From)
		if err != nil {
			return Statement{}, err
		}
		recv, pay := new(big.Rat), new(big.Rat)
		for _, e := range per[cur] {
			if (e.Type != TypeReceivable && e.Type != TypePayable) || e.ReversedBy != nil {
				continue
			}
			if e.Type == TypeReceivable {
				recv.Add(recv, left[e.ID][1])
			} else {
				pay.Add(pay, left[e.ID][1])
			}
		}
		closing := new(big.Rat).Add(opening.net(), period.net())
		out.Currencies = append(out.Currencies, StatementCurrency{Currency: cur, MinorUnit: money.MinorUnit(cur),
			OpeningBalance: money.Format(opening.net(), cur), In: period.f(period.in), Out: period.f(period.out), Net: period.f(period.net()),
			ClosingBalance: money.Format(closing, cur), EntryCount: period.count, ReceivableOutstanding: money.Format(recv, cur),
			PayableOutstanding: money.Format(pay, cur), Entries: lines})
	}
	return out, nil
}

func daysBetween(from, to string) int {
	a, err1 := time.Parse(time.DateOnly, from)
	b, err2 := time.Parse(time.DateOnly, to)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(b.Sub(a).Hours() / 24)
}

// Receivables serves GET .../reports/receivables: open receivables (or
// payables) and their ageing, per currency.
func (s *Service) Receivables(ctx context.Context, p *auth.Principal, bookID uuid.UUID, q ReportQuery) (Receivables, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Receivables{}, err
	}
	f := map[string]string{}
	if q.AsOf == "" {
		q.AsOf = s.bookToday(ctx, a)
	} else if !validDate(q.AsOf) {
		f["as_of"] = "must be a date (YYYY-MM-DD)"
	}
	switch q.Side {
	case "":
		q.Side = TypeReceivable
	case TypeReceivable, TypePayable:
	default:
		f["side"] = "must be RECEIVABLE or PAYABLE"
	}
	if err := s.checkCurrency(ctx, &q, f); err != nil {
		return Receivables{}, err
	}
	if q.ContactID != nil && s.links != nil {
		if _, ok, err := s.links.Contact(ctx, a.book, *q.ContactID); err != nil {
			return Receivables{}, err
		} else if !ok {
			f["contact_id"] = "is not a contact of this book"
		}
	}
	if len(f) > 0 {
		return Receivables{}, &ValidationError{Fields: f}
	}
	list, err := s.all(ctx, a.book, Filter{Type: q.Side, Currency: q.Currency, Branch: q.Branch, To: q.AsOf, ContactID: q.ContactID,
		ExcludeReversed: true})
	if err != nil {
		return Receivables{}, err
	}
	left, err := s.outstanding(ctx, a.book, list, q.AsOf)
	if err != nil {
		return Receivables{}, err
	}
	v, err := s.viewer(ctx, a)
	if err != nil {
		return Receivables{}, err
	}
	rent, err := s.rentArrears(ctx, a.book, q)
	if err != nil {
		return Receivables{}, err
	}
	out := Receivables{AsOf: q.AsOf, Side: q.Side, Currencies: []AgedCurrency{}}
	codes, per := byCurrency(list)
	for _, r := range rent {
		if _, ok := per[r.Currency]; !ok {
			per[r.Currency] = nil
			codes = append(codes, r.Currency)
		}
	}
	slices.Sort(codes)
	for _, cur := range codes {
		mu := money.MinorUnit(cur)
		total, rentTotal := new(big.Rat), new(big.Rat)
		perBucket := map[string]*big.Rat{}
		counts := map[string]int{}
		for _, b := range bucketNames {
			perBucket[b] = new(big.Rat)
		}
		ac := AgedCurrency{Currency: cur, MinorUnit: mu, Items: []AgedItem{}, RentItems: []RentAgedItem{}}
		for _, e := range per[cur] {
			paid, rest := left[e.ID][0], left[e.ID][1]
			if rest.Sign() <= 0 {
				continue
			}
			days := max(daysBetween(e.EffectiveDate, q.AsOf), 0)
			b := bucketOf(days)
			perBucket[b].Add(perBucket[b], rest)
			counts[b]++
			total.Add(total, rest)
			ev, err := v.view(ctx, e)
			if err != nil {
				return Receivables{}, err
			}
			ac.Items = append(ac.Items, AgedItem{Entry: ev, Currency: cur, MinorUnit: mu, Amount: e.Amount, Settled: money.Format(paid, cur),
				Outstanding: money.Format(rest, cur), DaysOverdue: days, Bucket: b})
		}
		for _, r := range rent {
			if r.Currency != cur {
				continue
			}
			rest := money.Rat(r.Outstanding)
			days := max(daysBetween(r.DueDate, q.AsOf), 0)
			b := bucketOf(days)
			perBucket[b].Add(perBucket[b], rest)
			counts[b]++
			total.Add(total, rest)
			rentTotal.Add(rentTotal, rest)
			ac.RentItems = append(ac.RentItems, RentAgedItem{Tenancy: r.Tenancy, DueID: r.DueID, DueDate: r.DueDate, Currency: cur, MinorUnit: mu,
				Amount: r.Amount, Settled: r.Settled, Outstanding: r.Outstanding, DaysOverdue: days, Bucket: b})
		}
		if len(ac.Items) == 0 && len(ac.RentItems) == 0 {
			continue
		}
		sort.SliceStable(ac.RentItems, func(i, j int) bool {
			if ac.RentItems[i].DaysOverdue != ac.RentItems[j].DaysOverdue {
				return ac.RentItems[i].DaysOverdue > ac.RentItems[j].DaysOverdue
			}
			return ac.RentItems[i].DueID.String() < ac.RentItems[j].DueID.String()
		})
		ac.Outstanding, ac.Count = money.Format(total, cur), len(ac.Items)+len(ac.RentItems)
		ac.RentOutstanding = money.Format(rentTotal, cur)
		for _, b := range bucketNames {
			ac.Buckets = append(ac.Buckets, Bucket{Currency: cur, MinorUnit: mu, Bucket: b, Outstanding: money.Format(perBucket[b], cur), Count: counts[b]})
		}
		out.Currencies = append(out.Currencies, ac)
	}
	return out, nil
}

// rentArrears returns the book's rent arrears the receivables report
// includes: side RECEIVABLE, no branch filter (rent has no branch), the
// currency and contact (a tenant) filters applied.
func (s *Service) rentArrears(ctx context.Context, book Book, q ReportQuery) ([]RentArrear, error) {
	if s.arrears == nil || q.Side != TypeReceivable || q.Branch != nil {
		return nil, nil
	}
	list, err := s.arrears(ctx, book, q.AsOf)
	if err != nil {
		return nil, err
	}
	var out []RentArrear
	for _, r := range list {
		if q.Currency != "" && r.Currency != q.Currency {
			continue
		}
		if q.ContactID != nil && !slices.Contains(r.Tenants, *q.ContactID) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

// Export is a CSV export.
type Export struct {
	FileName string
	Rows     [][]string
}

// cell guards a text cell against spreadsheet formula injection.
func cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func idCell(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// ExportCSV serves GET .../reports/export.csv (OWNER and MANAGER, audited).
// f is the entries filter (report=entries).
func (s *Service) ExportCSV(ctx context.Context, p *auth.Principal, bookID uuid.UUID, report string, q ReportQuery, f Filter) (Export, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Export{}, err
	}
	if !a.manage {
		return Export{}, ErrForbidden
	}
	var out Export
	switch report {
	case ReportCashbook:
		out, err = s.exportCashbook(ctx, a, q)
	case ReportCategories:
		out, err = s.exportCategories(ctx, a, q)
	case ReportStatement:
		out, err = s.exportStatement(ctx, a, q)
	case ReportEntries:
		out, err = s.exportEntries(ctx, a, f)
	default:
		return Export{}, &ValidationError{Fields: map[string]string{"report": "must be cashbook, entries, categories or statement"}}
	}
	if err != nil {
		return Export{}, err
	}
	after := map[string]any{"report": report, "rows": len(out.Rows) - 1}
	for k, v := range map[string]string{"from": q.From, "to": q.To, "currency": q.Currency} {
		if report == ReportEntries {
			v = map[string]string{"from": f.From, "to": f.To, "currency": f.Currency}[k]
		}
		if v != "" {
			after[k] = v
		}
	}
	if ch := (Change{Audit: s.event(ctx, a, orgaudit.ActionLedgerExported, orgaudit.TargetLedgerBook, a.book.ID(), report+" export", nil, after)}); len(ch.Audit) > 0 {
		if err := s.store.Apply(ctx, ch); err != nil {
			return Export{}, err
		}
	}
	return out, nil
}

func (s *Service) exportCashbook(ctx context.Context, a access, q ReportQuery) (Export, error) {
	cb, err := s.cashbook(ctx, a, q)
	if err != nil {
		return Export{}, err
	}
	rows := [][]string{{"currency", "row", "effective_date", "entry_id", "type", "method", "branch", "description", "reference", "in", "out", "balance"}}
	for _, c := range cb.Currencies {
		rows = append(rows, []string{c.Currency, "OPENING", cb.From, "", "", "", "", "", "", "", "", c.OpeningBalance})
		for _, l := range c.Entries {
			e := l.Entry
			rows = append(rows, []string{c.Currency, "ENTRY", e.EffectiveDate, e.ID.String(), e.Type, deref(e.Method), cell(deref(e.Branch)),
				cell(e.Description), cell(e.Reference), l.In, l.Out, l.RunningBalance})
		}
		rows = append(rows, []string{c.Currency, "CLOSING", cb.To, "", "", "", "", "", "", c.In, c.Out, c.ClosingBalance})
	}
	return Export{FileName: fmt.Sprintf("ledger-cashbook-%s-%s.csv", cb.From, cb.To), Rows: rows}, nil
}

func (s *Service) exportCategories(ctx context.Context, a access, q ReportQuery) (Export, error) {
	cr, err := s.categoriesReport(ctx, a, q)
	if err != nil {
		return Export{}, err
	}
	rows := [][]string{{"currency", "category", "category_label", "in", "out", "net", "entry_count"}}
	for _, c := range cr.Currencies {
		for _, t := range c.Categories {
			rows = append(rows, []string{c.Currency, deref(t.Category), cell(t.Label), t.In, t.Out, t.Net, strconv.Itoa(t.EntryCount)})
		}
		rows = append(rows, []string{c.Currency, "TOTAL", "Total", c.Total.In, c.Total.Out, c.Total.Net, strconv.Itoa(c.Total.EntryCount)})
	}
	return Export{FileName: fmt.Sprintf("ledger-categories-%s-%s.csv", cr.From, cr.To), Rows: rows}, nil
}

func (s *Service) exportStatement(ctx context.Context, a access, q ReportQuery) (Export, error) {
	st, err := s.statement(ctx, a, q)
	if err != nil {
		return Export{}, err
	}
	rows := [][]string{{"currency", "row", "effective_date", "entry_id", "type", "description", "reference", "in", "out", "balance"}}
	for _, c := range st.Currencies {
		rows = append(rows, []string{c.Currency, "OPENING", st.From, "", "", "", "", "", "", c.OpeningBalance})
		for _, l := range c.Entries {
			e := l.Entry
			rows = append(rows, []string{c.Currency, "ENTRY", e.EffectiveDate, e.ID.String(), e.Type, cell(e.Description), cell(e.Reference), l.In, l.Out, l.RunningBalance})
		}
		rows = append(rows, []string{c.Currency, "CLOSING", st.To, "", "", "", "", c.In, c.Out, c.ClosingBalance})
	}
	return Export{FileName: fmt.Sprintf("ledger-statement-%s-%s.csv", st.From, st.To), Rows: rows}, nil
}

func (s *Service) exportEntries(ctx context.Context, a access, f Filter) (Export, error) {
	f.Limit, f.Offset, f.Ascending = MaxExportRows, 0, true
	list, _, err := s.store.Entries(ctx, a.book, f)
	if err != nil {
		return Export{}, err
	}
	v, err := s.viewer(ctx, a)
	if err != nil {
		return Export{}, err
	}
	rows := [][]string{{"entry_id", "effective_date", "recorded_at", "type", "direction", "category", "method", "branch", "currency", "amount",
		"signed_amount", "status", "description", "reference", "contact_id", "contact_name", "listing_id", "lease_id", "reversal_of",
		"reversed_by", "correction_of", "corrected_by", "transfer_pair", "settles", "author_name"}}
	for _, e := range list {
		ev, err := v.view(ctx, e)
		if err != nil {
			return Export{}, err
		}
		contactName := ""
		if ev.Contact != nil {
			contactName = ev.Contact.Name
		}
		rows = append(rows, []string{e.ID.String(), e.EffectiveDate, e.RecordedAt.Format(time.RFC3339), e.Type, e.Direction, e.Category, e.Method,
			cell(e.Branch), e.Currency, e.Amount, ev.SignedAmount, e.Status, cell(e.Description), cell(e.Reference), idCell(e.ContactID),
			cell(contactName), idCell(e.ListingID), idCell(e.LeaseID), idCell(e.ReversalOf), idCell(e.ReversedBy), idCell(e.CorrectionOf),
			idCell(e.CorrectedBy), idCell(e.TransferPair), idCell(e.Settles), cell(e.AuthorName)})
	}
	name := "ledger-entries-all.csv"
	if f.From != "" || f.To != "" {
		name = fmt.Sprintf("ledger-entries-%s-%s.csv", orDash(f.From), orDash(f.To))
	}
	return Export{FileName: name, Rows: rows}, nil
}

func orDash(s string) string {
	if s == "" {
		return "all"
	}
	return s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// mayExport answers ErrForbidden to a reader who may not export.
func (s *Service) mayExport(p *auth.Principal, bookID uuid.UUID) error {
	a, err := s.access(p, bookID)
	if err != nil {
		return err
	}
	if !a.manage {
		return ErrForbidden
	}
	return nil
}
