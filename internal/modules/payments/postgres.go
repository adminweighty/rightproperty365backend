package payments

import (
	"context"
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

// PostgresStore keeps payments in payments, payment_refunds and
// payment_events (migration 0044).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *PostgresStore) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
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

const paymentColumns = `id, subscription_id, invoice_number, subscriber_kind, subscriber_id, subscriber_name, market_code, provider,
	COALESCE(provider_ref, ''), COALESCE(payment_ref, ''), idempotency_key, round(amount, 2)::text, currency_code,
	COALESCE(round(paid_amount, 2)::text, ''), COALESCE(paid_currency_code, ''), round(refunded_amount, 2)::text, status, hosted_url,
	expires_at, return_url, cancel_url, review_reason, failure_reason, opened_by, opened_by_name, created_at, updated_at, succeeded_at,
	failed_at, expired_at, refunded_at`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	if err := row.Scan(&p.ID, &p.SubscriptionID, &p.InvoiceNumber, &p.SubscriberKind, &p.SubscriberID, &p.SubscriberName, &p.MarketCode,
		&p.Provider, &p.ProviderRef, &p.PaymentRef, &p.IdempotencyKey, &p.Amount, &p.Currency, &p.PaidAmount, &p.PaidCurrency,
		&p.RefundedAmount, &p.Status, &p.HostedURL, &p.ExpiresAt, &p.ReturnURL, &p.CancelURL, &p.ReviewReason, &p.FailureReason,
		&p.OpenedBy, &p.OpenedByName, &p.CreatedAt, &p.UpdatedAt, &p.SucceededAt, &p.FailedAt, &p.ExpiredAt, &p.RefundedAt); err != nil {
		return Payment{}, err
	}
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	for _, t := range []**time.Time{&p.ExpiresAt, &p.SucceededAt, &p.FailedAt, &p.ExpiredAt, &p.RefundedAt} {
		if *t != nil {
			u := (**t).UTC()
			*t = &u
		}
	}
	return p, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func insertPayment(ctx context.Context, q querier, p Payment) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO payments (id, subscription_id, invoice_number, subscriber_kind, subscriber_id, subscriber_name, market_code, provider,
			provider_ref, payment_ref, idempotency_key, amount, currency_code, paid_amount, paid_currency_code, refunded_amount, status,
			hosted_url, expires_at, return_url, cancel_url, review_reason, failure_reason, opened_by, opened_by_name, created_at, updated_at,
			succeeded_at, failed_at, expired_at, refunded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::numeric, $13, $14::numeric, $15, COALESCE($16::numeric, 0), $17,
			$18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31)`,
		p.ID, p.SubscriptionID, p.InvoiceNumber, p.SubscriberKind, p.SubscriberID, p.SubscriberName, p.MarketCode, p.Provider,
		nullable(p.ProviderRef), nullable(p.PaymentRef), p.IdempotencyKey, p.Amount, p.Currency, nullable(p.PaidAmount), nullable(p.PaidCurrency),
		nullable(p.RefundedAmount), p.Status, p.HostedURL, p.ExpiresAt, p.ReturnURL, p.CancelURL, p.ReviewReason, p.FailureReason, p.OpenedBy,
		p.OpenedByName, p.CreatedAt, p.UpdatedAt, p.SucceededAt, p.FailedAt, p.ExpiredAt, p.RefundedAt); err != nil {
		return fmt.Errorf("insert payment: %w", err)
	}
	return nil
}

func updatePayment(ctx context.Context, q querier, p Payment) error {
	if _, err := q.Exec(ctx, `
		UPDATE payments SET provider_ref = $2, payment_ref = $3, paid_amount = $4::numeric, paid_currency_code = $5,
			refunded_amount = COALESCE($6::numeric, 0), status = $7, hosted_url = $8, expires_at = $9, review_reason = $10,
			failure_reason = $11, opened_by = $12, opened_by_name = $13, updated_at = $14, succeeded_at = $15, failed_at = $16,
			expired_at = $17, refunded_at = $18
		WHERE id = $1`,
		p.ID, nullable(p.ProviderRef), nullable(p.PaymentRef), nullable(p.PaidAmount), nullable(p.PaidCurrency), nullable(p.RefundedAmount),
		p.Status, p.HostedURL, p.ExpiresAt, p.ReviewReason, p.FailureReason, p.OpenedBy, p.OpenedByName, p.UpdatedAt, p.SucceededAt,
		p.FailedAt, p.ExpiredAt, p.RefundedAt); err != nil {
		return fmt.Errorf("update payment: %w", err)
	}
	return nil
}

// ReserveCheckout implements Store. The subscription row is locked, so two
// requests for the same invoice are decided one after the other.
func (s *PostgresStore) ReserveCheckout(ctx context.Context, p Payment, now time.Time) (Payment, bool, error) {
	var out Payment
	var reused bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM subscriptions WHERE id = $1 FOR UPDATE`, p.SubscriptionID); err != nil {
			return fmt.Errorf("lock subscription: %w", err)
		}
		var open *Payment
		cur, err := scanPayment(tx.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE subscription_id = $1 AND status = 'OPEN'
			FOR UPDATE`, p.SubscriptionID))
		switch {
		case err == nil:
			open = &cur
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("find open checkout: %w", err)
		}
		reuse, retired, err := reserve(open, now)
		if err != nil {
			return err
		}
		if reuse {
			out, reused = *open, true
			return nil
		}
		if retired != nil {
			if err := updatePayment(ctx, tx, *retired); err != nil {
				return err
			}
		}
		if err := insertPayment(ctx, tx, p); err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, reused, err
}

func payment(ctx context.Context, q querier, where string, args ...any) (Payment, error) {
	p, err := scanPayment(q.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE `+where, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrPaymentNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("load payment: %w", err)
	}
	return p, nil
}

