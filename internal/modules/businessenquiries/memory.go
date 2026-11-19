package businessenquiries

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore is a mutex-guarded in-memory Store for development and tests
// only. Data is lost on restart and is not shared between processes (the
// worker never sees its outbox). It starts empty; development profiles come
// from internal/devseed.
type MemoryStore struct {
	mu        sync.Mutex
	profiles  map[string]Profile // key: kind + "/" + slug
	byKey     map[string]Record
	refs      map[string]struct{}
	enquiries []NewEnquiry
	timeline  []MemoryTimelineEntry
	activity  []MemoryActivityEntry
	outbox    []MemoryOutboxEvent
	sink      outbox.Sink
}

// SetOutboxSink registers a receiver for events as they are written
// (development without a database). The sink must not call back into the
// store synchronously.
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// MemoryTimelineEntry mirrors a business_enquiry_timeline row.
type MemoryTimelineEntry struct {
	EnquiryID uuid.UUID
	EventType string
	ActorType string
	CreatedAt time.Time
}

// MemoryActivityEntry mirrors a customer_activity row.
type MemoryActivityEntry struct {
	EnquiryID    uuid.UUID
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

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		profiles: make(map[string]Profile),
		byKey:    make(map[string]Record),
		refs:     make(map[string]struct{}),
	}
}

// PutProfile adds or replaces a profile (seed and test helper).
func (s *MemoryStore) PutProfile(p Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles[p.Kind+"/"+p.Slug] = p
}

// GetProfile implements Store.
func (s *MemoryStore) GetProfile(_ context.Context, kind, slug string) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[kind+"/"+slug]
	if !ok {
		return Profile{}, ErrRecipientNotFound
	}
	return p, nil
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
func (s *MemoryStore) Create(_ context.Context, enq NewEnquiry) (Record, bool, error) {
	payload, err := outboxPayload(enq)
	if err != nil {
		return Record{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.byKey[enq.IdempotencyKey]; ok {
		return existing, false, nil
	}
	if _, taken := s.refs[enq.Reference]; taken {
		return Record{}, false, ErrReferenceCollision
	}

	rec := Record{ID: enq.ID, Reference: enq.Reference, Status: enq.Status, RequestHash: enq.RequestHash, CreatedAt: enq.CreatedAt}
	s.byKey[enq.IdempotencyKey] = rec
	s.refs[enq.Reference] = struct{}{}
	s.enquiries = append(s.enquiries, enq)
	for _, ev := range timelineFor(enq) {
		s.timeline = append(s.timeline, MemoryTimelineEntry{EnquiryID: enq.ID, EventType: ev.EventType, ActorType: ev.ActorType, CreatedAt: enq.CreatedAt})
	}
	activity := MemoryActivityEntry{EnquiryID: enq.ID, ActivityType: ActivityEnquirySubmitted, CreatedAt: enq.CreatedAt}
	if enq.CustomerUserID != nil {
		activity.UserID = enq.CustomerUserID
	} else {
		activity.GuestEmail = enq.GuestEmail
	}
	s.activity = append(s.activity, activity)
	s.outbox = append(s.outbox, MemoryOutboxEvent{
		EventType: EventBusinessEnquiryCreated, AggregateID: enq.ID, Payload: payload, CreatedAt: enq.CreatedAt,
	})
	if s.sink != nil {
		s.sink(outbox.Event{
			ID: uuid.New(), Type: EventBusinessEnquiryCreated, AggregateType: AggregateBusinessEnquiry, AggregateID: enq.ID,
			Payload: payload, Attempts: 1, CreatedAt: enq.CreatedAt,
		})
	}
	return rec, true, nil
}

// visible is the inbox rule of InboxScope (routing exceptions are in
// nobody's inbox).
func visible(scope InboxScope, enq NewEnquiry) bool {
	if enq.Recipient == nil {
		return false
	}
	org, addressee := enq.Recipient.OrganisationID, enq.Recipient.UserID
	return slices.Contains(scope.ManagedOrganisationIDs, org) ||
		(slices.Contains(scope.MemberOrganisationIDs, org) && addressee != nil && *addressee == scope.UserID)
}

func inboxItem(enq NewEnquiry) InboxItem {
	item := InboxItem{
		EnquiryID: enq.ID, Reference: enq.Reference,
		Recipient:              InboxRecipient{Kind: enq.ProfileKind, Slug: enq.ProfileSlug, Name: enq.ProfileName},
		Message:                enq.Message,
		PreferredContactMethod: enq.PreferredContactMethod,
		Contact:                InboxContact{Name: enq.GuestName, Email: enq.GuestEmail},
		Status:                 enq.Status,
		CreatedAt:              enq.CreatedAt,
	}
	if enq.Service != "" {
		service := enq.Service
		item.Service = &service
	}
	if enq.GuestPhone != "" {
		phone := enq.GuestPhone
		item.Contact.Phone = &phone
	}
	if enq.RepliedAt != nil {
		at := enq.RepliedAt.UTC()
		item.RepliedAt = &at
	}
	return item
}

// ListInbox implements Store with the same visibility rule as the Postgres query.
func (s *MemoryStore) ListInbox(_ context.Context, scope InboxScope) ([]InboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	items := []InboxItem{}
	for _, enq := range s.enquiries {
		if visible(scope, enq) {
			items = append(items, inboxItem(enq))
		}
	}
	// Newest first; UUIDv7 IDs break ties in creation order.
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].EnquiryID.String() > items[j].EnquiryID.String()
	})
	if scope.Limit > 0 && len(items) > scope.Limit {
		items = items[:scope.Limit]
	}
	return items, nil
}

