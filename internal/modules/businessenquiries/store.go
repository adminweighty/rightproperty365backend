package businessenquiries

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// TimelineReplied is the timeline row of an enquiry marked as replied to.
const TimelineReplied = "REPLIED"

// Names shared by both store implementations so the memory store mirrors
// exactly what Postgres persists.
const (
	EventBusinessEnquiryCreated = "BUSINESS_ENQUIRY_CREATED"
	AggregateBusinessEnquiry    = "business_enquiry"

	TimelineEnquiryReceived  = "ENQUIRY_RECEIVED"
	TimelineRoutingException = "ROUTING_EXCEPTION_RAISED"

	ActivityEnquirySubmitted = "BUSINESS_ENQUIRY_SUBMITTED"

	ActorCustomer = "CUSTOMER"
	ActorSystem   = "SYSTEM"
)

// Store persists business enquiries.
type Store interface {
	// GetProfile returns routing state for a profile or ErrRecipientNotFound.
	GetProfile(ctx context.Context, kind, slug string) (Profile, error)

	// FindByIdempotencyKey returns the enquiry stored under key or ErrNotFound.
	FindByIdempotencyKey(ctx context.Context, key string) (Record, error)

	// Create atomically persists the enquiry together with its timeline,
	// customer-activity and outbox records (one transaction in Postgres) and
	// returns only after they are durable.
	//
	// If an enquiry with the same idempotency key already exists (including
	// one committed concurrently), nothing is written and the existing record
	// is returned with created=false. ErrReferenceCollision means the
	// reference is taken and the caller should retry with a new one.
	Create(ctx context.Context, enq NewEnquiry) (rec Record, created bool, err error)

	// ListInbox returns the enquiries visible under scope (see InboxScope),
	// newest first, at most scope.Limit.
	ListInbox(ctx context.Context, scope InboxScope) ([]InboxItem, error)

	// MarkReplied records that the recipient replied to an enquiry visible
	// under scope (the first time only: a repeat keeps the first time) and
	// returns it as the inbox shows it; ErrNotFound when it is not visible.
	MarkReplied(ctx context.Context, scope InboxScope, id, by uuid.UUID, at time.Time) (InboxItem, error)

	// Enquiry returns what reviews need of one enquiry, or ErrNotFound.
	Enquiry(ctx context.Context, id uuid.UUID) (CustomerEnquiry, error)

	// CustomerEnquiries returns the enquiries a signed-in customer sent to a
	// profile, newest first.
	CustomerEnquiries(ctx context.Context, userID, profileID uuid.UUID) ([]CustomerEnquiry, error)
}

// timelineEvent is one row of business_enquiry_timeline.
type timelineEvent struct {
	EventType string
	ActorType string
}

// timelineFor lists the timeline rows written when an enquiry is created.
func timelineFor(enq NewEnquiry) []timelineEvent {
	events := []timelineEvent{{EventType: TimelineEnquiryReceived, ActorType: ActorCustomer}}
	if enq.Status == StatusRoutingException {
		events = append(events, timelineEvent{EventType: TimelineRoutingException, ActorType: ActorSystem})
	}
	return events
}

// outboxPayload is the BUSINESS_ENQUIRY_CREATED event body. It carries
// identifiers only: no name, email, phone, message or profile slug. Consumers
// load details by enquiry_id.
func outboxPayload(enq NewEnquiry) ([]byte, error) {
	payload := map[string]any{
		"enquiry_id":     enq.ID,
		"reference":      enq.Reference,
		"profile_id":     enq.ProfileID,
		"recipient_kind": enq.ProfileKind,
		"market_id":      enq.MarketID,
		"status":         enq.Status,
		"created_at":     enq.CreatedAt,
	}
	if enq.Development != nil {
		payload["development_id"] = enq.Development.ID
	}
	if enq.Recipient != nil {
		payload["recipient_organisation_id"] = enq.Recipient.OrganisationID
		if enq.Recipient.UserID != nil {
			payload["recipient_user_id"] = *enq.Recipient.UserID
		}
	}
	return json.Marshal(payload)
}