// Payment implements Store.
func (s *PostgresStore) Payment(ctx context.Context, id uuid.UUID) (Payment, error) {
	return payment(ctx, s.pool, `id = $1`, id)
}

// PaymentByRef implements Store.
func (s *PostgresStore) PaymentByRef(ctx context.Context, provider, ref string) (Payment, error) {
	if ref == "" {
		return Payment{}, ErrPaymentNotFound
	}
	return payment(ctx, s.pool, `provider = $1 AND (provider_ref = $2 OR payment_ref = $2) ORDER BY created_at DESC LIMIT 1`, provider, ref)
}

func likePattern(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q)) + "%"
}

func filterArgs(f Filter) (string, []any) {
	where := `($1::text[] IS NULL OR status = ANY($1)) AND ($2 = '' OR provider = $2) AND ($3::uuid IS NULL OR subscription_id = $3)
		AND ($4 = '' OR lower(invoice_number) LIKE $4 OR lower(subscriber_name) LIKE $4 OR lower(COALESCE(provider_ref, '')) LIKE $4
			OR lower(COALESCE(payment_ref, '')) LIKE $4)
		AND ($5::timestamptz IS NULL OR updated_at >= $5) AND ($6::timestamptz IS NULL OR created_at >= $6)
		AND ($7::timestamptz IS NULL OR created_at < $7)`
	var statuses []string
	if len(f.Statuses) > 0 {
		statuses = append([]string{}, f.Statuses...)
	}
	opt := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		return &t
	}
	args := []any{statuses, f.Provider, f.SubscriptionID, likePattern(f.Q), opt(f.UpdatedSince), opt(f.CreatedFrom), opt(f.CreatedTo)}
	if statuses == nil {
		args[0] = nil
	}
	return where, args
}

