package propertyrequests

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// Names shared by both store implementations so the memory store mirrors
// exactly what Postgres persists.
const (
	EventPropertyRequestCreated = "PROPERTY_REQUEST_CREATED"
	AggregatePropertyRequest    = "property_request"

	TimelineRequestReceived  = "REQUEST_RECEIVED"
	TimelineRoutingException = "ROUTING_EXCEPTION_RAISED"

	ActivityRequestSubmitted = "PROPERTY_REQUEST_SUBMITTED"

	ActorCustomer = "CUSTOMER"
	ActorSystem   = "SYSTEM"
)

// Store persists property requests.
type Store interface {
	// GetListing returns routing state for a listing or ErrListingNotFound.
	GetListing(ctx context.Context, id uuid.UUID) (Listing, error)

	// FindByIdempotencyKey returns the request stored under key or ErrNotFound.
	FindByIdempotencyKey(ctx context.Context, key string) (Record, error)

	// Create atomically persists the request together with its timeline,
	// customer-activity and outbox records (one transaction in Postgres) and
	// returns only after they are durable.
	//
	// If a request with the same idempotency key already exists (including one
	// committed concurrently), nothing is written and the existing record is
	// returned with created=false. ErrReferenceCollision means the reference
	// is taken and the caller should retry with a new one.
	Create(ctx context.Context, req NewRequest) (rec Record, created bool, err error)

	// WorkStore is everything that happens after a request was received:
	// the inbox, the lead stage, internal notes, the customer's own history
	// and the routing exceptions (see work.go).
	WorkStore

	// FunnelStore reads the cohort of a lead funnel report (funnel.go).
	FunnelStore

	// IsDirectSeller reports whether the user is the direct seller of at
	// least one listing (in any status).
	IsDirectSeller(ctx context.Context, userID uuid.UUID) (bool, error)
}

// timelineEvent is one row of request_timeline.
type timelineEvent struct {
	EventType string
	ActorType string
}

// timelineFor lists the timeline rows written when a request is created.
func timelineFor(req NewRequest) []timelineEvent {
	events := []timelineEvent{{EventType: TimelineRequestReceived, ActorType: ActorCustomer}}
	if req.Status == StatusRoutingException {
		events = append(events, timelineEvent{EventType: TimelineRoutingException, ActorType: ActorSystem})
	}
	return events
}

// outboxPayload is the PROPERTY_REQUEST_CREATED event body. It carries
// identifiers only (no personal data); consumers load details by request_id.
func outboxPayload(req NewRequest) ([]byte, error) {
	payload := map[string]any{
		"request_id":   req.ID,
		"reference":    req.Reference,
		"listing_id":   req.ListingID,
		"market_id":    req.MarketID,
		"request_type": req.RequestType,
		"status":       req.Status,
		"created_at":   req.CreatedAt,
	}
	if req.Recipient != nil {
		payload["recipient_type"] = req.Recipient.Type
	}
	return json.Marshal(payload)
}
