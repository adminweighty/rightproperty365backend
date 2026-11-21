package propertyrequests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// The Postgres side of work.go (migration 0018).
//
// NOTE: everything in this file is compiled but has not been exercised
// against a real database.

// requestColumns selects a StoredRequest; scanRequest reads it. The contact
// is the guest's, or the account's for a signed-in customer.
const requestColumns = `pr.id, pr.reference, pr.market_id, pr.listing_id, pr.request_type, pr.status, pr.stage,
	pr.stage_changed_at, pr.stage_changed_by, pr.message, pr.preferred_contact_method, pr.preferred_slot_at,
	pr.customer_user_id,
	COALESCE(pr.guest_name, u.full_name, ''), COALESCE(pr.guest_email, u.email, ''), COALESCE(pr.guest_phone, ''),
	pr.recipient_type, pr.recipient_user_id, pr.recipient_organisation_id,
	pr.check_in::text, pr.check_out::text, pr.guests, pr.created_at, pr.updated_at,
	pr.exception_resolved_at, pr.exception_resolved_by, COALESCE(pr.exception_resolution_note, ''),
	pr.message_count, pr.last_message_at, COALESCE(pr.last_message_author, ''), pr.customer_last_read_at, pr.professional_last_read_at,
	(SELECT lb.branch_id FROM listings lb WHERE lb.id = pr.listing_id)`

const requestFrom = ` FROM property_requests pr LEFT JOIN users u ON u.id = pr.customer_user_id `

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

func scanRequest(row pgx.Row) (StoredRequest, error) {
	var (
		r                 StoredRequest
		recipientType     *string
		recipientUser     *uuid.UUID
		recipientOrg      *uuid.UUID
		checkIn, checkOut *string
		guests            *int
	)
	err := row.Scan(&r.ID, &r.Reference, &r.MarketID, &r.ListingID, &r.RequestType, &r.Status, &r.Stage,
		&r.StageChangedAt, &r.StageChangedBy, &r.Message, &r.PreferredContactMethod, &r.PreferredSlot,
		&r.CustomerUserID, &r.ContactName, &r.ContactEmail, &r.ContactPhone,
		&recipientType, &recipientUser, &recipientOrg,
		&checkIn, &checkOut, &guests, &r.CreatedAt, &r.UpdatedAt,
		&r.ResolvedAt, &r.ResolvedBy, &r.ResolutionNote,
		&r.MessageCount, &r.LastMessageAt, &r.LastMessageAuthor, &r.CustomerLastReadAt, &r.ProfessionalLastReadAt, &r.ListingBranchID)
	if err != nil {
		return StoredRequest{}, err
	}
	r.LastMessageAt, r.CustomerLastReadAt, r.ProfessionalLastReadAt = utcPtr(r.LastMessageAt), utcPtr(r.CustomerLastReadAt), utcPtr(r.ProfessionalLastReadAt)
	if recipientType != nil {
		r.Recipient = &Recipient{Type: *recipientType, UserID: recipientUser, OrganisationID: recipientOrg}
	}
	r.Stay = StayOf(checkIn, checkOut, guests)
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	r.StageChangedAt, r.PreferredSlot, r.ResolvedAt = utcPtr(r.StageChangedAt), utcPtr(r.PreferredSlot), utcPtr(r.ResolvedAt)
	return r, nil
}

func collectRequests(rows pgx.Rows, err error) ([]StoredRequest, error) {
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (StoredRequest, error) { return scanRequest(row) })
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []StoredRequest{}
	}
	return items, nil
}

// likePattern turns free text into a pattern for ILIKE ... ESCAPE '\'.
func likePattern(q string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
}