// Payments implements Store.
func (s *PostgresStore) Payments(ctx context.Context, f Filter) ([]Payment, int, error) {
	where, args := filterArgs(f)
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM payments WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count payments: %w", err)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+paymentColumns+` FROM payments WHERE `+where+`
		ORDER BY created_at DESC, id DESC LIMIT $8 OFFSET $9`, append(args, limit, max(f.Offset, 0))...)
	if err != nil {
		return nil, 0, fmt.Errorf("list payments: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payment, error) { return scanPayment(row) })
	if err != nil {
		return nil, 0, fmt.Errorf("list payments: %w", err)
	}
	if out == nil {
		out = []Payment{}
	}
	return out, total, nil
}

// ChangePayment implements Store.
func (s *PostgresStore) ChangePayment(ctx context.Context, id uuid.UUID, change func(p *Payment) ([]adminaudit.Event, error)) (Payment, error) {
	var out Payment
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := payment(ctx, tx, `id = $1 FOR UPDATE`, id)
		if err != nil {
			return err
		}
		next := cur
		audits, err := change(&next)
		if errors.Is(err, errNoChange) {
			out = cur
			return nil
		}
		if err != nil {
			return err
		}
		if err := updatePayment(ctx, tx, next); err != nil {
			return err
		}
		for _, a := range audits {
			if err := adminaudit.Insert(ctx, tx, a); err != nil {
				return err
			}
		}
		out = next
		return nil
	})
	return out, err
}

// ClaimEvent implements Store: the unique (provider, event_id) makes the
// first delivery the only inserter; a later one claims it only after the
// lease of an unfinished one has run out.
func (s *PostgresStore) ClaimEvent(ctx context.Context, e EventRecord, now time.Time) (bool, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO payment_events (id, provider, event_id, event_type, provider_ref, payment_ref, refund_ref, amount, currency_code,
			occurred_at, received_at, processing_started_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11, $11)
		ON CONFLICT (provider, event_id) DO NOTHING
		RETURNING id`,
		e.ID, e.Provider, e.EventID, e.Type, e.ProviderRef, e.PaymentRef, e.RefundRef, nullableAmount(e.Amount), e.Currency,
		nullableTime(e.OccurredAt), now).Scan(&id)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("record payment event: %w", err)
	}
	err = s.pool.QueryRow(ctx, `
		UPDATE payment_events SET processing_started_at = $3
		WHERE provider = $1 AND event_id = $2 AND processed_at IS NULL
		  AND (processing_started_at IS NULL OR processing_started_at < $4)
		RETURNING id`, e.Provider, e.EventID, now, now.Add(-EventLease)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim payment event: %w", err)
	}
	return true, nil
}

func nullableAmount(s string) *string {
	if s == "" || !amountLike(s) {
		return nil
	}
	return &s
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// FinishEvent implements Store.
func (s *PostgresStore) FinishEvent(ctx context.Context, provider, eventID, outcome string, paymentID *uuid.UUID, at time.Time) error {
	if _, err := s.pool.Exec(ctx, `UPDATE payment_events SET processed_at = $3, outcome = $4, payment_id = $5 WHERE provider = $1 AND event_id = $2`,
		provider, eventID, at, outcome, paymentID); err != nil {
		return fmt.Errorf("finish payment event: %w", err)
	}
	return nil
}

// ReleaseEvent implements Store.
func (s *PostgresStore) ReleaseEvent(ctx context.Context, provider, eventID string) error {
	if _, err := s.pool.Exec(ctx, `UPDATE payment_events SET processing_started_at = NULL
		WHERE provider = $1 AND event_id = $2 AND processed_at IS NULL`, provider, eventID); err != nil {
		return fmt.Errorf("release payment event: %w", err)
	}
	return nil
}

const refundColumns = `id, payment_id, round(amount, 2)::text, currency_code, reason, status, COALESCE(provider_refund_ref, ''), idempotency_key,
	requested_by, requested_by_name, failure_reason, created_at, completed_at`

func scanRefund(row pgx.Row) (Refund, error) {
	var r Refund
	if err := row.Scan(&r.ID, &r.PaymentID, &r.Amount, &r.Currency, &r.Reason, &r.Status, &r.ProviderRefundRef, &r.IdempotencyKey,
		&r.RequestedBy, &r.RequestedByName, &r.FailureReason, &r.CreatedAt, &r.CompletedAt); err != nil {
		return Refund{}, err
	}
	r.CreatedAt = r.CreatedAt.UTC()
	if r.CompletedAt != nil {
		t := r.CompletedAt.UTC()
		r.CompletedAt = &t
	}
	return r, nil
}

func readRefunds(ctx context.Context, q querier, where string, args ...any) ([]Refund, error) {
	rows, err := q.Query(ctx, `SELECT `+refundColumns+` FROM payment_refunds WHERE `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read refunds: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Refund, error) { return scanRefund(row) })
	if err != nil {
		return nil, fmt.Errorf("read refunds: %w", err)
	}
	if out == nil {
		out = []Refund{}
	}
	return out, nil
}

// CreateRefund implements Store.
func (s *PostgresStore) CreateRefund(ctx context.Context, r Refund, check func(p Payment, refunds []Refund) error) (Refund, bool, error) {
	var out Refund
	var existing bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := payment(ctx, tx, `id = $1 FOR UPDATE`, r.PaymentID)
		if err != nil {
			return err
		}
		same, err := readRefunds(ctx, tx, `idempotency_key = $1`, r.IdempotencyKey)
		if err != nil {
			return err
		}
		if len(same) > 0 {
			out, existing = same[0], true
			return nil
		}
		refunds, err := readRefunds(ctx, tx, `payment_id = $1`, p.ID)
		if err != nil {
			return err
		}
		if check != nil {
			if err := check(p, refunds); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment_refunds (id, payment_id, amount, currency_code, reason, status, provider_refund_ref, idempotency_key,
				requested_by, requested_by_name, failure_reason, created_at, completed_at)
			VALUES ($1, $2, $3::numeric, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			r.ID, r.PaymentID, r.Amount, r.Currency, r.Reason, r.Status, nullable(r.ProviderRefundRef), r.IdempotencyKey, r.RequestedBy,
			r.RequestedByName, r.FailureReason, r.CreatedAt, r.CompletedAt); err != nil {
			return fmt.Errorf("insert refund: %w", err)
		}
		out = r
		return nil
	})
	return out, existing, err
}

