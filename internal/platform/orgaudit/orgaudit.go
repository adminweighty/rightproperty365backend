// Package orgaudit is the durable audit trail of an organisation's team:
// who invited whom, who accepted, who decided a join request, who changed a
// member's role or status, who removed a member, who assigned a request.
//
// Unlike the log-only audit lines elsewhere (audit=true in the logs), these
// records are kept in the database (organisation_audit_events, migration
// 0021) and are shown to the organisation's OWNER and MANAGER through
// GET /v1/agent/organisations/{id}/audit. They are written in the same
// transaction as the change they describe, by whichever module makes it.
//
// A record never carries a password, a token, a document or a customer's
// contact details: an actor, a target named by its ID and a short label
// (a person's name, an invited email address, a request reference), and the
// before / after values of what changed.
package orgaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Actions.
const (
	ActionInvitationCreated          = "INVITATION_CREATED"
	ActionInvitationCancelled        = "INVITATION_CANCELLED"
	ActionInvitationAccepted         = "INVITATION_ACCEPTED"
	ActionJoinRequestApproved        = "JOIN_REQUEST_APPROVED"
	ActionJoinRequestDeclined        = "JOIN_REQUEST_DECLINED"
	ActionJoinRequestChangesRequired = "JOIN_REQUEST_CHANGES_REQUESTED"
	ActionMemberRoleChanged          = "MEMBER_ROLE_CHANGED"
	ActionMemberJobTitleChanged      = "MEMBER_JOB_TITLE_CHANGED"
	ActionMemberSuspended            = "MEMBER_SUSPENDED"
	ActionMemberReactivated          = "MEMBER_REACTIVATED"
	ActionMemberRemoved              = "MEMBER_REMOVED"
	ActionRequestAssigned            = "REQUEST_ASSIGNED"
	ActionRequestUnassigned          = "REQUEST_UNASSIGNED"
	// The contact book (API 0.31.0).
	ActionContactOwnerChanged = "CONTACT_OWNER_CHANGED"
	ActionContactsMerged      = "CONTACTS_MERGED"
	ActionContactsImported    = "CONTACTS_IMPORTED"
	ActionContactsExported    = "CONTACTS_EXPORTED"
)

var actionLabels = map[string]string{
	ActionInvitationCreated:          "Invitation sent",
	ActionInvitationCancelled:        "Invitation cancelled",
	ActionInvitationAccepted:         "Invitation accepted",
	ActionJoinRequestApproved:        "Join request approved",
	ActionJoinRequestDeclined:        "Join request declined",
	ActionJoinRequestChangesRequired: "Join request sent back for changes",
	ActionMemberRoleChanged:          "Role changed",
	ActionMemberJobTitleChanged:      "Job title changed",
	ActionMemberSuspended:            "Member suspended",
	ActionMemberReactivated:          "Member reactivated",
	ActionMemberRemoved:              "Member removed",
	ActionRequestAssigned:            "Request assigned",
	ActionRequestUnassigned:          "Request returned to the organisation inbox",
	ActionContactOwnerChanged:        "Contact reassigned",
	ActionContactsMerged:             "Contacts merged",
	ActionContactsImported:           "Contacts imported",
	ActionContactsExported:           "Contacts exported",
}

