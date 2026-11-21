package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// PostgresStore is the production Store (migration 0024).
type PostgresStore struct {
	pool *pgxpool.Pool
	// entitle applies a change's Entitlement in its transaction (nil: ignored).
	entitle EntitlementTxFunc
}

// EntitlementTxFunc applies an entitlement inside the store's transaction.
type EntitlementTxFunc func(ctx context.Context, tx pgx.Tx, x Subscription, e Entitlement, at time.Time) error

// SetEntitlements sets the hook that grants and ends listing packages (wiring).
func (s *PostgresStore) SetEntitlements(fn EntitlementTxFunc) { s.entitle = fn }

// NewPostgresStore returns the Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (s *PostgresStore) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NextInvoiceNumber implements Store.
func (s *PostgresStore) NextInvoiceNumber(ctx context.Context, year int) (string, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT nextval('subscription_invoice_seq')`).Scan(&n); err != nil {
		return "", fmt.Errorf("next invoice number: %w", err)
	}
	return invoiceNumber(year, n), nil
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

const planColumns = `id, name, description, subscriber_kind, eligible_roles, features, benefits, status, sort_order, created_by,
	created_at, updated_at, retired_at, COALESCE(package_code, '')`

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	var features []byte
	if err := row.Scan(&p.ID, &p.Name, &p.Description, &p.SubscriberKind, &p.EligibleRoles, &features, &p.Benefits, &p.Status,
		&p.SortOrder, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.RetiredAt, &p.PackageCode); err != nil {
		return Plan{}, err
	}
	if err := json.Unmarshal(features, &p.Features); err != nil {
		return Plan{}, fmt.Errorf("plan features: %w", err)
	}
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	if p.RetiredAt != nil {
		t := p.RetiredAt.UTC()
		p.RetiredAt = &t
	}
	if p.Features == nil {
		p.Features = []Feature{}
	}
	if p.Benefits == nil {
		p.Benefits = []string{}
	}
	return p, nil
}

func loadPrices(ctx context.Context, q querier, plans []Plan) error {
	if len(plans) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(plans))
	index := map[uuid.UUID]int{}
	for i, p := range plans {
		ids[i], index[p.ID] = p.ID, i
		plans[i].Prices = []Price{}
	}
	rows, err := q.Query(ctx, `
		SELECT plan_id, currency_code, period, round(amount, 2)::text FROM subscription_plan_prices
		WHERE plan_id = ANY($1) ORDER BY currency_code, period`, ids)
	if err != nil {
		return fmt.Errorf("load plan prices: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var pr Price
		if err := rows.Scan(&id, &pr.CurrencyCode, &pr.Period, &pr.Amount); err != nil {
			return err
		}
		plans[index[id]].Prices = append(plans[index[id]].Prices, pr)
	}
	return rows.Err()
}

func writePlan(ctx context.Context, tx pgx.Tx, p Plan, insert bool) error {
	features, err := json.Marshal(p.Features)
	if err != nil {
		return err
	}
	if insert {
		_, err = tx.Exec(ctx, `
			INSERT INTO subscription_plans (id, name, description, subscriber_kind, eligible_roles, features, benefits, status, sort_order,
				created_by, created_at, updated_at, retired_at, package_code)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, $12, $13, NULLIF($14::text, ''))`,
			p.ID, p.Name, p.Description, p.SubscriberKind, p.EligibleRoles, string(features), p.Benefits, p.Status, p.SortOrder,
			p.CreatedBy, p.CreatedAt, p.UpdatedAt, p.RetiredAt, p.PackageCode)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE subscription_plans SET name = $2, description = $3, subscriber_kind = $4, eligible_roles = $5, features = $6::jsonb,
				benefits = $7, status = $8, sort_order = $9, updated_at = $10, retired_at = $11, package_code = NULLIF($12::text, '')
			WHERE id = $1`,
			p.ID, p.Name, p.Description, p.SubscriberKind, p.EligibleRoles, string(features), p.Benefits, p.Status, p.SortOrder,
			p.UpdatedAt, p.RetiredAt, p.PackageCode)
	}
	if err != nil {
		return fmt.Errorf("save plan: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM subscription_plan_prices WHERE plan_id = $1`, p.ID); err != nil {
		return fmt.Errorf("save plan prices: %w", err)
	}
	for _, pr := range p.Prices {
		if _, err := tx.Exec(ctx, `INSERT INTO subscription_plan_prices (plan_id, currency_code, period, amount) VALUES ($1, $2, $3, $4::numeric)`,
			p.ID, pr.CurrencyCode, pr.Period, pr.Amount); err != nil {
			return fmt.Errorf("save plan price: %w", err)
		}
	}
	return nil
}