// inboxWhere builds the WHERE clause of an inbox WITHOUT its stage part, and
// its arguments. The first three arguments are the scope (see InboxScope):
// routing exceptions have neither a recipient organisation nor a recipient
// user, so no branch can match them.
func inboxWhere(f InboxFilter) (string, []any) {
	args := []any{uuidStrings(f.Scope.ManagedOrganisationIDs), uuidStrings(f.Scope.MemberOrganisationIDs), f.Scope.UserID}
	where := `(pr.recipient_organisation_id IN (SELECT unnest($1::text[])::uuid)
		OR (pr.recipient_organisation_id IN (SELECT unnest($2::text[])::uuid)
		    AND (pr.recipient_user_id IS NULL OR pr.recipient_user_id = $3))
		OR (pr.recipient_type = 'SELLER' AND pr.recipient_user_id = $3))`
	add := func(clause string, arg any) {
		args = append(args, arg)
		where += " AND " + strings.ReplaceAll(clause, "?", fmt.Sprintf("$%d", len(args)))
	}
	// Branch pins (API 0.57.0): "org:branch" pairs; a request of a pinned
	// organisation about a listing of another branch is left out.
	if len(f.Scope.Branches) > 0 {
		pairs := make([]string, 0, len(f.Scope.Branches))
		for org, b := range f.Scope.Branches {
			pairs = append(pairs, org.String()+":"+b.String())
		}
		args = append(args, pairs)
		n := len(args)
		where += fmt.Sprintf(` AND NOT EXISTS (SELECT 1 FROM listings lb, unnest($%d::text[]) pin
			WHERE lb.id = pr.listing_id AND lb.branch_id IS NOT NULL
			  AND split_part(pin, ':', 1)::uuid = pr.recipient_organisation_id
			  AND split_part(pin, ':', 2)::uuid <> lb.branch_id)`, n)
	}
	if f.Type != "" {
		add("pr.request_type = ?", f.Type)
	}
	if f.ListingID != nil {
		add("pr.listing_id = ?", *f.ListingID)
	}
	if f.From != nil {
		add("pr.created_at >= ?", *f.From)
	}
	if f.To != nil {
		add("pr.created_at < ?", *f.To)
	}
	if f.Q != "" {
		add(`(pr.reference ILIKE ? ESCAPE '\' OR COALESCE(pr.guest_name, u.full_name, '') ILIKE ? ESCAPE '\'
			OR COALESCE(pr.guest_email, u.email, '') ILIKE ? ESCAPE '\' OR pr.message ILIKE ? ESCAPE '\')`, likePattern(f.Q))
	}
	return where, args
}

