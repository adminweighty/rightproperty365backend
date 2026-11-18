package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/saved"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// OutboxEventTypes are the outbox events the Recorder turns into
// server-side analytics events:
//
//	LISTING_SAVED            -> listing_save
//	PROPERTY_REQUEST_CREATED -> enquiry_submit (a request about a listing)
//	APPOINTMENT_REQUESTED    -> viewing_request (an appointment for a listing)
//	APPOINTMENT_CONFIRMED    -> appointment_confirm (for a listing)
//	LISTING_RELISTED         -> listing_relist
//	APPLICATION_SUBMITTED    -> application_submit (a first submission;
//	                            a resubmission after a request for more
//	                            information is not counted again)
func OutboxEventTypes() []string {
	return []string{saved.EventListingSaved, propertyrequests.EventPropertyRequestCreated, appointments.EventRequested,
		appointments.EventConfirmed, listings.EventListingRelisted, applications.EventTypeSubmitted}
}

// AppointmentListing returns the listing an appointment is about (nil for a
// consultation, or an appointment that does not exist).
type AppointmentListing func(ctx context.Context, appointmentID uuid.UUID) (*uuid.UUID, error)

// Recorder is the worker's outbox handler for OutboxEventTypes. It is
// idempotent: the analytics event_id is derived from the outbox event's ID.
type Recorder struct {
	svc         *Service
	appointment AppointmentListing
}

// NewRecorder builds the Recorder.
func NewRecorder(svc *Service, appointment AppointmentListing) *Recorder {
	return &Recorder{svc: svc, appointment: appointment}
}

// Register adds the Recorder to every event type it consumes, after any
// handler already registered (notifications).
func (r *Recorder) Register(d *outbox.Dispatcher) {
	for _, t := range OutboxEventTypes() {
		d.Also(t, outbox.HandlerFunc(r.Handle))
	}
}

// eventBody holds the fields of the payloads read here.
type eventBody struct {
	ListingID     *uuid.UUID `json:"listing_id"`
	AppointmentID *uuid.UUID `json:"appointment_id"`
	At            *time.Time `json:"at"`
	OccurredAt    *time.Time `json:"occurred_at"`
	CreatedAt     *time.Time `json:"created_at"`
	Resubmission  bool       `json:"resubmission"`
}

func (b eventBody) when(fallback time.Time) time.Time {
	for _, t := range []*time.Time{b.OccurredAt, b.At, b.CreatedAt} {
		if t != nil && !t.IsZero() {
			return *t
		}
	}
	return fallback
}

// Handle implements outbox.Handler.
func (r *Recorder) Handle(ctx context.Context, ev outbox.Event) error {
	var body eventBody
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &body); err != nil {
			return outbox.Permanent(fmt.Errorf("analytics: decode %s payload: %w", ev.Type, err))
		}
	}
	var eventType string
	listing := body.ListingID
	switch ev.Type {
	case saved.EventListingSaved:
		eventType = TypeListingSave
	case propertyrequests.EventPropertyRequestCreated:
		eventType = TypeEnquirySubmit
	case applications.EventTypeSubmitted:
		if body.Resubmission {
			return nil
		}
		eventType = TypeApplicationSubmit
	case listings.EventListingRelisted:
		eventType = TypeListingRelist
		if listing == nil {
			listing = &ev.AggregateID
		}
	case appointments.EventRequested, appointments.EventConfirmed:
		eventType = TypeViewingRequest
		if ev.Type == appointments.EventConfirmed {
			eventType = TypeAppointmentConfirm
		}
		id := ev.AggregateID
		if body.AppointmentID != nil {
			id = *body.AppointmentID
		}
		if r.appointment == nil {
			return nil
		}
		got, err := r.appointment(ctx, id)
		if err != nil {
			return fmt.Errorf("analytics: load appointment: %w", err)
		}
		listing = got
	default:
		return nil
	}
	if listing == nil || *listing == uuid.Nil {
		return nil // not about a listing: nothing to count
	}
	return r.svc.RecordServer(ctx, ServerEvent{SourceID: ev.ID, Type: eventType, ListingID: *listing, OccurredAt: body.when(ev.CreatedAt)})
}