// CreatePlan implements Store.
func (s *PostgresStore) CreatePlan(ctx context.Context, p Plan, audit adminaudit.Event) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := writePlan(ctx, tx, p, true); err != nil {
			return err
		}
		return adminaudit.Insert(ctx, tx, audit)
	})
}

// Plan implements Store.
func (s *PostgresStore) Plan(ctx context.Context, id uuid.UUID) (Plan, error) {
	return plan(ctx, s.pool, id, false)
}

func plan(ctx context.Context, q querier, id uuid.UUID, lock bool) (Plan, error) {
	sql := `SELECT ` + planColumns + ` FROM subscription_plans WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	p, err := scanPlan(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, ErrPlanNotFound
	}
	if err != nil {
		return Plan{}, fmt.Errorf("load plan: %w", err)
	}
	list := []Plan{p}
	if err := loadPrices(ctx, q, list); err != nil {
		return Plan{}, err
	}
	return list[0], nil
}

// Plans implements Store.
func (s *PostgresStore) Plans(ctx context.Context, includeRetired bool) ([]Plan, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+planColumns+` FROM subscription_plans WHERE status = 'ACTIVE' OR $1
		ORDER BY sort_order, name, id`, includeRetired)
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Plan, error) { return scanPlan(row) })
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	if list == nil {
		list = []Plan{}
	}
	return list, loadPrices(ctx, s.pool, list)
}

// ChangePlan implements Store.
func (s *PostgresStore) ChangePlan(ctx context.Context, id uuid.UUID, change func(p *Plan) (adminaudit.Event, error)) (Plan, error) {
	var out Plan
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := plan(ctx, tx, id, true)
		if err != nil {
			return err
		}
		next := clonePlan(cur)
		audit, err := change(&next)
		if err != nil {
			return err
		}
		if err := writePlan(ctx, tx, next, false); err != nil {
			return err
		}
		if err := adminaudit.Insert(ctx, tx, audit); err != nil {
			return err
		}
		out = next
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

const subColumns = `id, invoice_number, plan_id, plan_name, period, status, subscriber_kind, subscriber_id, subscriber_name, organisation_id,
	requested_by, requested_by_name, market_code, currency_code, round(amount, 2)::text, renewal_of, starts_at, ends_at, activated_at,
	payment_reference, round(payment_amount, 2)::text, to_char(payment_received_on, 'YYYY-MM-DD'), payment_note, payment_recorded_by,
	payment_recorded_by_name, rejected_at, rejection_reason, cancelled_at, cancel_reason, COALESCE(cancelled_by_kind, ''), refund_due,
	refund_note, expired_at, created_at, updated_at, payment_recorded_by_kind, payment_provider`

func scanSub(row pgx.Row) (Subscription, error) {
	var x Subscription
	var payRef, payAmount, payOn *string
	var payNote, payByName, payKind, payProvider string
	var payBy *uuid.UUID
	if err := row.Scan(&x.ID, &x.InvoiceNumber, &x.PlanID, &x.PlanName, &x.Period, &x.Status, &x.SubscriberKind, &x.SubscriberID,
		&x.SubscriberName, &x.OrganisationID, &x.RequestedBy, &x.RequestedByName, &x.MarketCode, &x.CurrencyCode, &x.Amount, &x.RenewalOf,
		&x.StartsAt, &x.EndsAt, &x.ActivatedAt, &payRef, &payAmount, &payOn, &payNote, &payBy, &payByName, &x.RejectedAt, &x.RejectionReason,
		&x.CancelledAt, &x.CancelReason, &x.CancelledBy, &x.RefundDue, &x.RefundNote, &x.ExpiredAt, &x.CreatedAt, &x.UpdatedAt,
		&payKind, &payProvider); err != nil {
		return Subscription{}, err
	}
	if payAmount != nil {
		x.Payment = &Payment{Amount: *payAmount, Note: payNote, RecordedBy: payBy, RecordedByName: payByName, RecordedByKind: payKind,
			Provider: payProvider}
		if payRef != nil {
			x.Payment.Reference = *payRef
		}
		if payOn != nil {
			x.Payment.ReceivedOn = *payOn
		}
	}
	for _, t := range []**time.Time{&x.StartsAt, &x.EndsAt, &x.ActivatedAt, &x.RejectedAt, &x.CancelledAt, &x.ExpiredAt} {
		if *t != nil {
			utc := (**t).UTC()
			*t = &utc
		}
	}
	x.CreatedAt, x.UpdatedAt = x.CreatedAt.UTC(), x.UpdatedAt.UTC()
	return x, nil
}

func insertEvent(ctx context.Context, q querier, e Event) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO subscription_events (id, subscription_id, action, actor_type, actor_user_id, actor_name, note, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.ID, e.SubscriptionID, e.Action, e.ActorType, e.ActorUserID, e.ActorName, e.Note, e.At); err != nil {
		return fmt.Errorf("insert subscription event: %w", err)
	}
	return nil
}

func insertOutbox(ctx context.Context, q querier, eventType string, id uuid.UUID, at time.Time) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
		VALUES ($1, $2, $3, $4, jsonb_build_object('subscription_id', $7::text, 'occurred_at', $5::text), $6, $6)`,
		uuid.New(), eventType, AggregateSubscription, id, at.UTC().Format(time.RFC3339Nano), at, id.String()); err != nil {
		return fmt.Errorf("insert outbox event %s: %w", eventType, err)
	}
	return nil
}