// MarkReplied implements Store.
func (s *MemoryStore) MarkReplied(_ context.Context, scope InboxScope, id, by uuid.UUID, at time.Time) (InboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.enquiries {
		enq := &s.enquiries[i]
		if enq.ID != id || !visible(scope, *enq) {
			continue
		}
		if enq.RepliedAt == nil {
			at, who := at.UTC(), by
			enq.RepliedAt, enq.RepliedBy = &at, &who
			s.timeline = append(s.timeline, MemoryTimelineEntry{EnquiryID: id, EventType: TimelineReplied, ActorType: "AGENT", CreatedAt: at})
		}
		return inboxItem(*enq), nil
	}
	return InboxItem{}, ErrNotFound
}

func customerEnquiry(enq NewEnquiry) CustomerEnquiry {
	out := CustomerEnquiry{ID: enq.ID, Reference: enq.Reference, ProfileID: enq.ProfileID, PrivacyVersion: enq.PrivacyVersion,
		CreatedAt: enq.CreatedAt.UTC()}
	if enq.CustomerUserID != nil {
		u := *enq.CustomerUserID
		out.CustomerUserID = &u
	}
	if enq.RepliedAt != nil {
		at := enq.RepliedAt.UTC()
		out.RepliedAt = &at
	}
	return out
}

// Enquiry implements Store.
func (s *MemoryStore) Enquiry(_ context.Context, id uuid.UUID) (CustomerEnquiry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, enq := range s.enquiries {
		if enq.ID == id {
			return customerEnquiry(enq), nil
		}
	}
	return CustomerEnquiry{}, ErrNotFound
}

// CustomerEnquiries implements Store.
func (s *MemoryStore) CustomerEnquiries(_ context.Context, userID, profileID uuid.UUID) ([]CustomerEnquiry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []CustomerEnquiry{}
	for _, enq := range s.enquiries {
		if enq.CustomerUserID != nil && *enq.CustomerUserID == userID && enq.ProfileID == profileID {
			out = append(out, customerEnquiry(enq))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out, nil
}

// ForgetUser unlinks a deleted account as the person who marked enquiries
// replied (account deletion; the enquiries themselves are the customer's).
func (s *MemoryStore) ForgetUser(userID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.enquiries {
		if s.enquiries[i].RepliedBy != nil && *s.enquiries[i].RepliedBy == userID {
			s.enquiries[i].RepliedBy = nil
		}
	}
}

// Enquiries returns a copy of the stored enquiries (test helper).
func (s *MemoryStore) Enquiries() []NewEnquiry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]NewEnquiry(nil), s.enquiries...)
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

// SetAgentEligible marks whether the user can receive enquiries addressed to
// their agent profiles of the organisation. Postgres reads this from the
// membership on every enquiry; the memory store keeps it on the profile, so
// the team store updates it when a member is suspended, reactivated,
// removed or joins.
func (s *MemoryStore) SetAgentEligible(organisationID, userID uuid.UUID, eligible bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.profiles {
		if p.OrganisationID == organisationID && p.AgentUserID != nil && *p.AgentUserID == userID {
			p.AgentEligible = eligible
			s.profiles[k] = p
		}
	}
}

// EnquiryRow is one enquiry as the reports count it.
type EnquiryRow struct {
	ID        uuid.UUID
	MarketID  uuid.UUID
	Status    string
	CreatedAt time.Time
}

// ReportRows returns every enquiry with its status and creation time (reports).
func (s *MemoryStore) ReportRows() []EnquiryRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make(map[uuid.UUID]Record, len(s.byKey))
	for _, r := range s.byKey {
		records[r.ID] = r
	}
	out := make([]EnquiryRow, 0, len(s.enquiries))
	for _, e := range s.enquiries {
		r := records[e.ID]
		out = append(out, EnquiryRow{ID: e.ID, MarketID: e.MarketID, Status: r.Status, CreatedAt: r.CreatedAt})
	}
	return out
}
