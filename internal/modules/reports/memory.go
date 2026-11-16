package reports

import (
	"context"
	"sort"
	"time"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/subscriptions"
)

// Row is one thing counted by the memory store: its group (status or
// role), market ("" for none) and creation time.
type Row struct {
	Group      string
	MarketCode string
	At         time.Time
}

// MemorySources read the in-memory stores (development and tests). A nil
// source counts nothing.
type MemorySources struct {
	Users         func() []UserRow
	Onboarding    func() []Row
	Listings      func(now time.Time) []listings.ReportRow
	Requests      func() []Row
	Enquiries     func() []Row
	Leases        func() []Row
	Agreements    func() []Row
	Subscriptions func() []subscriptions.Subscription
	Now           func() time.Time
}

// MemoryStore computes the reports from MemorySources.
type MemoryStore struct{ src MemorySources }

// NewMemoryStore returns a memory store.
func NewMemoryStore(src MemorySources) *MemoryStore {
	if src.Now == nil {
		src.Now = time.Now
	}
	return &MemoryStore{src: src}
}

func group(rows []Row, r Range) []Count {
	type key struct{ group, market string }
	n := map[key]int{}
	for _, row := range rows {
		if r.Contains(row.At) && r.HasMarket(row.MarketCode) {
			n[key{row.Group, row.MarketCode}]++
		}
	}
	out := make([]Count, 0, len(n))
	for k, c := range n {
		out = append(out, Count{Group: k.group, MarketCode: k.market, Count: c})
	}
	sortCounts(out)
	return out
}

func sortCounts(out []Count) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].MarketCode < out[j].MarketCode
	})
}

func rowsOf(f func() []Row) []Row {
	if f == nil {
		return nil
	}
	return f()
}

// firstMarket is the market an account is counted in.
func firstMarket(codes []string) string {
	first := ""
	for _, c := range codes {
		if first == "" || c < first {
			first = c
		}
	}
	return first
}

// NewUsers implements Store.
func (s *MemoryStore) NewUsers(_ context.Context, r Range) ([]Count, error) {
	var rows []Row
	if s.src.Users != nil {
		for _, u := range s.src.Users() {
			if len(u.PlatformRoles) > 0 {
				continue
			}
			market := firstMarket(u.MarketCodes)
			if len(u.AccountRoles) == 0 {
				rows = append(rows, Row{Group: NoRole, MarketCode: market, At: u.CreatedAt})
			}
			for _, role := range u.AccountRoles {
				rows = append(rows, Row{Group: role, MarketCode: market, At: u.CreatedAt})
			}
		}
	}
	return group(rows, r), nil
}

// Onboarding implements Store.
func (s *MemoryStore) Onboarding(_ context.Context, r Range) ([]Count, error) {
	return group(rowsOf(s.src.Onboarding), r), nil
}

// Listings implements Store.
func (s *MemoryStore) Listings(_ context.Context, r Range) ([]Count, error) {
	var rows []Row
	if s.src.Listings != nil {
		for _, l := range s.src.Listings(s.src.Now()) {
			rows = append(rows, Row{Group: l.Status, MarketCode: l.MarketCode, At: l.CreatedAt})
		}
	}
	return group(rows, r), nil
}

// PropertyRequests implements Store.
func (s *MemoryStore) PropertyRequests(_ context.Context, r Range) ([]Count, error) {
	return group(rowsOf(s.src.Requests), r), nil
}

// BusinessEnquiries implements Store.
func (s *MemoryStore) BusinessEnquiries(_ context.Context, r Range) ([]Count, error) {
	return group(rowsOf(s.src.Enquiries), r), nil
}

// Leases implements Store.
func (s *MemoryStore) Leases(_ context.Context, r Range) ([]Count, error) {
	return group(rowsOf(s.src.Leases), r), nil
}

// Agreements implements Store.
func (s *MemoryStore) Agreements(_ context.Context, r Range) ([]Count, error) {
	return group(rowsOf(s.src.Agreements), r), nil
}