func paymentArgs(p *Payment) (ref, amount, on *string, note string, by *uuid.UUID, byName string) {
	if p == nil {
		return nil, nil, nil, "", nil, ""
	}
	r, a, o := p.Reference, p.Amount, p.ReceivedOn
	return &r, &a, &o, p.Note, p.RecordedBy, p.RecordedByName
}

// paymentRecorder returns who recorded the payment (ADMIN when none) and the provider.
func paymentRecorder(p *Payment) (kind, provider string) {
	if p == nil || p.RecordedByKind == "" {
		return RecordedByAdmin, ""
	}
	return p.RecordedByKind, p.Provider
}

func cancelledBy(kind string) *string {
	if kind == "" {
		return nil
	}
	return &kind
}

// CreateSubscription implements Store.
func (s *PostgresStore) CreateSubscription(ctx context.Context, x Subscription, ev Event, check func([]Subscription) error) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+subColumns+` FROM subscriptions WHERE subscriber_kind = $1 AND subscriber_id = $2
			ORDER BY created_at DESC, id DESC FOR UPDATE`, x.SubscriberKind, x.SubscriberID)
		if err != nil {
			return fmt.Errorf("lock subscriber: %w", err)
		}
		existing, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Subscription, error) { return scanSub(row) })
		if err != nil {
			return fmt.Errorf("lock subscriber: %w", err)
		}
		if check != nil {
			if err := check(existing); err != nil {
				return err
			}
		}
		ref, amount, on, note, by, byName := paymentArgs(x.Payment)
		if _, err := tx.Exec(ctx, `
			INSERT INTO subscriptions (id, invoice_number, plan_id, plan_name, period, status, subscriber_kind, subscriber_id, subscriber_name,
				organisation_id, requested_by, requested_by_name, market_code, currency_code, amount, renewal_of, starts_at, ends_at, activated_at,
				payment_reference, payment_amount, payment_received_on, payment_note, payment_recorded_by, payment_recorded_by_name,
				created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::numeric, $16, $17, $18, $19, $20, $21::numeric,
				$22::date, $23, $24, $25, $26, $27)`,
			x.ID, x.InvoiceNumber, x.PlanID, x.PlanName, x.Period, x.Status, x.SubscriberKind, x.SubscriberID, x.SubscriberName,
			x.OrganisationID, x.RequestedBy, x.RequestedByName, x.MarketCode, x.CurrencyCode, x.Amount, x.RenewalOf, x.StartsAt, x.EndsAt,
			x.ActivatedAt, ref, amount, on, note, by, byName, x.CreatedAt, x.UpdatedAt); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "subscriptions_one_pending_key" {
				return ErrPendingExists
			}
			return fmt.Errorf("insert subscription: %w", err)
		}
		if err := insertEvent(ctx, tx, ev); err != nil {
			return err
		}
		if x.Status == StatusPendingPayment && ev.Action != ActionInvoiced {
			return insertOutbox(ctx, tx, EventRequested, x.ID, x.CreatedAt)
		}
		return nil
	})
}

