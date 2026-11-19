// Package adminaudit is the durable audit trail of what platform
// administrators do: suspending, reactivating and deleting accounts,
// managing subscription plans, deciding subscriptions, changing reference
// data and moderating reviews of service providers.
//
// The records are kept in admin_audit_events (migration 0024), written in
// the same transaction as the change they describe, and are immutable: a row
// only ever loses its actor's user ID (the name is kept). They are read by
// platform administrators through GET /v1/admin/audit and the account detail.
//
// A record never carries a password, a token or a document: the actor, the
// target named by its ID and a short label (a person's name, a plan name, an
// invoice number, a reference code), the administrator's reason, and the
// before / after values of what changed.
package adminaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Actions.
const (
	ActionUserSuspended        = "USER_SUSPENDED"
	ActionUserReactivated      = "USER_REACTIVATED"
	ActionUserDeleted          = "USER_DELETED"
	ActionUserSelfDeleted      = "USER_SELF_DELETED"
	ActionPlanCreated          = "PLAN_CREATED"
	ActionPlanUpdated          = "PLAN_UPDATED"
	ActionPlanRetired          = "PLAN_RETIRED"
	ActionSubscriptionApproved = "SUBSCRIPTION_APPROVED"
	ActionSubscriptionRejected = "SUBSCRIPTION_REJECTED"
	ActionSubscriptionCanceled = "SUBSCRIPTION_CANCELLED"
	ActionReferenceCreated     = "REFERENCE_ITEM_CREATED"
	ActionReferenceUpdated     = "REFERENCE_ITEM_UPDATED"
	ActionReferenceDeactivated = "REFERENCE_ITEM_DEACTIVATED"
	ActionReferenceReactivated = "REFERENCE_ITEM_REACTIVATED"
	// Review moderation (API 0.36.4): a review of a service provider
	// published, hidden or rejected, or its provider's reply removed.
	ActionReviewPublished    = "REVIEW_PUBLISHED"
	ActionReviewHidden       = "REVIEW_HIDDEN"
	ActionReviewRejected     = "REVIEW_REJECTED"
	ActionReviewReplyRemoved = "REVIEW_REPLY_REMOVED"
)

// Target types.
const (
	TargetUser          = "USER"
	TargetPlan          = "SUBSCRIPTION_PLAN"
	TargetSubscription  = "SUBSCRIPTION"
	TargetReferenceItem = "REFERENCE_ITEM"
	// TargetProviderReview is one review of a service provider (labelled
	// with the provider's name).
	TargetProviderReview = "PROVIDER_REVIEW"
)

// Event is one audit record.
type Event struct {
	ID          uuid.UUID
	ActorUserID *uuid.UUID
	ActorName   string
	Action      string
	TargetType  string
	// TargetID is a UUID for users, plans and subscriptions and
	// "<set>/<code>" for a reference item.
	TargetID    string
	TargetLabel string
	Reason      string
	Before      map[string]any
	After       map[string]any
	At          time.Time
}

// New returns an event with a fresh ID.
func New(actorID uuid.UUID, actorName, action, targetType, targetID, targetLabel, reason string, before, after map[string]any, at time.Time) Event {
	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}
	var actor *uuid.UUID
	if actorID != uuid.Nil {
		a := actorID
		actor = &a
	}
	return Event{ID: id, ActorUserID: actor, ActorName: actorName, Action: action, TargetType: targetType, TargetID: targetID,
		TargetLabel: targetLabel, Reason: reason, Before: before, After: after, At: at.UTC()}
}

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
		INSERT INTO admin_audit_events (id, actor_user_id, actor_name, action, target_type, target_id, target_label, reason,
			before_state, after_state, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::jsonb, $11)`,
		e.ID, e.ActorUserID, e.ActorName, e.Action, e.TargetType, e.TargetID, e.TargetLabel, e.Reason, before, after, e.At); err != nil {
		return fmt.Errorf("insert admin audit event: %w", err)
	}
	return nil
}

// Filter selects events. Empty fields match everything.
type Filter struct {
	TargetType string
	TargetID   string
	ActorID    *uuid.UUID
	Action     string
	Limit      int
	Offset     int
}

// Querier reads events.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// List returns one page of events, newest first, and the total.
func List(ctx context.Context, q Querier, f Filter) ([]Event, int, error) {
	where := `($1 = '' OR target_type = $1) AND ($2 = '' OR target_id = $2) AND ($3::uuid IS NULL OR actor_user_id = $3) AND ($4 = '' OR action = $4)`
	args := []any{f.TargetType, f.TargetID, f.ActorID, f.Action}
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM admin_audit_events WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count admin audit events: %w", err)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
		SELECT id, actor_user_id, actor_name, action, target_type, target_id, target_label, reason, before_state, after_state, created_at
		FROM admin_audit_events WHERE `+where+`
		ORDER BY created_at DESC, id DESC LIMIT $5 OFFSET $6`, append(args, limit, max(f.Offset, 0))...)
	if err != nil {
		return nil, 0, fmt.Errorf("list admin audit events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		var before, after []byte
		if err := row.Scan(&e.ID, &e.ActorUserID, &e.ActorName, &e.Action, &e.TargetType, &e.TargetID, &e.TargetLabel, &e.Reason,
			&before, &after, &e.At); err != nil {
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
		return nil, 0, fmt.Errorf("list admin audit events: %w", err)
	}
	if out == nil {
		out = []Event{}
	}
	return out, total, nil
}

// Log reads the trail; both the Postgres reader and MemoryLog implement it.
type Log interface {
	List(ctx context.Context, f Filter) ([]Event, int, error)
}

// PostgresLog reads admin_audit_events.
type PostgresLog struct{ Q Querier }

// List implements Log.
func (l PostgresLog) List(ctx context.Context, f Filter) ([]Event, int, error) {
	return List(ctx, l.Q, f)
}

// MemoryLog keeps events in memory (development and tests). One log is
// shared by every in-memory store that writes administrator actions.
type MemoryLog struct {
	mu     sync.Mutex
	events []Event
}

// Append adds an event.
func (l *MemoryLog) Append(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

// List implements Log.
func (l *MemoryLog) List(_ context.Context, f Filter) ([]Event, int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.events {
		if (f.TargetType != "" && e.TargetType != f.TargetType) || (f.TargetID != "" && !strings.EqualFold(e.TargetID, f.TargetID)) ||
			(f.Action != "" && e.Action != f.Action) || (f.ActorID != nil && (e.ActorUserID == nil || *e.ActorUserID != *f.ActorID)) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	total := len(out)
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	start := min(max(f.Offset, 0), total)
	end := min(start+limit, total)
	return append([]Event{}, out[start:end]...), total, nil
}

// All returns a copy of every event (test helper).
func (l *MemoryLog) All() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.events...)
}