// ListInbox implements Store.
func (s *PostgresStore) ListInbox(ctx context.Context, f InboxFilter) (InboxPage, error) {
	where, args := inboxWhere(f)
	page := InboxPage{StageCounts: map[string]int{}}

	rows, err := s.pool.Query(ctx, `SELECT pr.stage, count(*)`+requestFrom+`WHERE `+where+` GROUP BY pr.stage`, args...)
	if err != nil {
		return InboxPage{}, fmt.Errorf("count stages: %w", err)
	}
	for rows.Next() {
		var stage string
		var n int
		if err := rows.Scan(&stage, &n); err != nil {
			rows.Close()
			return InboxPage{}, fmt.Errorf("count stages: %w", err)
		}
		page.StageCounts[stage] = n
		if len(f.Stages) == 0 || slices.Contains(f.Stages, stage) {
			page.Total += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return InboxPage{}, fmt.Errorf("count stages: %w", err)
	}

	stages := f.Stages
	if len(stages) == 0 {
		stages = Stages()
	}
	args = append(args, stages, f.Limit, max(f.Offset, 0))
	n := len(args)
	page.Items, err = collectRequests(s.pool.Query(ctx, `SELECT `+requestColumns+requestFrom+`WHERE `+where+
		fmt.Sprintf(` AND pr.stage = ANY($%d::text[]) ORDER BY pr.created_at DESC, pr.id DESC LIMIT $%d OFFSET $%d`, n-2, n-1, n), args...))
	if err != nil {
		return InboxPage{}, fmt.Errorf("list requests: %w", err)
	}
	return page, nil
}

// GetRequest implements Store.
func (s *PostgresStore) GetRequest(ctx context.Context, id uuid.UUID) (StoredRequest, error) {
	r, err := scanRequest(s.pool.QueryRow(ctx, `SELECT `+requestColumns+requestFrom+`WHERE pr.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredRequest{}, ErrNotFound
	}
	return r, err
}

// ListByCustomer implements Store.
func (s *PostgresStore) ListByCustomer(ctx context.Context, f CustomerFilter) ([]StoredRequest, int, map[string]int, error) {
	counts, total := map[string]int{}, 0
	rows, err := s.pool.Query(ctx, `SELECT stage, count(*) FROM property_requests WHERE customer_user_id = $1 GROUP BY stage`, f.UserID)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("count stages: %w", err)
	}
	for rows.Next() {
		var stage string
		var n int
		if err := rows.Scan(&stage, &n); err != nil {
			rows.Close()
			return nil, 0, nil, fmt.Errorf("count stages: %w", err)
		}
		counts[stage] = n
		if f.Stages == nil || slices.Contains(f.Stages, stage) {
			total += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, nil, fmt.Errorf("count stages: %w", err)
	}
	stages := f.Stages
	if stages == nil {
		stages = Stages()
	}
	limit := f.Limit
	if limit <= 0 {
		limit = MaxPageSize
	}
	items, err := collectRequests(s.pool.Query(ctx, `SELECT `+requestColumns+requestFrom+`
		WHERE pr.customer_user_id = $1 AND pr.stage = ANY($2::text[])
		ORDER BY pr.created_at DESC, pr.id DESC LIMIT $3 OFFSET $4`, f.UserID, stages, limit, max(f.Offset, 0)))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("list requests: %w", err)
	}
	return items, total, counts, nil
}

// RequestTimeline implements Store.
func (s *PostgresStore) RequestTimeline(ctx context.Context, id uuid.UUID) ([]TimelineEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT event_type, actor_type, actor_user_id,
		       COALESCE(detail->>'actor_name', ''), COALESCE(detail->>'from', ''), COALESCE(detail->>'to', ''),
		       COALESCE(detail->>'assignee_name', ''), created_at
		FROM request_timeline WHERE request_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TimelineEntry, error) {
		var e TimelineEntry
		err := row.Scan(&e.EventType, &e.ActorType, &e.ActorUserID, &e.ActorName, &e.FromStage, &e.ToStage, &e.AssigneeName, &e.At)
		e.At = e.At.UTC()
		return e, err
	})
	if out == nil {
		out = []TimelineEntry{}
	}
	return out, err
}

// Notes implements Store.
func (s *PostgresStore) Notes(ctx context.Context, id uuid.UUID) ([]Note, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, request_id, author_user_id, author_name, body, created_at
		FROM property_request_notes WHERE request_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Note, error) {
		var n Note
		err := row.Scan(&n.ID, &n.RequestID, &n.AuthorUserID, &n.AuthorName, &n.Body, &n.CreatedAt)
		n.CreatedAt = n.CreatedAt.UTC()
		return n, err
	})
	if out == nil {
		out = []Note{}
	}
	return out, err
}

// locked runs fn in one transaction with the request's row locked, after
// allow accepted the current row. ErrNotFound when the request is missing.
func (s *PostgresStore) locked(ctx context.Context, id uuid.UUID, allow func(StoredRequest) error, fn func(tx pgx.Tx, cur StoredRequest) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	cur, err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+requestFrom+`WHERE pr.id = $1 FOR UPDATE OF pr`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock request: %w", err)
	}
	if err := allow(cur); err != nil {
		return err
	}
	if err := fn(tx, cur); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func insertTimeline(ctx context.Context, tx pgx.Tx, id uuid.UUID, event, actorType string, actor Actor, detail map[string]any, at time.Time) error {
	detail["actor_name"] = actor.Name
	body, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO request_timeline (id, request_id, event_type, actor_type, actor_user_id, detail, created_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)`,
		uuid.New(), id, event, actorType, actor.UserID, string(body), at); err != nil {
		return fmt.Errorf("insert request timeline: %w", err)
	}
	return nil
}

func insertOutbox(ctx context.Context, tx pgx.Tx, event string, id uuid.UUID, payload map[string]any, at time.Time) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
		uuid.New(), event, AggregatePropertyRequest, id, string(body), at); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// ChangeStage implements Store: the stage, its timeline entry and its outbox
// event are written in ONE transaction, with the row locked from the read.
func (s *PostgresStore) ChangeStage(ctx context.Context, id uuid.UUID, c StageChange, allow func(StoredRequest) error) (StoredRequest, error) {
	err := s.locked(ctx, id, allow, func(tx pgx.Tx, cur StoredRequest) error {
		if _, err := tx.Exec(ctx, `
			UPDATE property_requests SET stage = $2, stage_changed_at = $3, stage_changed_by = $4, updated_at = $3 WHERE id = $1`,
			id, c.To, c.At, c.Actor.UserID); err != nil {
			return fmt.Errorf("update stage: %w", err)
		}
		if err := insertTimeline(ctx, tx, id, TimelineStageChanged, ActorProfessional, c.Actor,
			map[string]any{"from": cur.Stage, "to": c.To}, c.At); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, EventStageChanged, id, stageEventPayload(cur, cur.Stage, c), c.At)
	})
	if err != nil {
		return StoredRequest{}, err
	}
	return s.GetRequest(ctx, id)
}

