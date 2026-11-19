package tenancies

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/platform/money"
)

// Rent rows (migration 0036). The guards: one due per tenancy and due date
// (rent_dues_tenancy_date_key), composite foreign keys that keep an
// allocation's due and receipt on the same tenancy, one receipt per ledger
// entry. Over-allocation is refused under row locks: a receipt's change
// locks its tenancy (ExpectStatus) and every due it allocates to.

// ActiveTenancies implements Store.
func (s *PostgresStore) ActiveTenancies(ctx context.Context) ([]Tenancy, error) {
	return s.tenancies(ctx, `SELECT `+tenancyColumns+` FROM tenancies t WHERE t.status = 'ACTIVE' ORDER BY t.id`)
}

const dueColumns = `d.id, d.tenancy_id, d.sequence, d.due_date::text, d.amount::text, d.currency, d.waived_at, d.waived_by, d.waived_by_name,
	COALESCE(d.waive_reason, ''), d.created_at`

func scanDue(row pgx.Row) (Due, error) {
	var d Due
	if err := row.Scan(&d.ID, &d.TenancyID, &d.Sequence, &d.DueDate, &d.Amount, &d.Currency, &d.WaivedAt, &d.WaivedBy, &d.WaivedByName,
		&d.WaiveReason, &d.CreatedAt); err != nil {
		return Due{}, err
	}
	d.Currency = strings.TrimSpace(d.Currency)
	d.Amount = money.Normalise(d.Amount, d.Currency)
	d.CreatedAt = d.CreatedAt.UTC()
	if d.WaivedAt != nil {
		t := d.WaivedAt.UTC()
		d.WaivedAt = &t
	}
	d.Allocations = []DueAllocation{}
	return d, nil
}