func (s *MemoryStore) subscriptions() []subscriptions.Subscription {
	if s.src.Subscriptions == nil {
		return nil
	}
	return s.src.Subscriptions()
}

// Subscriptions implements Store.
func (s *MemoryStore) Subscriptions(_ context.Context, r Range) ([]Count, error) {
	now := s.src.Now()
	var rows []Row
	for _, x := range s.subscriptions() {
		rows = append(rows, Row{Group: x.EffectiveStatus(now), MarketCode: x.MarketCode, At: x.CreatedAt})
	}
	return group(rows, r), nil
}

// Revenue implements Store.
func (s *MemoryStore) Revenue(_ context.Context, r Range) ([]Amount, error) {
	type key struct{ currency, market string }
	amounts := map[key][]string{}
	from, to := day(r.From), day(r.To)
	for _, x := range s.subscriptions() {
		if x.Payment == nil || x.Payment.ReceivedOn < from || x.Payment.ReceivedOn >= to || !r.HasMarket(x.MarketCode) {
			continue
		}
		k := key{x.CurrencyCode, x.MarketCode}
		amounts[k] = append(amounts[k], x.Payment.Amount)
	}
	out := make([]Amount, 0, len(amounts))
	for k, list := range amounts {
		out = append(out, Amount{CurrencyCode: k.currency, MarketCode: k.market, Total: subscriptions.SumAmounts(list...), Payments: len(list)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CurrencyCode != out[j].CurrencyCode {
			return out[i].CurrencyCode < out[j].CurrencyCode
		}
		return out[i].MarketCode < out[j].MarketCode
	})
	return out, nil
}

func hasAny(have, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

// ExportUsers implements Store.
func (s *MemoryStore) ExportUsers(_ context.Context, r Range, limit int, start func(int), each func(UserRow) error) error {
	var kept []UserRow
	if s.src.Users != nil {
		for _, u := range s.src.Users() {
			if r.Contains(u.CreatedAt) && (r.Markets == nil || hasAny(u.MarketCodes, r.Markets)) {
				kept = append(kept, u)
			}
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].CreatedAt.Equal(kept[j].CreatedAt) {
			return kept[i].CreatedAt.After(kept[j].CreatedAt)
		}
		return kept[i].ID.String() > kept[j].ID.String()
	})
	start(len(kept))
	for i, u := range kept {
		if i >= limit {
			break
		}
		if err := each(u); err != nil {
			return err
		}
	}
	return nil
}

// ExportListings implements Store.
func (s *MemoryStore) ExportListings(_ context.Context, r Range, limit int, start func(int), each func(listings.ReportRow) error) error {
	var kept []listings.ReportRow
	if s.src.Listings != nil {
		for _, l := range s.src.Listings(s.src.Now()) {
			if r.Contains(l.CreatedAt) && r.HasMarket(l.MarketCode) {
				kept = append(kept, l)
			}
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].CreatedAt.Equal(kept[j].CreatedAt) {
			return kept[i].CreatedAt.After(kept[j].CreatedAt)
		}
		return kept[i].ID.String() > kept[j].ID.String()
	})
	start(len(kept))
	for i, l := range kept {
		if i >= limit {
			break
		}
		if err := each(l); err != nil {
			return err
		}
	}
	return nil
}

// ExportSubscriptions implements Store.
func (s *MemoryStore) ExportSubscriptions(_ context.Context, r Range, limit int, start func(int), each func(subscriptions.Subscription) error) error {
	var kept []subscriptions.Subscription
	for _, x := range s.subscriptions() {
		if r.Contains(x.CreatedAt) && r.HasMarket(x.MarketCode) {
			kept = append(kept, x)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].CreatedAt.Equal(kept[j].CreatedAt) {
			return kept[i].CreatedAt.After(kept[j].CreatedAt)
		}
		return kept[i].ID.String() > kept[j].ID.String()
	})
	start(len(kept))
	for i, x := range kept {
		if i >= limit {
			break
		}
		if err := each(x); err != nil {
			return err
		}
	}
	return nil
}
