package propertyrequests

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/markets"
)

// Fixed IDs used by the development seed. Listing IDs match the frontend's
// sample data.
var (
	SeedOrganisationID = uuid.MustParse("0195c0de-0000-7000-8000-00000000b001")
	SeedSellerUserID   = uuid.MustParse("0195c0de-0000-7000-8000-00000000c001")
	SeedExpiredListing = SeedListingID(99)
)

// SeedListingID returns the ID of development listing n (1..6, or 99).
func SeedListingID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0195c0de-0000-7000-8000-%012d", n))
}

// MemoryStore is a mutex-guarded in-memory Store for development and tests
// only. Data is lost on restart and is not shared between processes (the
// worker never sees its outbox).
type MemoryStore struct {
	mu       sync.Mutex
	listings map[uuid.UUID]Listing
	byKey    map[string]Record
	refs     map[string]struct{}
	requests []NewRequest
	timeline []MemoryTimelineEntry
	activity []MemoryActivityEntry
	outbox   []MemoryOutboxEvent
}

// MemoryTimelineEntry mirrors a request_timeline row.
type MemoryTimelineEntry struct {
	RequestID uuid.UUID
	EventType string
	ActorType string
	CreatedAt time.Time
}

// MemoryActivityEntry mirrors a customer_activity row.
type MemoryActivityEntry struct {
	RequestID    uuid.UUID
	ListingID    uuid.UUID
	ActivityType string
	UserID       *uuid.UUID // set for an authenticated customer
	GuestEmail   string     // set for a guest
	CreatedAt    time.Time
}

// MemoryOutboxEvent mirrors an outbox_events row.
type MemoryOutboxEvent struct {
	EventType   string
	AggregateID uuid.UUID
	Payload     []byte
	CreatedAt   time.Time
}

// NewMemoryStore returns a store holding a small built-in fixture: six
// PUBLISHED Zimbabwe listings (1-5 owned by an organisation with no assigned
// agent, 6 by a direct seller) and one EXPIRED listing. In development the API
// replaces these with the full sample data set from internal/devseed (18
// listings, each with a named agent or seller).
func NewMemoryStore() *MemoryStore {
	s := &MemoryStore{
		listings: make(map[uuid.UUID]Listing),
		byKey:    make(map[string]Record),
		refs:     make(map[string]struct{}),
	}
	org, seller := SeedOrganisationID, SeedSellerUserID
	for n := 1; n <= 5; n++ {
		s.PutListing(Listing{
			ID: SeedListingID(n), MarketID: markets.ZimbabweID, Status: ListingStatusPublished,
			OrganisationID: &org, OrganisationEligible: true,
		})
	}
	s.PutListing(Listing{
		ID: SeedListingID(6), MarketID: markets.ZimbabweID, Status: ListingStatusPublished,
		SellerUserID: &seller, SellerEligible: true,
	})
	expiredAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.PutListing(Listing{
		ID: SeedExpiredListing, MarketID: markets.ZimbabweID, Status: "EXPIRED", ExpiresAt: &expiredAt,
		OrganisationID: &org, OrganisationEligible: true,
	})
	return s
}

// PutListing adds or replaces a listing (seed and test helper).
func (s *MemoryStore) PutListing(l Listing) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listings[l.ID] = l
}

// GetListing implements Store.
func (s *MemoryStore) GetListing(_ context.Context, id uuid.UUID) (Listing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.listings[id]
	if !ok {
		return Listing{}, ErrListingNotFound
	}
	return l, nil
}

// FindByIdempotencyKey implements Store.
func (s *MemoryStore) FindByIdempotencyKey(_ context.Context, key string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byKey[key]
	if !ok {
		return Record{}, ErrNotFound
	}
	return rec, nil
}