func (s *PostgresStore) dues(ctx context.Context, sql string, args ...any) ([]Due, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read rent dues: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Due, error) { return scanDue(row) })
	if err != nil {
		return nil, fmt.Errorf("read rent dues: %w", err)
	}
	if len(list) == 0 {
		return list, nil
	}
	ids := make([]uuid.UUID, 0, len(list))
	at := map[uuid.UUID]int{}
	for i, d := range list {
		ids = append(ids, d.ID)
		at[d.ID] = i
	}
	rows, err = s.pool.Query(ctx, `SELECT a.due_id, a.receipt_id, a.amount::text, r.received_on::text, COALESCE(r.reversed_on::text, '')
		FROM rent_allocations a JOIN rent_receipts r ON r.id = a.receipt_id
		WHERE a.due_id = ANY($1) ORDER BY r.received_on, r.recorded_at, a.receipt_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("read rent allocations: %w", err)
	}
	type row struct {
		due uuid.UUID
		a   DueAllocation
	}
	allocs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.due, &x.a.ReceiptID, &x.a.Amount, &x.a.ReceivedOn, &x.a.ReversedOn)
		return x, err
	})
	if err != nil {
		return nil, fmt.Errorf("read rent allocations: %w", err)
	}
	for _, x := range allocs {
		d := &list[at[x.due]]
		x.a.Amount = money.Normalise(x.a.Amount, d.Currency)
		d.Allocations = append(d.Allocations, x.a)
	}
	return list, nil
}

// Dues implements Store.
func (s *PostgresStore) Dues(ctx context.Context, tenancyIDs []uuid.UUID) ([]Due, error) {
	if len(tenancyIDs) == 0 {
		return nil, nil
	}
	return s.dues(ctx, `SELECT `+dueColumns+` FROM rent_dues d WHERE d.tenancy_id = ANY($1) ORDER BY d.tenancy_id, d.due_date`, tenancyIDs)
}

// Due implements Store.
func (s *PostgresStore) Due(ctx context.Context, id uuid.UUID) (Due, error) {
	list, err := s.dues(ctx, `SELECT `+dueColumns+` FROM rent_dues d WHERE d.id = $1`, id)
	if err != nil {
		return Due{}, err
	}
	if len(list) == 0 {
		return Due{}, ErrDueNotFound
	}
	return list[0], nil
}

// Receipts implements Store.
func (s *PostgresStore) Receipts(ctx context.Context, tenancyID uuid.UUID) ([]Receipt, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.id, r.tenancy_id, r.amount::text, r.currency, r.method, r.received_on::text, r.reference,
		r.contact_id, r.contact_name, r.ledger_entry_id, r.recorded_by, r.recorded_by_name, r.recorded_at, r.reversed_at, r.reversal_entry_id,
		COALESCE(r.reversed_on::text, ''), e.status, e.author_user_id
		FROM rent_receipts r JOIN ledger_entries e ON e.id = r.ledger_entry_id
		WHERE r.tenancy_id = $1 ORDER BY r.received_on DESC, r.recorded_at DESC, r.id DESC`, tenancyID)
	if err != nil {
		return nil, fmt.Errorf("read rent receipts: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Receipt, error) {
		var r Receipt
		err := row.Scan(&r.ID, &r.TenancyID, &r.Amount, &r.Currency, &r.Method, &r.ReceivedOn, &r.Reference, &r.ContactID, &r.ContactName,
			&r.LedgerEntryID, &r.RecordedBy, &r.RecordedByName, &r.RecordedAt, &r.ReversedAt, &r.ReversalEntryID, &r.ReversedOn,
			&r.LedgerStatus, &r.LedgerAuthor)
		r.Currency = strings.TrimSpace(r.Currency)
		r.Amount = money.Normalise(r.Amount, r.Currency)
		r.RecordedAt = r.RecordedAt.UTC()
		if r.ReversedAt != nil {
			t := r.ReversedAt.UTC()
			r.ReversedAt = &t
		}
		r.Allocations = []Allocation{}
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("read rent receipts: %w", err)
	}
	if len(list) == 0 {
		return list, nil
	}
	ids := make([]uuid.UUID, 0, len(list))
	at := map[uuid.UUID]int{}
	for i, r := range list {
		ids = append(ids, r.ID)
		at[r.ID] = i
	}
	rows, err = s.pool.Query(ctx, `SELECT a.receipt_id, a.due_id, a.amount::text FROM rent_allocations a JOIN rent_dues d ON d.id = a.due_id
		WHERE a.receipt_id = ANY($1) ORDER BY d.due_date, a.due_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("read rent allocations: %w", err)
	}
	type row struct {
		receipt uuid.UUID
		a       Allocation
	}
	allocs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.receipt, &x.a.DueID, &x.a.Amount)
		return x, err
	})
	if err != nil {
		return nil, fmt.Errorf("read rent allocations: %w", err)
	}
	for _, x := range allocs {
		r := &list[at[x.receipt]]
		x.a.Amount = money.Normalise(x.a.Amount, r.Currency)
		r.Allocations = append(r.Allocations, x.a)
	}
	return list, nil
}

// livePaid sums the allocations to a due of the receipts that are not reversed.
func livePaid(ctx context.Context, tx pgx.Tx, dueID uuid.UUID) (string, error) {
	var paid string
	err := tx.QueryRow(ctx, `SELECT COALESCE(sum(a.amount), 0)::text FROM rent_allocations a JOIN rent_receipts r ON r.id = a.receipt_id
		WHERE a.due_id = $1 AND r.reversed_at IS NULL`, dueID).Scan(&paid)
	if err != nil {
		return "", fmt.Errorf("sum rent allocations: %w", err)
	}
	return paid, nil
}

// lockDue locks a due of the tenancy and returns its amount and whether it
// is waived; ErrDueChanged when it is not a due of the tenancy.
func lockDue(ctx context.Context, tx pgx.Tx, id, tenancyID uuid.UUID) (string, bool, error) {
	var amount string
	var waived bool
	err := tx.QueryRow(ctx, `SELECT amount::text, waived_at IS NOT NULL FROM rent_dues WHERE id = $1 AND tenancy_id = $2 FOR UPDATE`,
		id, tenancyID).Scan(&amount, &waived)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrDueChanged
	}
	if err != nil {
		return "", false, fmt.Errorf("lock rent due: %w", err)
	}
	return amount, waived, nil
}

// left is amount - paid.
func left(amount, paid string) string {
	r := money.Rat(amount)
	r.Sub(r, money.Rat(paid))
	return r.FloatString(4)
}

// applyRentTx writes the rent parts of a Change (see Change for the order).
func applyRentTx(ctx context.Context, tx pgx.Tx, ch Change) error {
	for _, d := range ch.Dues {
		if _, err := tx.Exec(ctx, `INSERT INTO rent_dues (id, tenancy_id, sequence, due_date, amount, currency, created_at)
			VALUES ($1, $2, $3, $4::date, $5::numeric, $6, $7) ON CONFLICT (tenancy_id, due_date) DO NOTHING`,
			d.ID, d.TenancyID, d.Sequence, d.DueDate, d.Amount, d.Currency, d.CreatedAt); err != nil {
			return fmt.Errorf("write rent due: %w", err)
		}
	}
	if len(ch.DeleteDues) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM rent_dues d WHERE d.id = ANY($1) AND d.waived_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM rent_allocations a WHERE a.due_id = d.id)`, ch.DeleteDues); err != nil {
			return fmt.Errorf("remove rent dues: %w", err)
		}
	}
	if w := ch.Waive; w != nil {
		var amount string
		var waived bool
		err := tx.QueryRow(ctx, `SELECT amount::text, waived_at IS NOT NULL FROM rent_dues WHERE id = $1 FOR UPDATE`, w.DueID).Scan(&amount, &waived)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDueNotFound
		}
		if err != nil {
			return fmt.Errorf("lock rent due: %w", err)
		}
		paid, err := livePaid(ctx, tx, w.DueID)
		if err != nil {
			return err
		}
		if waived || money.Rat(left(amount, paid)).Sign() <= 0 {
			return ErrDueChanged
		}
		if _, err := tx.Exec(ctx, `UPDATE rent_dues SET waived_at = $2, waived_by = $3, waived_by_name = $4, waive_reason = $5 WHERE id = $1`,
			w.DueID, w.At, w.By, w.ByName, w.Reason); err != nil {
			return fmt.Errorf("waive rent due: %w", err)
		}
	}
	for _, r := range ch.Receipts {
		if _, err := tx.Exec(ctx, `INSERT INTO rent_receipts (id, tenancy_id, amount, currency, method, received_on, reference, contact_id,
			contact_name, ledger_entry_id, recorded_by, recorded_by_name, recorded_at)
			VALUES ($1, $2, $3::numeric, $4, $5, $6::date, $7, $8, $9, $10, $11, $12, $13)`,
			r.ID, r.TenancyID, r.Amount, r.Currency, r.Method, r.ReceivedOn, r.Reference, r.ContactID, r.ContactName, r.LedgerEntryID,
			r.RecordedBy, r.RecordedByName, r.RecordedAt); err != nil {
			return fmt.Errorf("write rent receipt: %w", err)
		}
		lines := slices.Clone(r.Allocations)
		// Lock the dues in a stable order.
		slices.SortFunc(lines, func(a, b Allocation) int { return strings.Compare(a.DueID.String(), b.DueID.String()) })
		for _, a := range lines {
			amount, waived, err := lockDue(ctx, tx, a.DueID, r.TenancyID)
			if err != nil {
				return err
			}
			paid, err := livePaid(ctx, tx, a.DueID)
			if err != nil {
				return err
			}
			if waived || money.Rat(a.Amount).Cmp(money.Rat(left(amount, paid))) > 0 {
				return ErrDueChanged
			}
			if _, err := tx.Exec(ctx, `INSERT INTO rent_allocations (receipt_id, tenancy_id, due_id, amount) VALUES ($1, $2, $3, $4::numeric)`,
				r.ID, r.TenancyID, a.DueID, a.Amount); err != nil {
				return translateRent(fmt.Errorf("write rent allocation: %w", err))
			}
		}
	}
	return nil
}

func translateRent(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" && strings.HasPrefix(pg.ConstraintName, "rent_allocations_") {
		return ErrDueChanged
	}
	return err
}

// ReleaseReceiptsTx is the ledger Postgres store's reversal hook
// (ConnectLedger): inside the reversal's transaction, the receipt of each
// reversed entry is marked reversed, so its allocations stop counting.
func ReleaseReceiptsTx(ctx context.Context, tx pgx.Tx, list []ledger.Reversal) error {
	for _, r := range list {
		if _, err := tx.Exec(ctx, `UPDATE rent_receipts SET reversed_at = $2, reversal_entry_id = $3, reversed_on = $4::date
			WHERE ledger_entry_id = $1 AND reversed_at IS NULL`, r.EntryID, r.At, r.ReversalID, r.EffectiveDate); err != nil {
			return fmt.Errorf("reverse rent receipt: %w", err)
		}
	}
	return nil
}
