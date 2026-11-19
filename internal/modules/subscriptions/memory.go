package subscriptions

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore is the in-memory Store for development and tests.
type MemoryStore struct {
	mu     sync.Mutex
	plans  map[uuid.UUID]Plan
	subs   map[uuid.UUID]Subscription
	events []Event
	seq    int
	outbox []outbox.Event
	sink   outbox.Sink
	audit  *adminaudit.MemoryLog
	// activeListings counts a subscriber's public listings (nil: 0).
	activeListings func(kind string, subscriberID uuid.UUID, now time.Time) int
	// entitle applies a change's Entitlement (nil: ignored).
	entitle EntitlementFunc
}

// SetEntitlements sets the hook that grants and ends listing packages (wiring).
func (s *MemoryStore) SetEntitlements(fn EntitlementFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entitle = fn
}

// NewMemoryStore returns an empty store writing its administrator audit
// records to audit (nil: a private log).
func NewMemoryStore(audit *adminaudit.MemoryLog) *MemoryStore {
	if audit == nil {
		audit = &adminaudit.MemoryLog{}
	}
	return &MemoryStore{plans: map[uuid.UUID]Plan{}, subs: map[uuid.UUID]Subscription{}, audit: audit}
}

// SetActiveListings supplies the listing counter behind the usage figures.
func (s *MemoryStore) SetActiveListings(count func(kind string, subscriberID uuid.UUID, now time.Time) int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeListings = count
}

// SetOutboxSink registers a receiver for events as they are written.
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// Outbox returns a copy of the outbox events (test helper).
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]outbox.Event(nil), s.outbox...)
}

// AuditLog returns the audit log the store writes to.
func (s *MemoryStore) AuditLog() *adminaudit.MemoryLog { return s.audit }

// emit writes an outbox event. Caller holds the lock.
func (s *MemoryStore) emit(eventType string, id uuid.UUID, at time.Time) {
	payload, _ := json.Marshal(map[string]string{"subscription_id": id.String(), "occurred_at": at.UTC().Format(time.RFC3339Nano)})
	ev := outbox.Event{ID: uuid.New(), Type: eventType, AggregateType: AggregateSubscription, AggregateID: id, Payload: payload, Attempts: 1, CreatedAt: at}
	s.outbox = append(s.outbox, ev)
	if s.sink != nil {
		s.sink(ev)
	}
}

func clonePlan(p Plan) Plan {
	p.EligibleRoles = append([]string{}, p.EligibleRoles...)
	p.Benefits = append([]string{}, p.Benefits...)
	p.Prices = append([]Price{}, p.Prices...)
	features := make([]Feature, 0, len(p.Features))
	for _, f := range p.Features {
		if f.Limit != nil {
			n := *f.Limit
			f.Limit = &n
		}
		features = append(features, f)
	}
	p.Features = features
	return p
}

func cloneSub(x Subscription) Subscription {
	if x.Payment != nil {
		p := *x.Payment
		x.Payment = &p
	}
	return x
}

// NextInvoiceNumber implements Store.
func (s *MemoryStore) NextInvoiceNumber(_ context.Context, year int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return invoiceNumber(year, s.seq), nil
}

// CreatePlan implements Store.
func (s *MemoryStore) CreatePlan(_ context.Context, p Plan, audit adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plans[p.ID] = clonePlan(p)
	s.audit.Append(audit)
	return nil
}

// PutPlan stores a plan as it is, without an audit record (seed helper).
func (s *MemoryStore) PutPlan(p Plan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plans[p.ID] = clonePlan(p)
}

// Plan implements Store.
func (s *MemoryStore) Plan(_ context.Context, id uuid.UUID) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		return Plan{}, ErrPlanNotFound
	}
	return clonePlan(p), nil
}

// OrderPlans sorts plans the way every store returns them.
func OrderPlans(list []Plan) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].SortOrder != list[j].SortOrder {
			return list[i].SortOrder < list[j].SortOrder
		}
		if list[i].Name != list[j].Name {
			return list[i].Name < list[j].Name
		}
		return list[i].ID.String() < list[j].ID.String()
	})
}

// Plans implements Store.
func (s *MemoryStore) Plans(_ context.Context, includeRetired bool) ([]Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Plan{}
	for _, p := range s.plans {
		if p.Status == PlanRetired && !includeRetired {
			continue
		}
		out = append(out, clonePlan(p))
	}
	OrderPlans(out)
	return out, nil
}

// ChangePlan implements Store.
func (s *MemoryStore) ChangePlan(_ context.Context, id uuid.UUID, change func(p *Plan) (adminaudit.Event, error)) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.plans[id]
	if !ok {
		return Plan{}, ErrPlanNotFound
	}
	next := clonePlan(cur)
	audit, err := change(&next)
	if err != nil {
		return Plan{}, err
	}
	s.plans[id] = clonePlan(next)
	s.audit.Append(audit)
	return clonePlan(next), nil
}

func (s *MemoryStore) subscriberSubsLocked(kind string, id uuid.UUID) []Subscription {
	var out []Subscription
	for _, x := range s.subs {
		if x.SubscriberKind == kind && x.SubscriberID == id {
			out = append(out, cloneSub(x))
		}
	}
	newestFirst(out)
	return out
}