// Actions returns every action, in a stable order.
func Actions() []string {
	out := make([]string, 0, len(actionLabels))
	for a := range actionLabels {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// ActionLabel is the wording shown for an action.
func ActionLabel(action string) string {
	if l, ok := actionLabels[action]; ok {
		return l
	}
	return action
}

// Target types.
const (
	TargetMember          = "MEMBER"
	TargetInvitation      = "INVITATION"
	TargetJoinRequest     = "JOIN_REQUEST"
	TargetPropertyRequest = "PROPERTY_REQUEST"
	// TargetContact is one contact (labelled with its name); TargetContactBook
	// the whole book (an import or an export; its ID is the organisation's).
	TargetContact     = "CONTACT"
	TargetContactBook = "CONTACT_BOOK"
)

// Event is one audit record.
type Event struct {
	ID             uuid.UUID
	OrganisationID uuid.UUID
	// ActorUserID is who acted (nil for the system); ActorName their name
	// at the time, so the record stays readable after they leave.
	ActorUserID *uuid.UUID
	ActorName   string
	Action      string
	TargetType  string
	TargetID    uuid.UUID
	// TargetLabel names the target: a person's name, the invited email
	// address, a request's reference.
	TargetLabel string
	// Before and After are the values that changed (nil when not applicable).
	Before map[string]any
	After  map[string]any
	At     time.Time
}

// New returns an event with a fresh ID.
func New(organisationID uuid.UUID, actorID uuid.UUID, actorName, action, targetType string, targetID uuid.UUID, targetLabel string, before, after map[string]any, at time.Time) Event {
	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}
	var actor *uuid.UUID
	if actorID != uuid.Nil {
		a := actorID
		actor = &a
	}
	return Event{
		ID: id, OrganisationID: organisationID, ActorUserID: actor, ActorName: actorName, Action: action,
		TargetType: targetType, TargetID: targetID, TargetLabel: targetLabel, Before: before, After: after, At: at,
	}
}

// Sink receives events written by an in-memory store (development and tests).
type Sink func(Event)

// Execer is a pgx pool or transaction.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func encode(m map[string]any) (*string, error) {
	if m == nil {
		return nil, nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	s := string(raw)
	return &s, nil
}

// Insert writes the event (inside the caller's transaction).
func Insert(ctx context.Context, q Execer, e Event) error {
	before, err := encode(e.Before)
	if err != nil {
		return err
	}
	after, err := encode(e.After)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO organisation_audit_events (id, organisation_id, actor_user_id, actor_name, action, target_type, target_id,
			target_label, before_state, after_state, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::jsonb, $11)`,
		e.ID, e.OrganisationID, e.ActorUserID, e.ActorName, e.Action, e.TargetType, e.TargetID,
		e.TargetLabel, before, after, e.At); err != nil {
		return fmt.Errorf("insert organisation audit event: %w", err)
	}
	return nil
}

// Querier reads events.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// List returns one page of an organisation's events, newest first, and the total.
func List(ctx context.Context, q Querier, organisationID uuid.UUID, limit, offset int) ([]Event, int, error) {
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM organisation_audit_events WHERE organisation_id = $1`, organisationID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count organisation audit events: %w", err)
	}
	rows, err := q.Query(ctx, `
		SELECT id, organisation_id, actor_user_id, actor_name, action, target_type, target_id, target_label,
		       before_state, after_state, created_at
		FROM organisation_audit_events WHERE organisation_id = $1
		ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, organisationID, limit, max(offset, 0))
	if err != nil {
		return nil, 0, fmt.Errorf("list organisation audit events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		var before, after []byte
		if err := row.Scan(&e.ID, &e.OrganisationID, &e.ActorUserID, &e.ActorName, &e.Action, &e.TargetType, &e.TargetID,
			&e.TargetLabel, &before, &after, &e.At); err != nil {
			return Event{}, err
		}
		if len(before) > 0 {
			if err := json.Unmarshal(before, &e.Before); err != nil {
				return Event{}, err
			}
		}
		if len(after) > 0 {
			if err := json.Unmarshal(after, &e.After); err != nil {
				return Event{}, err
			}
		}
		e.At = e.At.UTC()
		return e, nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list organisation audit events: %w", err)
	}
	if out == nil {
		out = []Event{}
	}
	return out, total, nil
}

// MemoryLog keeps events in memory (development and tests).
type MemoryLog struct {
	mu     sync.Mutex
	events []Event
}

// Append adds an event. It is a Sink.
func (l *MemoryLog) Append(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

// List returns one page of an organisation's events, newest first, and the total.
func (l *MemoryLog) List(organisationID uuid.UUID, limit, offset int) ([]Event, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.events {
		if e.OrganisationID == organisationID {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	total := len(out)
	start := min(max(offset, 0), total)
	end := total
	if limit > 0 {
		end = min(start+limit, total)
	}
	return append([]Event{}, out[start:end]...), total
}

// All returns a copy of every event (test helper).
func (l *MemoryLog) All() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.events...)
}