// Subscription implements Store.
func (s *PostgresStore) Subscription(ctx context.Context, id uuid.UUID) (Subscription, error) {
	x, err := scanSub(s.pool.QueryRow(ctx, `SELECT `+subColumns+` FROM subscriptions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("load subscription: %w", err)
	}
	return x, nil
}

func likePattern(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q)) + "%"
}

// Subscriptions implements Store.
func (s *PostgresStore) Subscriptions(ctx context.Context, f Filter) ([]Subscription, int, error) {
	where := `($1 = '' OR status = $1) AND ($2::uuid IS NULL OR plan_id = $2) AND ($3 = '' OR market_code = $3) AND ($4 = '' OR period = $4)
		AND ($5 = '' OR subscriber_kind = $5) AND ($6::uuid[] IS NULL OR subscriber_id = ANY($6))
		AND ($7 = '' OR lower(invoice_number) LIKE $7 OR lower(subscriber_name) LIKE $7)
		AND ($8::timestamptz IS NULL OR created_at >= $8) AND ($9::timestamptz IS NULL OR created_at < $9)
		AND ($10::text[] IS NULL OR market_code = ANY($10))`
	var subscribers []uuid.UUID
	if f.Subscribers != nil {
		subscribers = append([]uuid.UUID{}, f.Subscribers...)
	}
	var from, to *time.Time
	if !f.CreatedFrom.IsZero() {
		from = &f.CreatedFrom
	}
	if !f.CreatedTo.IsZero() {
		to = &f.CreatedTo
	}
	var marketCodes []string
	if f.Markets != nil {
		marketCodes = append([]string{}, f.Markets...)
	}
	args := []any{f.Status, f.PlanID, f.Market, f.Period, f.SubscriberKind, subscribers, likePattern(f.Q), from, to, marketCodes}
	if f.Subscribers == nil {
		args[5] = nil
	}
	if f.Markets == nil {
		args[9] = nil
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM subscriptions WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count subscriptions: %w", err)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+subColumns+` FROM subscriptions WHERE `+where+`
		ORDER BY created_at DESC, id DESC LIMIT $11 OFFSET $12`, append(args, limit, max(f.Offset, 0))...)
	if err != nil {
		return nil, 0, fmt.Errorf("list subscriptions: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Subscription, error) { return scanSub(row) })
	if err != nil {
		return nil, 0, fmt.Errorf("list subscriptions: %w", err)
	}
	if out == nil {
		out = []Subscription{}
	}
	return out, total, nil
}

// ChangeSubscription implements Store.
func (s *PostgresStore) ChangeSubscription(ctx context.Context, id uuid.UUID, change func(x *Subscription) (Change, error)) (Subscription, error) {
	var out Subscription
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := scanSub(tx.QueryRow(ctx, `SELECT `+subColumns+` FROM subscriptions WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load subscription: %w", err)
		}
		next := cloneSub(cur)
		ch, err := change(&next)
		if err != nil {
			return err
		}
		ref, amount, on, note, by, byName := paymentArgs(next.Payment)
		kind, provider := paymentRecorder(next.Payment)
		if _, err := tx.Exec(ctx, `
			UPDATE subscriptions SET status = $2, starts_at = $3, ends_at = $4, activated_at = $5, payment_reference = $6,
				payment_amount = $7::numeric, payment_received_on = $8::date, payment_note = $9, payment_recorded_by = $10,
				payment_recorded_by_name = $11, rejected_at = $12, rejection_reason = $13, cancelled_at = $14, cancel_reason = $15,
				cancelled_by_kind = $16, refund_due = $17, refund_note = $18, expired_at = $19, updated_at = $20,
				payment_recorded_by_kind = $21, payment_provider = $22
			WHERE id = $1`,
			id, next.Status, next.StartsAt, next.EndsAt, next.ActivatedAt, ref, amount, on, note, by, byName, next.RejectedAt,
			next.RejectionReason, next.CancelledAt, next.CancelReason, cancelledBy(next.CancelledBy), next.RefundDue, next.RefundNote,
			next.ExpiredAt, next.UpdatedAt, kind, provider); err != nil {
			return fmt.Errorf("update subscription: %w", err)
		}
		if ch.Entitlement != nil && s.entitle != nil {
			if err := s.entitle(ctx, tx, next, *ch.Entitlement, next.UpdatedAt); err != nil {
				return err
			}
		}
		for _, e := range ch.Events {
			if err := insertEvent(ctx, tx, e); err != nil {
				return err
			}
		}
		if ch.Audit != nil {
			if err := adminaudit.Insert(ctx, tx, *ch.Audit); err != nil {
				return err
			}
		}
		if ch.Outbox != "" {
			if err := insertOutbox(ctx, tx, ch.Outbox, id, next.UpdatedAt); err != nil {
				return err
			}
		}
		out = next
		return nil
	})
	return out, err
}

