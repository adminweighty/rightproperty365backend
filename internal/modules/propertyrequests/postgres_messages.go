package propertyrequests

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The Postgres side of messages.go (migration 0048).

// Messages implements Store.
func (s *PostgresStore) Messages(ctx context.Context, id uuid.UUID) ([]Message, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, request_id, author_type, author_user_id, author_name, body, created_at
		FROM property_request_messages WHERE request_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Message, error) {
		var m Message
		err := row.Scan(&m.ID, &m.RequestID, &m.AuthorType, &m.AuthorUserID, &m.AuthorName, &m.Body, &m.CreatedAt)
		m.CreatedAt = m.CreatedAt.UTC()
		return m, err
	})
	if out == nil {
		out = []Message{}
	}
	return out, err
}

// AddMessage implements Store: the message, the conversation summary, the
// stage move of a first reply, the timeline entries and the outbox events
// in ONE transaction, with the request row locked from the read.
func (s *PostgresStore) AddMessage(ctx context.Context, m Message, allow func(StoredRequest) error) (StoredRequest, error) {
	err := s.locked(ctx, m.RequestID, allow, func(tx pgx.Tx, cur StoredRequest) error {
		actor := Actor{Name: m.AuthorName}
		if m.AuthorUserID != nil {
			actor.UserID = *m.AuthorUserID
		}
		event, timelineEvent, actorType := EventCustomerReplied, TimelineCustomerReplied, ActorCustomer
		if m.AuthorType == AuthorProfessional {
			event, timelineEvent, actorType = EventReplied, TimelineReplied, ActorProfessional
			if to := replyAdvance(cur.Stage); to != "" {
				c := StageChange{To: to, Actor: actor, At: m.CreatedAt, Via: StageViaReply}
				if _, err := tx.Exec(ctx, `
					UPDATE property_requests SET stage = $2, stage_changed_at = $3, stage_changed_by = $4 WHERE id = $1`,
					m.RequestID, to, c.At, actor.UserID); err != nil {
					return fmt.Errorf("update stage: %w", err)
				}
				if err := insertTimeline(ctx, tx, m.RequestID, TimelineStageChanged, ActorProfessional, actor,
					map[string]any{"from": cur.Stage, "to": to, "via": StageViaReply}, c.At); err != nil {
					return err
				}
				if err := insertOutbox(ctx, tx, EventStageChanged, m.RequestID, stageEventPayload(cur, cur.Stage, c), c.At); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO property_request_messages (id, request_id, author_type, author_user_id, author_name, body, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, m.ID, m.RequestID, m.AuthorType, m.AuthorUserID, m.AuthorName, m.Body, m.CreatedAt); err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE property_requests SET message_count = message_count + 1, last_message_at = $2, last_message_author = $3, updated_at = $2
			WHERE id = $1`, m.RequestID, m.CreatedAt, m.AuthorType); err != nil {
			return fmt.Errorf("update conversation: %w", err)
		}
		// The timeline says a message was written, never what it says.
		if err := insertTimeline(ctx, tx, m.RequestID, timelineEvent, actorType, actor, map[string]any{}, m.CreatedAt); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, event, m.RequestID, messageEventPayload(cur, m), m.CreatedAt)
	})
	if err != nil {
		return StoredRequest{}, err
	}
	return s.GetRequest(ctx, m.RequestID)
}

// MarkRead implements Store.
func (s *PostgresStore) MarkRead(ctx context.Context, id uuid.UUID, side string, at time.Time) error {
	column := "professional_last_read_at"
	if side == AuthorCustomer {
		column = "customer_last_read_at"
	}
	tag, err := s.pool.Exec(ctx, `UPDATE property_requests SET `+column+` = $2 WHERE id = $1`, id, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
