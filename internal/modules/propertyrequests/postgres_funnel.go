package propertyrequests

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The Postgres side of funnel.go. The cohort is read with the inbox's own
// WHERE clause (inboxWhere) plus the report's filters, and the stage history
// with one LEFT JOIN on request_timeline (index request_timeline_request_idx).
// The counting itself is ComputeFunnel, shared with the memory store.

// funnelWhere is the WHERE clause of a report's cohort and its arguments.
func funnelWhere(f FunnelFilter) (string, []any) {
	where, args := inboxWhere(f.inbox())
	add := func(clause string, arg any) {
		args = append(args, arg)
		where += " AND " + strings.ReplaceAll(clause, "?", fmt.Sprintf("$%d", len(args)))
	}
	if f.OrganisationID != nil {
		add("pr.recipient_organisation_id = ?", *f.OrganisationID)
	}
	if f.AgentUserID != nil {
		add("pr.recipient_user_id = ?", *f.AgentUserID)
	}
	if f.MarketIDs != nil {
		add("pr.market_id IN (SELECT unnest(?::text[])::uuid)", uuidStrings(f.MarketIDs))
	}
	return where, args
}

// FunnelFacts implements Store.
func (s *PostgresStore) FunnelFacts(ctx context.Context, f FunnelFilter) ([]FunnelFact, error) {
	where, args := funnelWhere(f)
	rows, err := s.pool.Query(ctx, `
		SELECT pr.id, pr.listing_id, pr.request_type, pr.stage, pr.created_at,
		       COALESCE(pr.recipient_type, ''), pr.recipient_user_id,
		       t.event_type, t.actor_type, COALESCE(t.detail->>'to', ''), t.created_at
		FROM property_requests pr
		LEFT JOIN request_timeline t ON t.request_id = pr.id AND t.event_type IN ('`+TimelineStageChanged+`', '`+TimelineAssigned+`')
		WHERE `+where+`
		ORDER BY pr.created_at, pr.id, t.created_at, t.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read funnel cohort: %w", err)
	}
	defer rows.Close()
	out := []FunnelFact{}
	for rows.Next() {
		var (
			fact         FunnelFact
			event, actor *string
			to           string
			at           *time.Time
		)
		if err := rows.Scan(&fact.RequestID, &fact.ListingID, &fact.RequestType, &fact.Stage, &fact.CreatedAt,
			&fact.RecipientType, &fact.HandlerUserID, &event, &actor, &to, &at); err != nil {
			return nil, fmt.Errorf("read funnel cohort: %w", err)
		}
		if n := len(out); n == 0 || out[n-1].RequestID != fact.RequestID {
			fact.CreatedAt = fact.CreatedAt.UTC()
			out = append(out, fact)
		}
		last := &out[len(out)-1]
		switch {
		case event == nil:
		case *event == TimelineAssigned:
			last.EverAssigned = true
		case *event == TimelineStageChanged && actor != nil && at != nil:
			last.Moves = append(last.Moves, FunnelMove{To: to, ActorType: *actor, At: at.UTC()})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read funnel cohort: %w", err)
	}
	return out, nil
}

// SeedRequests writes the requests with their histories in one transaction,
// replacing a request with the same ID (its timeline and notes go with it by
// cascade). A request an appointment points at is left as it is. Nothing
// goes to the outbox or the customer activity.
func (s *PostgresStore) SeedRequests(ctx context.Context, seeds []SeedRequest) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := SeedRequestsTx(ctx, tx, seeds); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SeedRequestsTx is SeedRequests inside the caller's transaction (the
// development seed's).
func SeedRequestsTx(ctx context.Context, tx pgx.Tx, seeds []SeedRequest) error {
	for _, seed := range seeds {
		if err := seedOne(ctx, tx, seed); err != nil {
			return fmt.Errorf("seed request %s: %w", seed.Request.Reference, err)
		}
	}
	return nil
}

func seedOne(ctx context.Context, tx pgx.Tx, seed SeedRequest) error {
	req := seed.Request
	if _, err := tx.Exec(ctx, `
		DELETE FROM property_requests pr WHERE pr.id = $1
		AND NOT EXISTS (SELECT 1 FROM appointments a WHERE a.property_request_id = pr.id)`, req.ID); err != nil {
		return fmt.Errorf("remove the previous copy: %w", err)
	}
	var recipientType *string
	var recipientUser, recipientOrg *uuid.UUID
	if req.Recipient != nil {
		recipientType, recipientUser, recipientOrg = &req.Recipient.Type, req.Recipient.UserID, req.Recipient.OrganisationID
	}
	var guestName, guestEmail, guestPhone *string
	if req.GuestName != "" {
		guestName = &req.GuestName
	}
	if req.GuestEmail != "" {
		guestEmail = &req.GuestEmail
	}
	if req.GuestPhone != "" {
		guestPhone = &req.GuestPhone
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO property_requests (
			id, reference, market_id, listing_id, request_type, status, message,
			preferred_contact_method, preferred_slot_at, idempotency_key, request_hash,
			guest_name, guest_email, guest_phone, consent_privacy_version, consent_accepted_at,
			recipient_type, recipient_user_id, recipient_organisation_id, created_at, updated_at, customer_user_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $20, $21)
		ON CONFLICT (id) DO NOTHING`,
		req.ID, req.Reference, req.MarketID, req.ListingID, req.RequestType, req.Status, req.Message,
		req.PreferredContactMethod, req.PreferredSlot, req.IdempotencyKey, req.RequestHash,
		guestName, guestEmail, guestPhone, req.PrivacyVersion, req.ConsentAcceptedAt,
		recipientType, recipientUser, recipientOrg, req.CreatedAt, req.CustomerUserID)
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // kept: an appointment points at it
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO request_timeline (id, request_id, event_type, actor_type, created_at) VALUES ($1, $2, $3, $4, $5)`,
		uuid.New(), req.ID, TimelineRequestReceived, ActorCustomer, req.CreatedAt); err != nil {
		return fmt.Errorf("insert timeline: %w", err)
	}
	stage, updated := StageNewEnquiry, req.CreatedAt
	var changedAt *time.Time
	var changedBy *uuid.UUID
	for _, step := range seed.Steps {
		at, by := step.At, step.Actor.UserID
		updated = at
		if step.AssignTo != nil {
			if _, err := tx.Exec(ctx, `UPDATE property_requests SET recipient_type = $2, recipient_user_id = $3 WHERE id = $1`,
				req.ID, RecipientAgent, *step.AssignTo); err != nil {
				return fmt.Errorf("assign: %w", err)
			}
			detail := map[string]any{"assignee_user_id": step.AssignTo.String(), "assignee_name": step.AssigneeName}
			if err := insertTimeline(ctx, tx, req.ID, TimelineAssigned, ActorProfessional, step.Actor, detail, at); err != nil {
				return err
			}
			continue
		}
		if err := insertTimeline(ctx, tx, req.ID, TimelineStageChanged, ActorProfessional, step.Actor,
			map[string]any{"from": stage, "to": step.Stage}, at); err != nil {
			return err
		}
		stage, changedAt, changedBy = step.Stage, &at, &by
	}
	if _, err := tx.Exec(ctx, `
		UPDATE property_requests SET stage = $2, stage_changed_at = $3, stage_changed_by = $4, updated_at = $5 WHERE id = $1`,
		req.ID, stage, changedAt, changedBy, updated); err != nil {
		return fmt.Errorf("set the stage: %w", err)
	}
	return nil
}