// AddNote implements Store.
func (s *PostgresStore) AddNote(ctx context.Context, n Note, allow func(StoredRequest) error) error {
	return s.locked(ctx, n.RequestID, allow, func(tx pgx.Tx, _ StoredRequest) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO property_request_notes (id, request_id, author_user_id, author_name, body, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, n.ID, n.RequestID, n.AuthorUserID, n.AuthorName, n.Body, n.CreatedAt); err != nil {
			return fmt.Errorf("insert note: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE property_requests SET updated_at = $2 WHERE id = $1`, n.RequestID, n.CreatedAt); err != nil {
			return fmt.Errorf("touch request: %w", err)
		}
		// The timeline says a note was added, never what it says.
		return insertTimeline(ctx, tx, n.RequestID, TimelineNoteAdded, ActorProfessional,
			Actor{UserID: n.AuthorUserID, Name: n.AuthorName}, map[string]any{}, n.CreatedAt)
	})
}

// ListExceptions implements Store.
func (s *PostgresStore) ListExceptions(ctx context.Context, f ExceptionFilter) ([]StoredRequest, int, error) {
	// A nil slice is sent as NULL: every market.
	var markets []string
	if f.MarketIDs != nil {
		markets = uuidStrings(f.MarketIDs)
	}
	// A rerouted exception (API 0.49.0) is RECEIVED now but stays in the
	// list, resolved: it is the only kind of request with a resolution.
	where := `(pr.status = 'ROUTING_EXCEPTION' OR pr.exception_resolved_at IS NOT NULL)
		AND ($1::text[] IS NULL OR pr.market_id IN (SELECT unnest($1::text[])::uuid))
		AND ($2::boolean IS NULL OR (pr.exception_resolved_at IS NOT NULL) = $2)`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+requestFrom+`WHERE `+where, markets, f.Resolved).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count routing exceptions: %w", err)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = MaxPageSize
	}
	items, err := collectRequests(s.pool.Query(ctx, `SELECT `+requestColumns+requestFrom+`WHERE `+where+`
		ORDER BY pr.created_at, pr.id LIMIT $3 OFFSET $4`, markets, f.Resolved, limit, max(f.Offset, 0)))
	if err != nil {
		return nil, 0, fmt.Errorf("list routing exceptions: %w", err)
	}
	return items, total, nil
}

// ResolveException implements Store.
func (s *PostgresStore) ResolveException(ctx context.Context, id uuid.UUID, res Resolution, allow func(StoredRequest) error) (StoredRequest, error) {
	err := s.locked(ctx, id, allow, func(tx pgx.Tx, cur StoredRequest) error {
		var note *string
		if res.Note != "" {
			note = &res.Note
		}
		if _, err := tx.Exec(ctx, `
			UPDATE property_requests
			SET exception_resolved_at = $2, exception_resolved_by = $3, exception_resolution_note = $4, updated_at = $2
			WHERE id = $1`, id, res.At, res.Actor.UserID, note); err != nil {
			return fmt.Errorf("resolve routing exception: %w", err)
		}
		if err := insertTimeline(ctx, tx, id, TimelineExceptionResolved, ActorAdmin, res.Actor, map[string]any{}, res.At); err != nil {
			return err
		}
		return insertOutbox(ctx, tx, EventExceptionResolved, id, resolvedEventPayload(cur, res), res.At)
	})
	if err != nil {
		return StoredRequest{}, err
	}
	return s.GetRequest(ctx, id)
}

