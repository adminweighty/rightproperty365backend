package listings

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file is what account administration (internal/modules/accounts) does
// to the listings of a person, in Postgres inside the caller's transaction
// and in memory on this store:
//
//   - Refresh: a direct owner's account was suspended or reactivated. Nothing
//     about the listings changes (they stay PUBLISHED): the public rule
//     (advertiserVerifiedCondition) already requires the owner's account to
//     be ACTIVE, so they leave and come back to the public catalogue with the
//     account. Each is touched and the search index is told, as an approval
//     of the owner's role does (onboarding).
//   - Withdraw: a direct owner's account was deleted. Their listings are
//     ARCHIVED, exactly as if they had archived each themselves.
//   - Unassign: an agent's account was deleted. The organisation's listings
//     they were the assigned agent of stay with the organisation, without an
//     agent.

// withdrawable are the statuses Archive accepts.
var withdrawable = []string{StatusDraft, StatusPendingReview, StatusChangesRequested, StatusPublished, StatusExpired, StatusPaused}

func listingEvent(id uuid.UUID) []byte {
	raw, _ := json.Marshal(map[string]string{"listing_id": id.String()})
	return raw
}

// RefreshOwnerListingsTx touches every listing the user owns directly (no
// organisation) and writes a LISTING_UPDATED event for each.
func RefreshOwnerListingsTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, at time.Time) (int, error) {
	tag, err := tx.Exec(ctx, `
		WITH touched AS (UPDATE listings SET updated_at = $2 WHERE seller_user_id = $1 AND organisation_id IS NULL RETURNING id)
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		SELECT gen_random_uuid(), $3, $4, t.id, jsonb_build_object('listing_id', t.id), $2, $2 FROM touched t`,
		userID, at, EventListingUpdated, AggregateListing)
	if err != nil {
		return 0, fmt.Errorf("refresh the owner's listings: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// WithdrawOwnerListingsTx archives every listing the user owns directly that
// can be archived, with a timeline entry (actorKind ActorAdvertiser for a
// self-deletion, ActorReviewer for an administrator) and the index event.
func WithdrawOwnerListingsTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, actorID *uuid.UUID, actorKind, reason string, at time.Time) (int, error) {
	rows, err := tx.Query(ctx, `
		UPDATE listings l SET status = 'ARCHIVED', revision = NULL, revision_status = NULL, updated_at = $2
		FROM (SELECT id, status::text AS old_status FROM listings
		      WHERE seller_user_id = $1 AND organisation_id IS NULL AND status::text = ANY($3) FOR UPDATE) old
		WHERE l.id = old.id
		RETURNING l.id, CASE WHEN old.old_status = 'PUBLISHED' AND l.expires_at <= $2 THEN 'EXPIRED' ELSE old.old_status END`,
		userID, at, withdrawable)
	if err != nil {
		return 0, fmt.Errorf("withdraw the owner's listings: %w", err)
	}
	type withdrawn struct {
		id   uuid.UUID
		from string
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (withdrawn, error) {
		var w withdrawn
		err := row.Scan(&w.id, &w.from)
		return w, err
	})
	if err != nil {
		return 0, fmt.Errorf("withdraw the owner's listings: %w", err)
	}
	for _, w := range list {
		ch := Change{Entry: &TimelineEntry{ID: uuid.New(), ListingID: w.id, ActorUserID: actorID, ActorKind: actorKind, Action: ActionArchived,
			FromStatus: w.from, ToStatus: StatusArchived, Reason: reason, CreatedAt: at},
			Events: []ChangeEvent{{Type: EventListingUnpublished, Payload: listingEvent(w.id)}}}
		if err := writeChange(ctx, tx, w.id, ch, at); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

// UnassignAgentTx removes the user as the assigned agent of every listing
// and writes a LISTING_UPDATED event for each.
func UnassignAgentTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, at time.Time) (int, error) {
	tag, err := tx.Exec(ctx, `
		WITH touched AS (UPDATE listings SET agent_user_id = NULL, updated_at = $2 WHERE agent_user_id = $1 RETURNING id)
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		SELECT gen_random_uuid(), $3, $4, t.id, jsonb_build_object('listing_id', t.id), $2, $2 FROM touched t`,
		userID, at, EventListingUpdated, AggregateListing)
	if err != nil {
		return 0, fmt.Errorf("unassign the agent's listings: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// UserListingCounts counts, by status, the listings a user owns directly or
// is the assigned agent of. A PUBLISHED listing past its expiry is EXPIRED.
func UserListingCounts(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, userID uuid.UUID, now time.Time) (map[string]int, error) {
	rows, err := q.Query(ctx, `
		SELECT CASE WHEN status = 'PUBLISHED' AND expires_at <= $2 THEN 'EXPIRED' ELSE status::text END, count(*)
		FROM listings WHERE (seller_user_id = $1 AND organisation_id IS NULL) OR agent_user_id = $1
		GROUP BY 1`, userID, now)
	if err != nil {
		return nil, fmt.Errorf("count the user's listings: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] += n
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// The in-memory counterparts
// ---------------------------------------------------------------------------

func ownedDirectly(r Record, userID uuid.UUID) bool {
	return r.OrganisationID == nil && r.SellerUserID != nil && *r.SellerUserID == userID
}

// RefreshOwnerListings is RefreshOwnerListingsTx on the memory store.
func (s *MemoryStore) RefreshOwnerListings(userID uuid.UUID, at time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.records {
		if !ownedDirectly(s.records[i], userID) {
			continue
		}
		s.records[i].UpdatedAt = at
		s.record(s.records[i].ID, Change{Events: []ChangeEvent{indexEvent(EventListingUpdated, s.records[i].ID)}}, at)
		n++
	}
	return n
}

// WithdrawOwnerListings is WithdrawOwnerListingsTx on the memory store.
func (s *MemoryStore) WithdrawOwnerListings(userID uuid.UUID, actorID *uuid.UUID, actorKind, reason string, at time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.records {
		r := s.records[i]
		if !ownedDirectly(r, userID) || !slices.Contains(withdrawable, r.Status) {
			continue
		}
		from := r.Status
		if from == StatusPublished && !r.ExpiresAt.After(at) {
			from = StatusExpired
		}
		s.records[i].Status, s.records[i].UpdatedAt = StatusArchived, at
		if meta := s.meta[r.ID]; meta != nil {
			meta.revision = nil
		}
		s.record(r.ID, Change{Entry: &TimelineEntry{ID: uuid.New(), ListingID: r.ID, ActorUserID: actorID, ActorKind: actorKind, Action: ActionArchived,
			FromStatus: from, ToStatus: StatusArchived, Reason: reason, CreatedAt: at},
			Events: []ChangeEvent{indexEvent(EventListingUnpublished, r.ID)}}, at)
		n++
	}
	return n
}

// UnassignAgent is UnassignAgentTx on the memory store.
func (s *MemoryStore) UnassignAgent(userID uuid.UUID, at time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.records {
		if s.records[i].AgentUserID == nil || *s.records[i].AgentUserID != userID {
			continue
		}
		s.records[i].AgentUserID, s.records[i].UpdatedAt = nil, at
		s.record(s.records[i].ID, Change{Events: []ChangeEvent{indexEvent(EventListingUpdated, s.records[i].ID)}}, at)
		n++
	}
	return n
}

// UserListingCounts is UserListingCounts on the memory store.
func (s *MemoryStore) UserListingCounts(userID uuid.UUID, now time.Time) map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{}
	for _, r := range s.records {
		if !ownedDirectly(r, userID) && (r.AgentUserID == nil || *r.AgentUserID != userID) {
			continue
		}
		status := r.Status
		if status == StatusPublished && !r.ExpiresAt.After(now) {
			status = StatusExpired
		}
		out[status]++
	}
	return out
}

// ReportRow is one listing as the reports and the CSV export see it.
type ReportRow struct {
	ID             uuid.UUID
	Reference      string
	Title          string
	Status         string
	MarketCode     string
	Deal           string
	Category       string
	PropertyType   string
	Price          string
	Currency       string
	OrganisationID *uuid.UUID
	AgentUserID    *uuid.UUID
	SellerUserID   *uuid.UUID
	CreatedAt      time.Time
	PublishedAt    *time.Time
	ExpiresAt      *time.Time
}

// ReportRows returns every listing of the memory store (reports).
func (s *MemoryStore) ReportRows(now time.Time) []ReportRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ReportRow, 0, len(s.records))
	for _, r := range s.records {
		status := r.Status
		if status == StatusPublished && !r.ExpiresAt.After(now) {
			status = StatusExpired
		}
		row := ReportRow{ID: r.ID, Reference: r.Reference, Title: r.Title, Status: status, MarketCode: r.MarketCode, Deal: r.Deal,
			Category: r.Category, PropertyType: r.PropertyType, Price: r.Price.Amount, Currency: r.Price.Currency,
			OrganisationID: r.OrganisationID, AgentUserID: r.AgentUserID, SellerUserID: r.SellerUserID, CreatedAt: r.UpdatedAt}
		if meta := s.meta[r.ID]; meta != nil && !meta.createdAt.IsZero() {
			row.CreatedAt = meta.createdAt
		}
		if !r.PublishedAt.IsZero() {
			p := r.PublishedAt
			row.PublishedAt = &p
		}
		if !r.ExpiresAt.IsZero() {
			e := r.ExpiresAt
			row.ExpiresAt = &e
		}
		out = append(out, row)
	}
	return out
}
