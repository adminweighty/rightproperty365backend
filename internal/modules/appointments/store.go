package appointments

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Names shared by both store implementations.
const (
	AggregateAppointment = "appointment"

	EventRequested           = "APPOINTMENT_REQUESTED"
	EventConfirmed           = "APPOINTMENT_CONFIRMED"
	EventRescheduleRequested = "APPOINTMENT_RESCHEDULE_REQUESTED"
	EventCancelled           = "APPOINTMENT_CANCELLED"
	EventCompleted           = "APPOINTMENT_COMPLETED"
	EventNoShow              = "APPOINTMENT_NO_SHOW"
	EventAssigned            = "APPOINTMENT_ASSIGNED"
)

// EventTypes lists every outbox event this module writes.
func EventTypes() []string {
	return []string{EventRequested, EventConfirmed, EventRescheduleRequested, EventCancelled, EventCompleted, EventNoShow, EventAssigned}
}

// Store persists appointments.
type Store interface {
	// FindByIdempotencyKey returns the appointment created under key, or ErrNotFound.
	FindByIdempotencyKey(ctx context.Context, key string) (Appointment, error)

	// Create atomically persists the appointment, its first timeline entry
	// and the APPOINTMENT_REQUESTED outbox event. If an appointment with the
	// same (non-empty) idempotency key already exists, nothing is written and
	// it is returned with created=false. ErrReferenceCollision means the
	// reference is taken and the caller should retry with a new one.
	Create(ctx context.Context, a Appointment, entry TimelineEntry) (stored Appointment, created bool, err error)

	// Get returns the appointment, or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Appointment, error)
	// Timeline returns the appointment's entries, oldest first.
	Timeline(ctx context.Context, id uuid.UUID) ([]TimelineEntry, error)

	// Update changes one appointment atomically. It reads the current row
	// under a lock, calls decide with it, and writes the returned state, its
	// timeline entry and its outbox event together (one transaction in
	// Postgres). An error from decide is returned as is and nothing is
	// written. When the new state is CONFIRMED, the store checks — inside the
	// same critical section — that the professional (Appointment.BusyKey) has
	// no other CONFIRMED appointment overlapping it, and returns ErrSlotTaken
	// if one does. ErrNotFound when the appointment does not exist.
	Update(ctx context.Context, id uuid.UUID, decide func(current Appointment) (Mutation, error)) (Appointment, error)

	// List returns one page under the filter and the total number of matches.
	List(ctx context.Context, f ListFilter) ([]Appointment, int, error)
	// Count returns the badge counts under scope at now.
	Count(ctx context.Context, scope Scope, now time.Time) (Counts, error)
}

// eventPayload is the body of every appointment event. It carries identifiers,
// statuses and times only — no personal data; consumers load the rest by ID.
func eventPayload(a Appointment, e TimelineEntry) ([]byte, error) {
	payload := map[string]any{
		"appointment_id": a.ID,
		"reference":      a.Reference,
		"type":           a.Type,
		"event":          e.Event,
		"from":           e.FromStatus,
		"to":             e.ToStatus,
		"actor":          e.ActorType,
		"start_utc":      a.StartUTC,
		"occurred_at":    e.At,
	}
	if a.ProposedStartUTC != nil {
		payload["proposed_start_utc"] = *a.ProposedStartUTC
		payload["proposed_by"] = a.ProposedBy
	}
	if a.CancelledBy != "" {
		payload["cancelled_by"] = a.CancelledBy
	}
	return json.Marshal(payload)
}

// EventPayload is the decoded body of an appointment event.
type EventPayload struct {
	AppointmentID    uuid.UUID  `json:"appointment_id"`
	Event            string     `json:"event"`
	From             string     `json:"from"`
	To               string     `json:"to"`
	Actor            string     `json:"actor"`
	StartUTC         time.Time  `json:"start_utc"`
	ProposedStartUTC *time.Time `json:"proposed_start_utc"`
	ProposedBy       string     `json:"proposed_by"`
	CancelledBy      string     `json:"cancelled_by"`
	OccurredAt       time.Time  `json:"occurred_at"`
}
