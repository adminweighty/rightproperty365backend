package invoices

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore keeps invoices in memory (development without a database, tests).
type MemoryStore struct {
	mu       sync.Mutex
	invoices map[uuid.UUID]Invoice
	payments []Payment
	credits  []CreditNote
	events   []Event
	audit    []adminaudit.Event
	counters map[string]int
	outbox   []outbox.Event
	sink     outbox.Sink
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{invoices: map[uuid.UUID]Invoice{}, counters: map[string]int{}}
}

// SetOutboxSink receives the events as they are written (development).
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// Outbox returns the events written (tests).
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.outbox)
}

// Audit returns the audit records written (tests).
func (s *MemoryStore) Audit() []adminaudit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.audit)
}

func clone(inv Invoice) Invoice {
	inv.Lines = slices.Clone(inv.Lines)
	if inv.Tax != nil {
		t := *inv.Tax
		inv.Tax = &t
	}
	return inv
}

// next takes the next number of a kind in a market and year. Caller holds the lock.
func (s *MemoryStore) next(kind, market string, year int) string {
	key := kind + "/" + market + "/" + time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006")
	s.counters[key]++
	return FormatNumber(kind, market, year, s.counters[key])
}

// Create implements Store.
func (s *MemoryStore) Create(_ context.Context, inv Invoice, ev Event, a adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invoices[inv.ID] = clone(inv)
	s.events = append(s.events, ev)
	s.audit = append(s.audit, a)
	return nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Invoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invoices[id]
	if !ok {
		return Invoice{}, ErrNotFound
	}
	return clone(inv), nil
}

func (f Filter) matches(inv Invoice) bool {
	if len(f.Statuses) > 0 && !slices.Contains(f.Statuses, inv.Status) {
		return false
	}
	if f.ExcludeDrafts && inv.Status == StatusDraft {
		return false
	}
	if f.Market != "" && inv.MarketCode != f.Market {
		return false
	}
	if f.Subscribers != nil && !slices.Contains(f.Subscribers, inv.SubscriberID) {
		return false
	}
	if f.RenewsSubscription != nil && inv.RenewsSubscriptionID != *f.RenewsSubscription {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Q)); q != "" &&
		!strings.Contains(strings.ToLower(inv.Number), q) && !strings.Contains(strings.ToLower(inv.PayerName), q) {
		return false
	}
	return true
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, f Filter) ([]Invoice, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Invoice
	for _, inv := range s.invoices {
		if f.matches(inv) {
			out = append(out, clone(inv))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	total := len(out)
	start := min(max(f.Offset, 0), total)
	end := total
	if f.Limit > 0 {
		end = min(start+f.Limit, total)
	}
	return out[start:end], total, nil
}

// Change implements Store.
func (s *MemoryStore) Change(_ context.Context, id uuid.UUID, change func(inv *Invoice) (Change, error)) (Invoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.invoices[id]
	if !ok {
		return Invoice{}, ErrNotFound
	}
	x := clone(cur)
	ch, err := change(&x)
	if err != nil {
		return Invoice{}, err
	}
	now := x.UpdatedAt
	if ch.Payment != nil && ch.Payment.SourceKey != "" {
		for _, p := range s.payments {
			if p.InvoiceID == id && p.SourceKey == ch.Payment.SourceKey {
				return Invoice{}, ErrDuplicatePayment
			}
		}
	}
	if ch.Number && x.Number == "" {
		x.Number = s.next(KindInvoice, x.MarketCode, now.Year())
	}
	if ch.Payment != nil {
		p := *ch.Payment
		p.ReceiptNumber = s.next(KindReceipt, x.MarketCode, now.Year())
		s.payments = append(s.payments, p)
	}
	if ch.Credit != nil {
		c := *ch.Credit
		c.Number = s.next(KindCredit, x.MarketCode, now.Year())
		s.credits = append(s.credits, c)
	}
	s.invoices[id] = clone(x)
	s.events = append(s.events, ch.Events...)
	for _, a := range ch.Audit {
		if a.TargetLabel == "Draft for "+x.PayerName && x.Number != "" {
			a.TargetLabel = x.Number
		}
		s.audit = append(s.audit, a)
	}
	for _, o := range ch.Outbox {
		ev := outboxEvent(id, o, now)
		s.outbox = append(s.outbox, ev)
		if s.sink != nil {
			s.sink(ev)
		}
	}
	return clone(x), nil
}

// Delete implements Store.
func (s *MemoryStore) Delete(_ context.Context, id uuid.UUID, a adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invoices[id]
	if !ok {
		return ErrNotFound
	}
	if inv.Status != StatusDraft {
		return ErrState
	}
	delete(s.invoices, id)
	s.events = slices.DeleteFunc(s.events, func(e Event) bool { return e.InvoiceID == id })
	s.audit = append(s.audit, a)
	return nil
}

// Payments implements Store.
func (s *MemoryStore) Payments(_ context.Context, id uuid.UUID) ([]Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Payment{}
	for _, p := range s.payments {
		if p.InvoiceID == id {
			out = append(out, p)
		}
	}
	return out, nil
}

// Credits implements Store.
func (s *MemoryStore) Credits(_ context.Context, id uuid.UUID) ([]CreditNote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []CreditNote{}
	for _, c := range s.credits {
		if c.InvoiceID == id {
			out = append(out, c)
		}
	}
	return out, nil
}

// Events implements Store.
func (s *MemoryStore) Events(_ context.Context, id uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Event{}
	for _, e := range s.events {
		if e.InvoiceID == id {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// BySubscription implements Store.
func (s *MemoryStore) BySubscription(_ context.Context, subscriptionID uuid.UUID) (Invoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, inv := range s.invoices {
		if inv.SubscriptionID != nil && *inv.SubscriptionID == subscriptionID {
			return clone(inv), nil
		}
	}
	return Invoice{}, ErrNotFound
}

// DueCandidates implements Store.
func (s *MemoryStore) DueCandidates(_ context.Context, d time.Time) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uuid.UUID
	for _, inv := range s.invoices {
		if (inv.Status == StatusIssued || inv.Status == StatusPartPaid) && !inv.DueOn.After(d) {
			out = append(out, inv.ID)
		}
	}
	return out, nil
}
