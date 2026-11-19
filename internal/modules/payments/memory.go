package payments

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// MemoryStore is the in-memory Store for development and tests.
type MemoryStore struct {
	mu       sync.Mutex
	payments map[uuid.UUID]Payment
	refunds  map[uuid.UUID]Refund
	events   map[string]*memEvent
	audit    *adminaudit.MemoryLog
}

type memEvent struct {
	rec       EventRecord
	started   *time.Time
	processed *time.Time
	outcome   string
	paymentID *uuid.UUID
}

// NewMemoryStore returns an empty store writing its audit records to audit
// (nil: a private log).
func NewMemoryStore(audit *adminaudit.MemoryLog) *MemoryStore {
	if audit == nil {
		audit = &adminaudit.MemoryLog{}
	}
	return &MemoryStore{payments: map[uuid.UUID]Payment{}, refunds: map[uuid.UUID]Refund{}, events: map[string]*memEvent{}, audit: audit}
}

// AuditLog returns the audit log the store writes to.
func (s *MemoryStore) AuditLog() *adminaudit.MemoryLog { return s.audit }

// EventOutcome returns a processed event's outcome ("" when unknown or not
// processed; test helper).
func (s *MemoryStore) EventOutcome(provider, eventID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.events[provider+"\x00"+eventID]; ok && e.processed != nil {
		return e.outcome
	}
	return ""
}

// ReserveCheckout implements Store.
func (s *MemoryStore) ReserveCheckout(_ context.Context, p Payment, now time.Time) (Payment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var open *Payment
	for _, x := range s.payments {
		if x.SubscriptionID == p.SubscriptionID && x.Status == StatusOpen {
			x := x
			open = &x
		}
	}
	reuse, retired, err := reserve(open, now)
	if err != nil {
		return Payment{}, false, err
	}
	if reuse {
		return *open, true, nil
	}
	if retired != nil {
		s.payments[retired.ID] = *retired
	}
	for _, x := range s.payments {
		if x.IdempotencyKey == p.IdempotencyKey {
			return Payment{}, false, errors.New("payments: duplicate idempotency key")
		}
	}
	s.payments[p.ID] = p
	return p, false, nil
}

// Payment implements Store.
func (s *MemoryStore) Payment(_ context.Context, id uuid.UUID) (Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payments[id]
	if !ok {
		return Payment{}, ErrPaymentNotFound
	}
	return p, nil
}

// PaymentByRef implements Store.
func (s *MemoryStore) PaymentByRef(_ context.Context, provider, ref string) (Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref == "" {
		return Payment{}, ErrPaymentNotFound
	}
	for _, p := range s.payments {
		if p.Provider == provider && (p.ProviderRef == ref || p.PaymentRef == ref) {
			return p, nil
		}
	}
	return Payment{}, ErrPaymentNotFound
}

// Matches reports whether a payment passes the filter (the rule the
// Postgres query expresses in SQL).
func (f Filter) Matches(p Payment) bool {
	q := strings.ToLower(strings.TrimSpace(f.Q))
	switch {
	case len(f.Statuses) > 0 && !slices.Contains(f.Statuses, p.Status),
		f.Provider != "" && p.Provider != f.Provider,
		f.SubscriptionID != nil && p.SubscriptionID != *f.SubscriptionID,
		q != "" && !strings.Contains(strings.ToLower(p.InvoiceNumber), q) && !strings.Contains(strings.ToLower(p.SubscriberName), q) &&
			!strings.Contains(strings.ToLower(p.ProviderRef), q) && !strings.Contains(strings.ToLower(p.PaymentRef), q),
		!f.UpdatedSince.IsZero() && p.UpdatedAt.Before(f.UpdatedSince),
		!f.CreatedFrom.IsZero() && p.CreatedAt.Before(f.CreatedFrom),
		!f.CreatedTo.IsZero() && !p.CreatedAt.Before(f.CreatedTo):
		return false
	}
	return true
}

// Payments implements Store.
func (s *MemoryStore) Payments(_ context.Context, f Filter) ([]Payment, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found []Payment
	for _, p := range s.payments {
		if f.Matches(p) {
			found = append(found, p)
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		if !found[i].CreatedAt.Equal(found[j].CreatedAt) {
			return found[i].CreatedAt.After(found[j].CreatedAt)
		}
		return found[i].ID.String() > found[j].ID.String()
	})
	total := len(found)
	limit := f.Limit
	if limit <= 0 {
		limit = total
	}
	start := min(max(f.Offset, 0), total)
	end := min(start+limit, total)
	return append([]Payment{}, found[start:end]...), total, nil
}