// Create implements Store. Holding the mutex for the whole call gives the same
// all-or-nothing behaviour as the Postgres transaction.
func (s *MemoryStore) Create(_ context.Context, req NewRequest) (Record, bool, error) {
	payload, err := outboxPayload(req)
	if err != nil {
		return Record{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.byKey[req.IdempotencyKey]; ok {
		return existing, false, nil
	}
	if _, taken := s.refs[req.Reference]; taken {
		return Record{}, false, ErrReferenceCollision
	}

	rec := Record{ID: req.ID, Reference: req.Reference, Status: req.Status, RequestHash: req.RequestHash, CreatedAt: req.CreatedAt}
	s.byKey[req.IdempotencyKey] = rec
	s.refs[req.Reference] = struct{}{}
	s.requests = append(s.requests, req)
	for _, ev := range timelineFor(req) {
		s.timeline = append(s.timeline, MemoryTimelineEntry{RequestID: req.ID, EventType: ev.EventType, ActorType: ev.ActorType, CreatedAt: req.CreatedAt})
	}
	activity := MemoryActivityEntry{
		RequestID: req.ID, ListingID: req.ListingID, ActivityType: ActivityRequestSubmitted, CreatedAt: req.CreatedAt,
	}
	if req.CustomerUserID != nil {
		activity.UserID = req.CustomerUserID
	} else {
		activity.GuestEmail = req.GuestEmail
	}
	s.activity = append(s.activity, activity)
	s.outbox = append(s.outbox, MemoryOutboxEvent{
		EventType: EventPropertyRequestCreated, AggregateID: req.ID, Payload: payload, CreatedAt: req.CreatedAt,
	})
	return rec, true, nil
}

// ListInbox implements Store with the same visibility rule as the Postgres query.
func (s *MemoryStore) ListInbox(_ context.Context, scope InboxScope) ([]InboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	items := []InboxItem{}
	for _, req := range s.requests {
		if req.Recipient == nil {
			continue // routing exception: in nobody's inbox
		}
		assignee := req.Recipient.UserID
		visible := false
		if req.Recipient.OrganisationID != nil {
			org := *req.Recipient.OrganisationID
			visible = slices.Contains(scope.ManagedOrganisationIDs, org) ||
				(slices.Contains(scope.MemberOrganisationIDs, org) && (assignee == nil || *assignee == scope.UserID))
		}
		// Routed to the caller as the direct seller of the listing.
		if req.Recipient.Type == RecipientSeller && assignee != nil && *assignee == scope.UserID {
			visible = true
		}
		if !visible {
			continue
		}
		item := InboxItem{
			RequestID: req.ID, Reference: req.Reference, ListingID: req.ListingID,
			RequestType: req.RequestType, Status: req.Status, Message: req.Message,
			PreferredContactMethod: req.PreferredContactMethod,
			Contact:                InboxContact{Name: req.GuestName, Email: req.GuestEmail},
			CreatedAt:              req.CreatedAt,
		}
		if req.GuestPhone != "" {
			phone := req.GuestPhone
			item.Contact.Phone = &phone
		}
		items = append(items, item)
	}
	// Newest first; UUIDv7 IDs break ties in creation order.
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].RequestID.String() > items[j].RequestID.String()
	})
	if scope.Limit > 0 && len(items) > scope.Limit {
		items = items[:scope.Limit]
	}
	return items, nil
}

// IsDirectSeller implements Store.
func (s *MemoryStore) IsDirectSeller(_ context.Context, userID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.listings {
		if l.SellerUserID != nil && *l.SellerUserID == userID {
			return true, nil
		}
	}
	return false, nil
}

// Requests returns a copy of the stored requests (test helper).
func (s *MemoryStore) Requests() []NewRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]NewRequest(nil), s.requests...)
}

// Timeline returns a copy of the timeline entries (test helper).
func (s *MemoryStore) Timeline() []MemoryTimelineEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MemoryTimelineEntry(nil), s.timeline...)
}

// Activity returns a copy of the customer activity entries (test helper).
func (s *MemoryStore) Activity() []MemoryActivityEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MemoryActivityEntry(nil), s.activity...)
}

// Outbox returns a copy of the outbox events (test helper).
func (s *MemoryStore) Outbox() []MemoryOutboxEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MemoryOutboxEvent(nil), s.outbox...)
}