// CreateSubscription implements Store.
func (s *MemoryStore) CreateSubscription(_ context.Context, x Subscription, ev Event, check func([]Subscription) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := s.subscriberSubsLocked(x.SubscriberKind, x.SubscriberID)
	if check != nil {
		if err := check(existing); err != nil {
			return err
		}
	}
	for _, e := range existing {
		if e.Status == StatusPendingPayment {
			return ErrPendingExists
		}
	}
	s.subs[x.ID] = cloneSub(x)
	s.events = append(s.events, ev)
	s.emit(EventRequested, x.ID, x.CreatedAt)
	return nil
}

// PutSubscription stores a subscription as it is (seed helper).
func (s *MemoryStore) PutSubscription(x Subscription, events ...Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs[x.ID] = cloneSub(x)
	s.events = append(s.events, events...)
}

// Subscription implements Store.
func (s *MemoryStore) Subscription(_ context.Context, id uuid.UUID) (Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.subs[id]
	if !ok {
		return Subscription{}, ErrNotFound
	}
	return cloneSub(x), nil
}

func newestFirst(list []Subscription) {
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		}
		return list[i].ID.String() > list[j].ID.String()
	})
}

// Matches reports whether a subscription passes the filter (the rule the
// Postgres query expresses in SQL).
func (f Filter) Matches(x Subscription) bool {
	q := strings.ToLower(strings.TrimSpace(f.Q))
	switch {
	case f.Status != "" && x.Status != f.Status,
		f.PlanID != nil && x.PlanID != *f.PlanID,
		f.Market != "" && x.MarketCode != f.Market,
		f.Period != "" && x.Period != f.Period,
		f.SubscriberKind != "" && x.SubscriberKind != f.SubscriberKind,
		f.Subscribers != nil && !slices.Contains(f.Subscribers, x.SubscriberID),
		q != "" && !strings.Contains(strings.ToLower(x.InvoiceNumber), q) && !strings.Contains(strings.ToLower(x.SubscriberName), q),
		!f.CreatedFrom.IsZero() && x.CreatedAt.Before(f.CreatedFrom), !f.CreatedTo.IsZero() && !x.CreatedAt.Before(f.CreatedTo),
		f.Markets != nil && !slices.Contains(f.Markets, x.MarketCode):
		return false
	}
	return true
}

// Subscriptions implements Store.
func (s *MemoryStore) Subscriptions(_ context.Context, f Filter) ([]Subscription, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found []Subscription
	for _, x := range s.subs {
		if f.Matches(x) {
			found = append(found, cloneSub(x))
		}
	}
	newestFirst(found)
	total := len(found)
	limit := f.Limit
	if limit <= 0 {
		limit = total
	}
	start := min(max(f.Offset, 0), total)
	end := min(start+limit, total)
	return append([]Subscription{}, found[start:end]...), total, nil
}

// ChangeSubscription implements Store.
func (s *MemoryStore) ChangeSubscription(_ context.Context, id uuid.UUID, change func(x *Subscription) (Change, error)) (Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.subs[id]
	if !ok {
		return Subscription{}, ErrNotFound
	}
	next := cloneSub(cur)
	ch, err := change(&next)
	if err != nil {
		return Subscription{}, err
	}
	if ch.Entitlement != nil && s.entitle != nil {
		if err := s.entitle(cloneSub(next), *ch.Entitlement, next.UpdatedAt); err != nil {
			return Subscription{}, err
		}
	}
	s.subs[id] = cloneSub(next)
	s.events = append(s.events, ch.Events...)
	if ch.Audit != nil {
		s.audit.Append(*ch.Audit)
	}
	if ch.Outbox != "" {
		s.emit(ch.Outbox, id, next.UpdatedAt)
	}
	return cloneSub(next), nil
}

// Events implements Store.
func (s *MemoryStore) Events(_ context.Context, id uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Event{}
	for _, e := range s.events {
		if e.SubscriptionID == id {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// ExpireDue implements Store.
func (s *MemoryStore) ExpireDue(_ context.Context, now time.Time) ([]Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Subscription
	for id, x := range s.subs {
		if x.Status != StatusActive || x.EndsAt == nil || now.Before(*x.EndsAt) {
			continue
		}
		at := now
		x.Status, x.ExpiredAt, x.UpdatedAt = StatusExpired, &at, now
		s.subs[id] = x
		s.events = append(s.events, NewEvent(id, ActionExpired, ActorSystem, uuid.Nil, "", "", now))
		s.emit(EventExpired, id, now)
		out = append(out, cloneSub(x))
	}
	newestFirst(out)
	return out, nil
}

// ActiveListings implements Store.
func (s *MemoryStore) ActiveListings(_ context.Context, kind string, subscriberID uuid.UUID, now time.Time) (int, error) {
	s.mu.Lock()
	count := s.activeListings
	s.mu.Unlock()
	if count == nil {
		return 0, nil
	}
	return count(kind, subscriberID, now), nil
}

// CancelForDeletedUser cancels the person's own pending and active
// subscriptions (their account was deleted) and reports how many. In
// Postgres the accounts module does this in its own transaction.
func (s *MemoryStore) CancelForDeletedUser(userID uuid.UUID, at time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, x := range s.subs {
		if x.SubscriberKind != KindIndividual || x.SubscriberID != userID || (x.Status != StatusPendingPayment && x.Status != StatusActive) {
			continue
		}
		t := at
		x.Status, x.CancelledAt, x.CancelReason, x.CancelledBy, x.UpdatedAt = StatusCancelled, &t, "The account was deleted.", ActorSystem, at
		s.subs[id] = x
		s.events = append(s.events, NewEvent(id, ActionCancelled, ActorSystem, uuid.Nil, "", "The account was deleted.", at))
		n++
	}
	return n
}