// ChangePayment implements Store.
func (s *MemoryStore) ChangePayment(_ context.Context, id uuid.UUID, change func(p *Payment) ([]adminaudit.Event, error)) (Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.payments[id]
	if !ok {
		return Payment{}, ErrPaymentNotFound
	}
	next := cur
	audits, err := change(&next)
	if errors.Is(err, errNoChange) {
		return cur, nil
	}
	if err != nil {
		return Payment{}, err
	}
	s.payments[id] = next
	for _, a := range audits {
		s.audit.Append(a)
	}
	return next, nil
}

// ClaimEvent implements Store.
func (s *MemoryStore) ClaimEvent(_ context.Context, e EventRecord, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := e.Provider + "\x00" + e.EventID
	ev, ok := s.events[key]
	if !ok {
		t := now
		s.events[key] = &memEvent{rec: e, started: &t}
		return true, nil
	}
	if ev.processed != nil || (ev.started != nil && now.Sub(*ev.started) < EventLease) {
		return false, nil
	}
	t := now
	ev.started = &t
	return true, nil
}

// FinishEvent implements Store.
func (s *MemoryStore) FinishEvent(_ context.Context, provider, eventID, outcome string, paymentID *uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev, ok := s.events[provider+"\x00"+eventID]; ok {
		t := at
		ev.processed, ev.outcome, ev.paymentID = &t, outcome, paymentID
	}
	return nil
}

// ReleaseEvent implements Store.
func (s *MemoryStore) ReleaseEvent(_ context.Context, provider, eventID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev, ok := s.events[provider+"\x00"+eventID]; ok && ev.processed == nil {
		ev.started = nil
	}
	return nil
}

func (s *MemoryStore) refundsLocked(paymentID uuid.UUID) []Refund {
	out := []Refund{}
	for _, r := range s.refunds {
		if r.PaymentID == paymentID {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// CreateRefund implements Store.
func (s *MemoryStore) CreateRefund(_ context.Context, r Refund, check func(p Payment, refunds []Refund) error) (Refund, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payments[r.PaymentID]
	if !ok {
		return Refund{}, false, ErrPaymentNotFound
	}
	for _, x := range s.refunds {
		if x.IdempotencyKey == r.IdempotencyKey {
			return x, true, nil
		}
	}
	if check != nil {
		if err := check(p, s.refundsLocked(p.ID)); err != nil {
			return Refund{}, false, err
		}
	}
	s.refunds[r.ID] = r
	return r, false, nil
}

// ChangeRefund implements Store.
func (s *MemoryStore) ChangeRefund(_ context.Context, id uuid.UUID, change func(r *Refund, p *Payment) ([]adminaudit.Event, error)) (Refund, Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.refunds[id]
	if !ok {
		return Refund{}, Payment{}, ErrRefundNotFound
	}
	p := s.payments[r.PaymentID]
	nr, np := r, p
	audits, err := change(&nr, &np)
	if errors.Is(err, errNoChange) {
		return r, p, nil
	}
	if err != nil {
		return Refund{}, Payment{}, err
	}
	s.refunds[id], s.payments[p.ID] = nr, np
	for _, a := range audits {
		s.audit.Append(a)
	}
	return nr, np, nil
}

// RefundByProviderRef implements Store.
func (s *MemoryStore) RefundByProviderRef(_ context.Context, paymentID uuid.UUID, ref string) (Refund, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.refunds {
		if r.PaymentID == paymentID && ref != "" && r.ProviderRefundRef == ref {
			return r, nil
		}
	}
	return Refund{}, ErrRefundNotFound
}

// Refunds implements Store.
func (s *MemoryStore) Refunds(_ context.Context, paymentIDs []uuid.UUID) (map[uuid.UUID][]Refund, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID][]Refund{}
	for _, id := range paymentIDs {
		out[id] = s.refundsLocked(id)
	}
	return out, nil
}

// ForgetUser is ForgetUserTx of the Postgres store.
func (s *MemoryStore) ForgetUser(userID uuid.UUID, deletedName string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.payments {
		if p.OpenedBy != nil && *p.OpenedBy == userID {
			p.OpenedBy, p.OpenedByName = nil, deletedName
		}
		if p.Status == StatusOpen && p.SubscriberKind == "INDIVIDUAL" && p.SubscriberID == userID {
			t := at
			p.Status, p.ExpiredAt, p.UpdatedAt, p.HostedURL = StatusExpired, &t, at, ""
		}
		s.payments[id] = p
	}
	for id, r := range s.refunds {
		if r.RequestedBy != nil && *r.RequestedBy == userID {
			r.RequestedBy, r.RequestedByName = nil, deletedName
			s.refunds[id] = r
		}
	}
}