// AdminCounts implements Store.
func (s *PostgresStore) AdminCounts(ctx context.Context, marketIDs []uuid.UUID, now time.Time) (AdminCounts, error) {
	var markets []string
	if marketIDs != nil {
		markets = uuidStrings(marketIDs)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT pr.stage, pr.status, pr.exception_resolved_at IS NOT NULL, count(*),
		       count(*) FILTER (WHERE pr.created_at > $2), count(*) FILTER (WHERE pr.created_at > $3)
		FROM property_requests pr
		WHERE $1::text[] IS NULL OR pr.market_id IN (SELECT unnest($1::text[])::uuid)
		GROUP BY 1, 2, 3`, markets, now.AddDate(0, 0, -7), now.AddDate(0, 0, -30))
	if err != nil {
		return AdminCounts{}, err
	}
	defer rows.Close()
	c := AdminCounts{ByStage: map[string]int{}}
	for rows.Next() {
		var stage, status string
		var resolved bool
		var n, last7, last30 int
		if err := rows.Scan(&stage, &status, &resolved, &n, &last7, &last30); err != nil {
			return AdminCounts{}, err
		}
		c.Total, c.Last7Days, c.Last30Days = c.Total+n, c.Last7Days+last7, c.Last30Days+last30
		switch {
		case status != StatusRoutingException:
			c.ByStage[stage] += n
		case resolved:
			c.ExceptionsResolved += n
		default:
			c.ExceptionsOpen += n
		}
	}
	return c, rows.Err()
}

// Assign implements Store (assign.go): everything in ONE transaction, with
// the row locked from the read.
func (s *PostgresStore) Assign(ctx context.Context, id uuid.UUID, a Assignment, allow func(StoredRequest) error) (StoredRequest, error) {
	err := s.locked(ctx, id, allow, func(tx pgx.Tx, cur StoredRequest) error {
		recipientType, event := RecipientOrganisation, TimelineUnassigned
		if a.To != nil {
			recipientType, event = RecipientAgent, TimelineAssigned
		}
		if _, err := tx.Exec(ctx, `
			UPDATE property_requests SET recipient_type = $2, recipient_user_id = $3, updated_at = $4 WHERE id = $1`,
			id, recipientType, a.To, a.At); err != nil {
			return fmt.Errorf("assign request: %w", err)
		}
		detail := map[string]any{}
		if a.To != nil {
			detail["assignee_user_id"], detail["assignee_name"] = a.To.String(), a.ToName
		}
		if err := insertTimeline(ctx, tx, id, event, ActorProfessional, a.Actor, detail, a.At); err != nil {
			return err
		}
		if a.To != nil && cur.Stage == StageNewEnquiry {
			change := StageChange{To: StageAssigned, Actor: a.Actor, At: a.At}
			if _, err := tx.Exec(ctx, `
				UPDATE property_requests SET stage = $2, stage_changed_at = $3, stage_changed_by = $4 WHERE id = $1`,
				id, StageAssigned, a.At, a.Actor.UserID); err != nil {
				return fmt.Errorf("update stage: %w", err)
			}
			if err := insertTimeline(ctx, tx, id, TimelineStageChanged, ActorProfessional, a.Actor,
				map[string]any{"from": cur.Stage, "to": StageAssigned}, a.At); err != nil {
				return err
			}
			if err := insertOutbox(ctx, tx, EventStageChanged, id, stageEventPayload(cur, cur.Stage, change), a.At); err != nil {
				return err
			}
		}
		if a.To != nil {
			if err := insertOutbox(ctx, tx, EventRequestAssigned, id, assignedPayload(cur, a), a.At); err != nil {
				return err
			}
		}
		return orgaudit.Insert(ctx, tx, a.Audit)
	})
	if err != nil {
		return StoredRequest{}, err
	}
	return s.GetRequest(ctx, id)
}

// OpenLeads implements Store.
func (s *PostgresStore) OpenLeads(ctx context.Context, organisationID uuid.UUID) (map[uuid.UUID]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT recipient_user_id, count(*) FROM property_requests
		WHERE recipient_organisation_id = $1 AND recipient_user_id IS NOT NULL AND stage NOT IN ('CONVERTED', 'CLOSED')
		GROUP BY recipient_user_id`, organisationID)
	if err != nil {
		return nil, fmt.Errorf("count open leads: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]int{}
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ReleaseMemberTx returns the organisation's open requests assigned to the
// user to the organisation inbox, each with an UNASSIGNED timeline entry, in
// the caller's transaction (removing or suspending a member). It returns how
// many there were.
func ReleaseMemberTx(ctx context.Context, tx pgx.Tx, organisationID, userID uuid.UUID, actor Actor, at time.Time) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		WITH released AS (
			UPDATE property_requests SET recipient_type = 'ORGANISATION', recipient_user_id = NULL, updated_at = $3
			WHERE recipient_organisation_id = $1 AND recipient_user_id = $2 AND stage NOT IN ('CONVERTED', 'CLOSED')
			RETURNING id
		), entries AS (
			INSERT INTO request_timeline (id, request_id, event_type, actor_type, actor_user_id, detail, created_at)
			SELECT gen_random_uuid(), r.id, $4, $5, $6, jsonb_build_object('actor_name', $7::text), $3 FROM released r
			RETURNING 1
		)
		SELECT count(*) FROM entries`,
		organisationID, userID, at, TimelineUnassigned, ActorProfessional, actor.UserID, actor.Name).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("release the member's requests: %w", err)
	}
	return n, nil
}