// Events implements Store.
func (s *PostgresStore) Events(ctx context.Context, id uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, subscription_id, action, actor_type, actor_user_id, actor_name, note, created_at
		FROM subscription_events WHERE subscription_id = $1 ORDER BY created_at, seq`, id)
	if err != nil {
		return nil, fmt.Errorf("load subscription history: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.ID, &e.SubscriptionID, &e.Action, &e.ActorType, &e.ActorUserID, &e.ActorName, &e.Note, &e.At)
		e.At = e.At.UTC()
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("load subscription history: %w", err)
	}
	if out == nil {
		out = []Event{}
	}
	return out, nil
}

// ExpireDue implements Store.
func (s *PostgresStore) ExpireDue(ctx context.Context, now time.Time) ([]Subscription, error) {
	var out []Subscription
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE subscriptions SET status = 'EXPIRED', expired_at = $1, updated_at = $1
			WHERE id IN (SELECT id FROM subscriptions WHERE status = 'ACTIVE' AND ends_at <= $1 FOR UPDATE SKIP LOCKED)
			RETURNING `+subColumns, now)
		if err != nil {
			return fmt.Errorf("expire subscriptions: %w", err)
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Subscription, error) { return scanSub(row) })
		if err != nil {
			return fmt.Errorf("expire subscriptions: %w", err)
		}
		for _, x := range out {
			if err := insertEvent(ctx, tx, NewEvent(x.ID, ActionExpired, ActorSystem, uuid.Nil, "", "", now)); err != nil {
				return err
			}
			if err := insertOutbox(ctx, tx, EventExpired, x.ID, now); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// ActiveListings implements Store.
func (s *PostgresStore) ActiveListings(ctx context.Context, kind string, subscriberID uuid.UUID, now time.Time) (int, error) {
	where := `organisation_id = $1`
	if kind == KindIndividual {
		where = `((seller_user_id = $1 AND organisation_id IS NULL) OR agent_user_id = $1)`
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM listings WHERE `+where+` AND status IN ('PUBLISHED', 'PAUSED') AND expires_at > $2`,
		subscriberID, now).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active listings: %w", err)
	}
	return n, nil
}