// ChangeRefund implements Store.
func (s *PostgresStore) ChangeRefund(ctx context.Context, id uuid.UUID, change func(r *Refund, p *Payment) ([]adminaudit.Event, error)) (Refund, Payment, error) {
	var outR Refund
	var outP Payment
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var paymentID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT payment_id FROM payment_refunds WHERE id = $1`, id).Scan(&paymentID); errors.Is(err, pgx.ErrNoRows) {
			return ErrRefundNotFound
		} else if err != nil {
			return fmt.Errorf("load refund: %w", err)
		}
		p, err := payment(ctx, tx, `id = $1 FOR UPDATE`, paymentID)
		if err != nil {
			return err
		}
		r, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM payment_refunds WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return fmt.Errorf("load refund: %w", err)
		}
		nr, np := r, p
		audits, err := change(&nr, &np)
		if errors.Is(err, errNoChange) {
			outR, outP = r, p
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_refunds SET status = $2, provider_refund_ref = $3, failure_reason = $4, completed_at = $5,
			idempotency_key = $6 WHERE id = $1`, nr.ID, nr.Status, nullable(nr.ProviderRefundRef), nr.FailureReason, nr.CompletedAt,
			nr.IdempotencyKey); err != nil {
			return fmt.Errorf("update refund: %w", err)
		}
		if err := updatePayment(ctx, tx, np); err != nil {
			return err
		}
		for _, a := range audits {
			if err := adminaudit.Insert(ctx, tx, a); err != nil {
				return err
			}
		}
		outR, outP = nr, np
		return nil
	})
	return outR, outP, err
}

// RefundByProviderRef implements Store.
func (s *PostgresStore) RefundByProviderRef(ctx context.Context, paymentID uuid.UUID, ref string) (Refund, error) {
	if ref == "" {
		return Refund{}, ErrRefundNotFound
	}
	list, err := readRefunds(ctx, s.pool, `payment_id = $1 AND provider_refund_ref = $2`, paymentID, ref)
	if err != nil {
		return Refund{}, err
	}
	if len(list) == 0 {
		return Refund{}, ErrRefundNotFound
	}
	return list[0], nil
}

// Refunds implements Store.
func (s *PostgresStore) Refunds(ctx context.Context, paymentIDs []uuid.UUID) (map[uuid.UUID][]Refund, error) {
	out := map[uuid.UUID][]Refund{}
	for _, id := range paymentIDs {
		out[id] = []Refund{}
	}
	if len(paymentIDs) == 0 {
		return out, nil
	}
	list, err := readRefunds(ctx, s.pool, `payment_id = ANY($1)`, paymentIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		out[r.PaymentID] = append(out[r.PaymentID], r)
	}
	return out, nil
}

// ForgetUserTx handles a deleted account's payment rows inside the account
// deletion's transaction (the users row is anonymised, not deleted, so no ON
// DELETE fires): the payments and refunds are KEPT for finance, the person
// is unlinked as who opened a checkout or asked for a refund (their name
// replaced), and their own open checkouts are expired.
func ForgetUserTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, deletedName string, at time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE payments SET opened_by = NULL, opened_by_name = $2 WHERE opened_by = $1`, userID, deletedName); err != nil {
		return fmt.Errorf("unlink payment opener: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET status = 'EXPIRED', expired_at = $2, updated_at = $2, hosted_url = ''
		WHERE status = 'OPEN' AND subscriber_kind = 'INDIVIDUAL' AND subscriber_id = $1`, userID, at); err != nil {
		return fmt.Errorf("expire open checkouts: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_refunds SET requested_by = NULL, requested_by_name = $2 WHERE requested_by = $1`,
		userID, deletedName); err != nil {
		return fmt.Errorf("unlink refund requester: %w", err)
	}
	return nil
}
