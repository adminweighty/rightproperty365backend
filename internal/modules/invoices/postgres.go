package invoices

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
	"rightproperty365.com/backend/internal/platform/outbox"
)

// PostgresStore keeps invoices in Postgres (migration 0049).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a store on pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// outboxEvent is the outbox row of a change's event.
func outboxEvent(id uuid.UUID, o OutboxEvent, at time.Time) outbox.Event {
	payload, _ := json.Marshal(o.Payload)
	return outbox.Event{ID: uuid.New(), Type: o.Type, AggregateType: AggregateInvoice, AggregateID: id, Payload: payload, Attempts: 1, CreatedAt: at}
}

const invoiceColumns = `id, COALESCE(number, ''), status, COALESCE(status_before_dispute, ''), market_code, currency_code, subscriber_kind,
	subscriber_id, organisation_id, payer_name, renews_subscription_id, subscription_id, plan_id, plan_name, period, service_from, service_to,
	issue_on, due_on, terms, subtotal::text, tax_name, tax_rate::text, tax_inclusive, tax_amount::text, tax_is_sample, total::text,
	amount_paid::text, amount_credited::text, COALESCE(void_reason, ''), COALESCE(dispute_reason, ''), replaces_invoice_id, sent_at,
	last_sent_at, send_count, overdue_notified_at, created_by, created_by_name, issued_by, created_at, updated_at`

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func scanInvoice(row pgx.Row) (Invoice, error) {
	var (
		inv                    Invoice
		taxName, taxRate       *string
		taxInclusive           *bool
		taxAmount              string
		taxSample              bool
		serviceFrom, serviceTo time.Time
		issueOn                *time.Time
		dueOn                  time.Time
	)
	err := row.Scan(&inv.ID, &inv.Number, &inv.Status, &inv.StatusBeforeDispute, &inv.MarketCode, &inv.CurrencyCode, &inv.SubscriberKind,
		&inv.SubscriberID, &inv.OrganisationID, &inv.PayerName, &inv.RenewsSubscriptionID, &inv.SubscriptionID, &inv.PlanID, &inv.PlanName,
		&inv.Period, &serviceFrom, &serviceTo, &issueOn, &dueOn, &inv.Terms, &inv.Subtotal, &taxName, &taxRate, &taxInclusive, &taxAmount,
		&taxSample, &inv.Total, &inv.AmountPaid, &inv.AmountCredited, &inv.VoidReason, &inv.DisputeReason, &inv.ReplacesInvoiceID,
		&inv.SentAt, &inv.LastSentAt, &inv.SendCount, &inv.OverdueNotifiedAt, &inv.CreatedBy, &inv.CreatedByName, &inv.IssuedBy,
		&inv.CreatedAt, &inv.UpdatedAt)
	if err != nil {
		return Invoice{}, err
	}
	inv.ServiceFrom, inv.ServiceTo, inv.DueOn = day(serviceFrom), day(serviceTo), day(dueOn)
	if issueOn != nil {
		d := day(*issueOn)
		inv.IssueOn = &d
	}
	if taxName != nil && taxRate != nil {
		inv.Tax = &Tax{Name: *taxName, Rate: *taxRate, Inclusive: taxInclusive != nil && *taxInclusive, Amount: taxAmount, IsSample: taxSample}
	}
	inv.CreatedAt, inv.UpdatedAt = inv.CreatedAt.UTC(), inv.UpdatedAt.UTC()
	for _, t := range []**time.Time{&inv.SentAt, &inv.LastSentAt, &inv.OverdueNotifiedAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	return inv, nil
}

func lines(ctx context.Context, q queryer, id uuid.UUID) ([]Line, error) {
	rows, err := q.Query(ctx, `SELECT id, description, quantity, unit_amount::text, amount::text FROM invoice_lines WHERE invoice_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Line, error) {
		var l Line
		err := row.Scan(&l.ID, &l.Description, &l.Quantity, &l.UnitAmount, &l.Amount)
		return l, err
	})
	if out == nil {
		out = []Line{}
	}
	return out, err
}

func get(ctx context.Context, q queryer, id uuid.UUID, lock bool) (Invoice, error) {
	sql := `SELECT ` + invoiceColumns + ` FROM invoices WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	inv, err := scanInvoice(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	if err != nil {
		return Invoice{}, err
	}
	if inv.Lines, err = lines(ctx, q, id); err != nil {
		return Invoice{}, err
	}
	return inv, nil
}

func taxArgs(t *Tax) (name, rate *string, inclusive *bool, amount string, sample bool) {
	if t == nil {
		return nil, nil, nil, "0", false
	}
	n, r, i := t.Name, t.Rate, t.Inclusive
	return &n, &r, &i, t.Amount, t.IsSample
}

func numberOrNil(n string) *string {
	if n == "" {
		return nil
	}
	return &n
}

func textOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func writeLines(ctx context.Context, tx pgx.Tx, inv Invoice) error {
	if _, err := tx.Exec(ctx, `DELETE FROM invoice_lines WHERE invoice_id = $1`, inv.ID); err != nil {
		return err
	}
	for i, l := range inv.Lines {
		if _, err := tx.Exec(ctx, `INSERT INTO invoice_lines (id, invoice_id, position, description, quantity, unit_amount, amount)
			VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric)`, l.ID, inv.ID, i, l.Description, l.Quantity, l.UnitAmount, l.Amount); err != nil {
			return fmt.Errorf("insert invoice line: %w", err)
		}
	}
	return nil
}

func insertEvents(ctx context.Context, tx pgx.Tx, evs []Event) error {
	for _, e := range evs {
		if _, err := tx.Exec(ctx, `INSERT INTO invoice_events (id, invoice_id, action, actor_type, actor_user_id, actor_name, note, at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, e.ID, e.InvoiceID, e.Action, e.ActorType, e.ActorUserID, e.ActorName, e.Note, e.At); err != nil {
			return fmt.Errorf("insert invoice event: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Create implements Store.
func (s *PostgresStore) Create(ctx context.Context, inv Invoice, ev Event, a adminaudit.Event) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		name, rate, incl, amount, sample := taxArgs(inv.Tax)
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoices (id, number, status, market_code, currency_code, subscriber_kind, subscriber_id, organisation_id, payer_name,
				renews_subscription_id, plan_id, plan_name, period, service_from, service_to, due_on, terms, subtotal, tax_name, tax_rate,
				tax_inclusive, tax_amount, tax_is_sample, total, amount_paid, amount_credited, replaces_invoice_id, created_by, created_by_name,
				created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18::numeric, $19, $20::numeric, $21,
				$22::numeric, $23, $24::numeric, $25::numeric, $26::numeric, $27, $28, $29, $30, $31)`,
			inv.ID, numberOrNil(inv.Number), inv.Status, inv.MarketCode, inv.CurrencyCode, inv.SubscriberKind, inv.SubscriberID, inv.OrganisationID,
			inv.PayerName, inv.RenewsSubscriptionID, inv.PlanID, inv.PlanName, inv.Period, inv.ServiceFrom, inv.ServiceTo, inv.DueOn, inv.Terms,
			inv.Subtotal, name, rate, incl, amount, sample, inv.Total, inv.AmountPaid, inv.AmountCredited, inv.ReplacesInvoiceID, inv.CreatedBy,
			inv.CreatedByName, inv.CreatedAt, inv.UpdatedAt); err != nil {
			return fmt.Errorf("insert invoice: %w", err)
		}
		if err := writeLines(ctx, tx, inv); err != nil {
			return err
		}
		if err := insertEvents(ctx, tx, []Event{ev}); err != nil {
			return err
		}
		return adminaudit.Insert(ctx, tx, a)
	})
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Invoice, error) {
	return get(ctx, s.pool, id, false)
}

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, f Filter) ([]Invoice, int, error) {
	var where []string
	var args []any
	add := func(clause string, arg any) {
		args = append(args, arg)
		where = append(where, strings.ReplaceAll(clause, "?", fmt.Sprintf("$%d", len(args))))
	}
	if len(f.Statuses) > 0 {
		add("status = ANY(?::text[])", f.Statuses)
	}
	if f.ExcludeDrafts {
		where = append(where, "status <> 'DRAFT'")
	}
	if f.Market != "" {
		add("market_code = ?", f.Market)
	}
	if f.Subscribers != nil {
		add("subscriber_id = ANY(?::uuid[])", f.Subscribers)
	}
	if f.RenewsSubscription != nil {
		add("renews_subscription_id = ?", *f.RenewsSubscription)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		pattern := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		add(`(COALESCE(number, '') ILIKE ? ESCAPE '\' OR payer_name ILIKE ? ESCAPE '\')`, pattern)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM invoices`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = MaxPageSize
	}
	args = append(args, limit, max(f.Offset, 0))
	rows, err := s.pool.Query(ctx, `SELECT `+invoiceColumns+` FROM invoices`+clause+
		fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Invoice, error) { return scanInvoice(row) })
	if err != nil {
		return nil, 0, err
	}
	if out == nil {
		out = []Invoice{}
	}
	return out, total, nil
}

// next takes the next number of a kind under the counter's row lock.
func next(ctx context.Context, tx pgx.Tx, kind, market string, year int) (string, error) {
	var n int
	if err := tx.QueryRow(ctx, `
		INSERT INTO invoice_counters (kind, market_code, year, last) VALUES ($1, $2, $3, 1)
		ON CONFLICT (kind, market_code, year) DO UPDATE SET last = invoice_counters.last + 1
		RETURNING last`, kind, market, year).Scan(&n); err != nil {
		return "", fmt.Errorf("next %s number: %w", kind, err)
	}
	return FormatNumber(kind, market, year, n), nil
}

// Change implements Store.
func (s *PostgresStore) Change(ctx context.Context, id uuid.UUID, change func(inv *Invoice) (Change, error)) (Invoice, error) {
	err := s.tx(ctx, func(tx pgx.Tx) error {
		x, err := get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		ch, err := change(&x)
		if err != nil {
			return err
		}
		year := x.UpdatedAt.UTC().Year()
		if ch.Number && x.Number == "" {
			if x.Number, err = next(ctx, tx, KindInvoice, x.MarketCode, year); err != nil {
				return err
			}
		}
		name, rate, incl, amount, sample := taxArgs(x.Tax)
		if _, err := tx.Exec(ctx, `
			UPDATE invoices SET number = $2, status = $3, status_before_dispute = $4, subscription_id = $5, period = $6, service_to = $7,
				issue_on = $8, due_on = $9, terms = $10, subtotal = $11::numeric, tax_name = $12, tax_rate = $13::numeric, tax_inclusive = $14,
				tax_amount = $15::numeric, tax_is_sample = $16, total = $17::numeric, amount_paid = $18::numeric, amount_credited = $19::numeric,
				void_reason = $20, dispute_reason = $21, sent_at = $22, last_sent_at = $23, send_count = $24, overdue_notified_at = $25,
				issued_by = $26, updated_at = $27
			WHERE id = $1`,
			id, numberOrNil(x.Number), x.Status, textOrNil(x.StatusBeforeDispute), x.SubscriptionID, x.Period, x.ServiceTo, x.IssueOn, x.DueOn,
			x.Terms, x.Subtotal, name, rate, incl, amount, sample, x.Total, x.AmountPaid, x.AmountCredited, textOrNil(x.VoidReason),
			textOrNil(x.DisputeReason), x.SentAt, x.LastSentAt, x.SendCount, x.OverdueNotifiedAt, x.IssuedBy, x.UpdatedAt); err != nil {
			return fmt.Errorf("update invoice: %w", err)
		}
		if ch.Lines {
			if err := writeLines(ctx, tx, x); err != nil {
				return err
			}
		}
		if p := ch.Payment; p != nil {
			if p.ReceiptNumber, err = next(ctx, tx, KindReceipt, x.MarketCode, year); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO invoice_payments (id, invoice_id, receipt_number, amount, method, reference, received_on, source_key, recorded_by,
					recorded_by_name, created_at)
				VALUES ($1, $2, $3, $4::numeric, $5, $6, $7, $8, $9, $10, $11)`,
				p.ID, id, p.ReceiptNumber, p.Amount, p.Method, p.Reference, p.ReceivedOn, textOrNil(p.SourceKey), p.RecordedBy, p.RecordedByName,
				p.CreatedAt); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.ConstraintName == "invoice_payments_source_unique" {
					return ErrDuplicatePayment
				}
				return fmt.Errorf("insert invoice payment: %w", err)
			}
		}
		if c := ch.Credit; c != nil {
			if c.Number, err = next(ctx, tx, KindCredit, x.MarketCode, year); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO credit_notes (id, invoice_id, number, amount, reason, created_by, created_by_name, created_at)
				VALUES ($1, $2, $3, $4::numeric, $5, $6, $7, $8)`, c.ID, id, c.Number, c.Amount, c.Reason, c.CreatedBy, c.CreatedByName, c.CreatedAt); err != nil {
				return fmt.Errorf("insert credit note: %w", err)
			}
		}
		if err := insertEvents(ctx, tx, ch.Events); err != nil {
			return err
		}
		for _, a := range ch.Audit {
			if a.TargetLabel == "Draft for "+x.PayerName && x.Number != "" {
				a.TargetLabel = x.Number
			}
			if err := adminaudit.Insert(ctx, tx, a); err != nil {
				return err
			}
		}
		for _, o := range ch.Outbox {
			ev := outboxEvent(id, o, x.UpdatedAt)
			if _, err := tx.Exec(ctx, `
				INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
				VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`, ev.ID, ev.Type, ev.AggregateType, ev.AggregateID, string(ev.Payload), ev.CreatedAt); err != nil {
				return fmt.Errorf("insert outbox event: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return Invoice{}, err
	}
	return s.Get(ctx, id)
}

// Delete implements Store.
func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID, a adminaudit.Event) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM invoices WHERE id = $1 AND status = 'DRAFT'`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrState
		}
		return adminaudit.Insert(ctx, tx, a)
	})
}

// Payments implements Store.
func (s *PostgresStore) Payments(ctx context.Context, id uuid.UUID) ([]Payment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, invoice_id, receipt_number, amount::text, method, reference, received_on, COALESCE(source_key, ''), recorded_by,
		       recorded_by_name, created_at
		FROM invoice_payments WHERE invoice_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Payment, error) {
		var p Payment
		err := row.Scan(&p.ID, &p.InvoiceID, &p.ReceiptNumber, &p.Amount, &p.Method, &p.Reference, &p.ReceivedOn, &p.SourceKey, &p.RecordedBy,
			&p.RecordedByName, &p.CreatedAt)
		p.ReceivedOn, p.CreatedAt = day(p.ReceivedOn), p.CreatedAt.UTC()
		return p, err
	})
	if out == nil {
		out = []Payment{}
	}
	return out, err
}

// Credits implements Store.
func (s *PostgresStore) Credits(ctx context.Context, id uuid.UUID) ([]CreditNote, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, invoice_id, number, amount::text, reason, created_by, created_by_name, created_at
		FROM credit_notes WHERE invoice_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (CreditNote, error) {
		var c CreditNote
		err := row.Scan(&c.ID, &c.InvoiceID, &c.Number, &c.Amount, &c.Reason, &c.CreatedBy, &c.CreatedByName, &c.CreatedAt)
		c.CreatedAt = c.CreatedAt.UTC()
		return c, err
	})
	if out == nil {
		out = []CreditNote{}
	}
	return out, err
}

// Events implements Store.
func (s *PostgresStore) Events(ctx context.Context, id uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, invoice_id, action, actor_type, actor_user_id, actor_name, note, at
		FROM invoice_events WHERE invoice_id = $1 ORDER BY at, id`, id)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.ID, &e.InvoiceID, &e.Action, &e.ActorType, &e.ActorUserID, &e.ActorName, &e.Note, &e.At)
		e.At = e.At.UTC()
		return e, err
	})
	if out == nil {
		out = []Event{}
	}
	return out, err
}

// BySubscription implements Store.
func (s *PostgresStore) BySubscription(ctx context.Context, subscriptionID uuid.UUID) (Invoice, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM invoices WHERE subscription_id = $1`, subscriptionID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, ErrNotFound
	}
	if err != nil {
		return Invoice{}, err
	}
	return s.Get(ctx, id)
}

// DueCandidates implements Store.
func (s *PostgresStore) DueCandidates(ctx context.Context, d time.Time) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM invoices WHERE status IN ('ISSUED', 'PART_PAID') AND due_on <= $1::date ORDER BY due_on, id`, d)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
