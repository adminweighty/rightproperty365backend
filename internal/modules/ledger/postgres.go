package ledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/money"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// PostgresStore is the production Store (migration 0033). The guards are
// the database's own: the trigger ledger_entries_guard refuses changes to
// posted entries and every delete; the partial unique index
// ledger_entries_opening_idx allows one live opening balance per currency,
// method and branch.
type PostgresStore struct {
	pool       *pgxpool.Pool
	onReversed TxReversalHook
}

// TxReversalHook runs inside the transaction of a Change that reverses
// entries, after the Change is written: an error rolls everything back.
type TxReversalHook func(ctx context.Context, tx pgx.Tx, list []Reversal) error

// SetReversalHook sets the hook run by Apply when entries are reversed (the
// tenancies module undoes a rent receipt's allocations there).
func (s *PostgresStore) SetReversalHook(hook TxReversalHook) { s.onReversed = hook }

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
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const entryColumns = `e.id, e.organisation_id, e.book_user_id, e.type, e.direction, COALESCE(e.category, ''), COALESCE(e.method, ''),
	e.amount::text, e.currency, e.effective_date::text, e.recorded_at, e.author_user_id, e.author_name, e.status, e.reconciled_at,
	e.reconciled_by, e.reconciled_by_name, COALESCE(e.reconcile_note, ''), e.reversal_of, e.reversed_by, e.correction_of, e.corrected_by,
	e.transfer_pair, e.settles_entry_id, COALESCE(e.reason, ''), e.contact_id, e.listing_id, e.lease_id, e.tenancy_id, COALESCE(e.branch, ''),
	e.description, e.reference, e.property_id, e.branch_id,
	(SELECT count(*) FROM ledger_entry_notes n WHERE n.entry_id = e.id),
	(SELECT count(*) FROM ledger_attachments a WHERE a.entry_id = e.id)`

func scanEntry(row pgx.Row) (Entry, error) {
	var e Entry
	if err := row.Scan(&e.ID, &e.Book.OrganisationID, &e.Book.UserID, &e.Type, &e.Direction, &e.Category, &e.Method, &e.Amount, &e.Currency,
		&e.EffectiveDate, &e.RecordedAt, &e.AuthorUserID, &e.AuthorName, &e.Status, &e.ReconciledAt, &e.ReconciledBy, &e.ReconciledName,
		&e.ReconcileNote, &e.ReversalOf, &e.ReversedBy, &e.CorrectionOf, &e.CorrectedBy, &e.TransferPair, &e.Settles, &e.Reason,
		&e.ContactID, &e.ListingID, &e.LeaseID, &e.TenancyID, &e.Branch, &e.Description, &e.Reference, &e.PropertyID, &e.BranchID, &e.NoteCount,
		&e.AttachmentCount); err != nil {
		return Entry{}, err
	}
	e.Currency = strings.TrimSpace(e.Currency)
	e.Amount = money.Normalise(e.Amount, e.Currency)
	e.RecordedAt = e.RecordedAt.UTC()
	if e.ReconciledAt != nil {
		t := e.ReconciledAt.UTC()
		e.ReconciledAt = &t
	}
	return e, nil
}

func collectEntries(rows pgx.Rows, err error) ([]Entry, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) { return scanEntry(row) })
}

// bookWhere is the condition selecting a book's rows of alias.
func bookWhere(alias string, book Book, args *[]any) string {
	*args = append(*args, book.ID())
	n := strconv.Itoa(len(*args))
	if book.OrganisationID != nil {
		return alias + ".organisation_id = $" + n
	}
	return alias + ".book_user_id = $" + n
}

// Entry implements Store.
func (s *PostgresStore) Entry(ctx context.Context, id uuid.UUID) (Entry, error) {
	e, err := scanEntry(s.pool.QueryRow(ctx, `SELECT `+entryColumns+` FROM ledger_entries e WHERE e.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, ErrEntryNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("read ledger entry: %w", err)
	}
	return e, nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func entryWhere(book Book, f Filter) (string, []any) {
	var args []any
	conds := []string{bookWhere("e", book, &args)}
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.Currency != "" {
		add("e.currency = ?", f.Currency)
	}
	if f.Type != "" {
		add("e.type = ?", f.Type)
	}
	if f.Category != "" {
		add("e.category = ?", f.Category)
	}
	if f.Method != "" {
		add("e.method = ?", f.Method)
	}
	if f.Status != "" {
		add("e.status = ?", f.Status)
	}
	if f.From != "" {
		add("e.effective_date >= ?::date", f.From)
	}
	if f.To != "" {
		add("e.effective_date <= ?::date", f.To)
	}
	if f.Branch != nil {
		add("COALESCE(lower(e.branch), '') = lower(?)", *f.Branch)
	}
	if f.OnlyBranch != nil {
		add("(e.branch_id IS NULL OR e.branch_id = ?)", *f.OnlyBranch)
	}
	if f.ContactID != nil {
		add("e.contact_id = ?", *f.ContactID)
	}
	if f.ListingID != nil {
		add("e.listing_id = ?", *f.ListingID)
	}
	if f.LeaseID != nil {
		add("e.lease_id = ?", *f.LeaseID)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		add("(e.description ILIKE ? OR e.reference ILIKE ?)", "%"+likeEscape(q)+"%")
	}
	if f.ExcludeReversed {
		conds = append(conds, "e.reversed_by IS NULL AND e.type <> 'REVERSAL'")
	}
	return strings.Join(conds, " AND "), args
}

// Entries implements Store.
func (s *PostgresStore) Entries(ctx context.Context, book Book, f Filter) ([]Entry, int, error) {
	where, args := entryWhere(book, f)
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries e WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count ledger entries: %w", err)
	}
	order := "e.effective_date DESC, e.recorded_at DESC, e.id DESC"
	if f.Ascending {
		order = "e.effective_date, e.recorded_at, e.id"
	}
	sql := `SELECT ` + entryColumns + ` FROM ledger_entries e WHERE ` + where + ` ORDER BY ` + order
	if f.Limit > 0 {
		args = append(args, f.Limit, max(f.Offset, 0))
		sql += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	}
	list, err := collectEntries(s.pool.Query(ctx, sql, args...))
	if err != nil {
		return nil, 0, fmt.Errorf("list ledger entries: %w", err)
	}
	return list, total, nil
}

// Related implements Store.
func (s *PostgresStore) Related(ctx context.Context, book Book, id uuid.UUID) ([]Entry, error) {
	args := []any{}
	where := bookWhere("e", book, &args)
	args = append(args, id)
	list, err := collectEntries(s.pool.Query(ctx, `SELECT `+entryColumns+` FROM ledger_entries e WHERE `+where+`
		AND e.id <> $2 AND (e.reversal_of = $2 OR e.correction_of = $2 OR e.transfer_pair = $2 OR e.settles_entry_id = $2)
		ORDER BY e.effective_date, e.recorded_at, e.id`, args...))
	if err != nil {
		return nil, fmt.Errorf("related ledger entries: %w", err)
	}
	return list, nil
}

// Settled implements Store.
func (s *PostgresStore) Settled(ctx context.Context, book Book, ids []uuid.UUID) (map[uuid.UUID][]Entry, error) {
	out := map[uuid.UUID][]Entry{}
	if len(ids) == 0 {
		return out, nil
	}
	args := []any{}
	where := bookWhere("e", book, &args)
	args = append(args, ids)
	list, err := collectEntries(s.pool.Query(ctx, `SELECT `+entryColumns+` FROM ledger_entries e WHERE `+where+`
		AND e.settles_entry_id = ANY($2) AND e.reversed_by IS NULL ORDER BY e.effective_date, e.recorded_at, e.id`, args...))
	if err != nil {
		return nil, fmt.Errorf("settlements: %w", err)
	}
	for _, e := range list {
		out[*e.Settles] = append(out[*e.Settles], e)
	}
	return out, nil
}

// OpeningBalance implements Store.
func (s *PostgresStore) OpeningBalance(ctx context.Context, book Book, currency, method, branch string) (Entry, bool, error) {
	args := []any{}
	where := bookWhere("e", book, &args)
	args = append(args, currency, method, branch)
	e, err := scanEntry(s.pool.QueryRow(ctx, `SELECT `+entryColumns+` FROM ledger_entries e WHERE `+where+`
		AND e.type = 'OPENING_BALANCE' AND e.reversed_by IS NULL AND e.currency = $2 AND e.method = $3
		AND COALESCE(lower(e.branch), '') = lower($4) LIMIT 1`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("read opening balance: %w", err)
	}
	return e, true, nil
}

// Branches implements Store.
func (s *PostgresStore) Branches(ctx context.Context, book Book) ([]string, error) {
	args := []any{}
	where := bookWhere("e", book, &args)
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT e.branch FROM ledger_entries e WHERE `+where+` AND e.branch IS NOT NULL ORDER BY 1`, args...)
	if err != nil {
		return nil, fmt.Errorf("ledger branches: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Notes implements Store.
func (s *PostgresStore) Notes(ctx context.Context, entryID uuid.UUID) ([]Note, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, entry_id, text, author_user_id, author_name, created_at FROM ledger_entry_notes
		WHERE entry_id = $1 ORDER BY created_at, id`, entryID)
	if err != nil {
		return nil, fmt.Errorf("ledger notes: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Note, error) {
		var n Note
		err := row.Scan(&n.ID, &n.EntryID, &n.Text, &n.AuthorUserID, &n.AuthorName, &n.CreatedAt)
		n.CreatedAt = n.CreatedAt.UTC()
		return n, err
	})
}

// Attachments implements Store.
func (s *PostgresStore) Attachments(ctx context.Context, entryID uuid.UUID) ([]Attachment, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, entry_id, file_name, content_type, size_bytes, sha256, object_key, uploaded_by, uploaded_by_name,
		uploaded_at FROM ledger_attachments WHERE entry_id = $1 ORDER BY uploaded_at, id`, entryID)
	if err != nil {
		return nil, fmt.Errorf("ledger attachments: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Attachment, error) {
		var a Attachment
		err := row.Scan(&a.ID, &a.EntryID, &a.FileName, &a.ContentType, &a.Size, &a.SHA256, &a.ObjectKey, &a.UploadedBy, &a.UploadedByName, &a.UploadedAt)
		a.UploadedAt = a.UploadedAt.UTC()
		return a, err
	})
}

// Categories implements Store.
func (s *PostgresStore) Categories(ctx context.Context, book Book) ([]Category, error) {
	args := []any{}
	where := bookWhere("c", book, &args)
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.organisation_id, c.book_user_id, c.code, c.label, c.kind, c.active, c.is_default, c.sort_order,
		c.created_by, c.created_at, c.updated_at,
		EXISTS (SELECT 1 FROM ledger_entries e WHERE e.book_id = c.book_id AND e.category = c.code)
		FROM ledger_categories c WHERE `+where+` ORDER BY c.sort_order, lower(c.label), c.code`, args...)
	if err != nil {
		return nil, fmt.Errorf("ledger categories: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Category, error) {
		var c Category
		err := row.Scan(&c.ID, &c.Book.OrganisationID, &c.Book.UserID, &c.Code, &c.Label, &c.Kind, &c.Active, &c.Default, &c.SortOrder,
			&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.Used)
		return c, err
	})
}

const insertCategory = `INSERT INTO ledger_categories (id, organisation_id, book_user_id, code, label, kind, active, is_default, sort_order,
	created_by, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

func categoryArgs(book Book, c Category) []any {
	c = stamped(c)
	id := c.ID
	return []any{id, book.OrganisationID, book.UserID, c.Code, c.Label, c.Kind, c.Active, c.Default, c.SortOrder, c.CreatedBy, c.CreatedAt, c.UpdatedAt}
}

// EnsureCategories implements Store.
func (s *PostgresStore) EnsureCategories(ctx context.Context, book Book, defaults []Category) error {
	return s.inTx(ctx, func(tx pgx.Tx) error { return EnsureCategoriesTx(ctx, tx, book, defaults) })
}

// EnsureCategoriesTx adds the categories a book lacks inside the caller's
// transaction (the store, and the development seed).
func EnsureCategoriesTx(ctx context.Context, q querier, book Book, defaults []Category) error {
	for _, c := range defaults {
		if _, err := q.Exec(ctx, insertCategory+` ON CONFLICT (book_id, code) DO NOTHING`, categoryArgs(book, c)...); err != nil {
			return fmt.Errorf("add ledger category: %w", err)
		}
	}
	return nil
}

// CreateCategory implements Store.
func (s *PostgresStore) CreateCategory(ctx context.Context, c Category) error {
	_, err := s.pool.Exec(ctx, insertCategory, categoryArgs(c.Book, c)...)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return ErrCategoryExists
	}
	if err != nil {
		return fmt.Errorf("create ledger category: %w", err)
	}
	return nil
}

// UpdateCategory implements Store.
func (s *PostgresStore) UpdateCategory(ctx context.Context, c Category) error {
	args := []any{}
	where := bookWhere("ledger_categories", c.Book, &args)
	args = append(args, c.Code, c.Label, c.Kind, c.Active, c.UpdatedAt)
	tag, err := s.pool.Exec(ctx, `UPDATE ledger_categories SET label = $3, kind = $4, active = $5, updated_at = $6
		WHERE `+where+` AND code = $2`, args...)
	if err != nil {
		return fmt.Errorf("update ledger category: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCategoryNotFound
	}
	return nil
}

// Apply implements Store.
func (s *PostgresStore) Apply(ctx context.Context, ch Change) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := ApplyTx(ctx, tx, ch); err != nil {
			return err
		}
		if rev := ch.reversals(); len(rev) > 0 && s.onReversed != nil {
			return s.onReversed(ctx, tx, rev)
		}
		return nil
	})
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func insertEntry(ctx context.Context, tx pgx.Tx, e Entry) error {
	if e.Settles != nil {
		var amount, currency string
		if err := tx.QueryRow(ctx, `SELECT amount::text, currency FROM ledger_entries WHERE id = $1 FOR UPDATE`, *e.Settles).Scan(&amount, &currency); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrEntryNotFound
			}
			return fmt.Errorf("lock settled entry: %w", err)
		}
		var paid string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::text FROM ledger_entries WHERE settles_entry_id = $1 AND reversed_by IS NULL`,
			*e.Settles).Scan(&paid); err != nil {
			return fmt.Errorf("sum settlements: %w", err)
		}
		left := money.Rat(amount)
		left.Sub(left, money.Rat(paid))
		if money.Rat(e.Amount).Cmp(left) > 0 {
			return ErrOverSettled
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (id, organisation_id, book_user_id, type, direction, category, method, amount, currency,
		effective_date, recorded_at, author_user_id, author_name, status, reconciled_at, reconciled_by, reconciled_by_name, reconcile_note,
		reversal_of, correction_of, transfer_pair, settles_entry_id, reason, contact_id, listing_id, lease_id, tenancy_id, branch, description, reference,
		property_id, branch_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10::date, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24,
		$25, $26, $27, $28, $29, $30, $31,
		(SELECT b.id FROM organisation_branches b WHERE b.organisation_id = $2 AND lower(b.name) = lower(trim($28)) AND b.active))`,
		e.ID, e.Book.OrganisationID, e.Book.UserID, e.Type, e.Direction, nullable(e.Category), nullable(e.Method), e.Amount, e.Currency,
		e.EffectiveDate, e.RecordedAt, e.AuthorUserID, e.AuthorName, e.Status, e.ReconciledAt, e.ReconciledBy, e.ReconciledName,
		nullable(e.ReconcileNote), e.ReversalOf, e.CorrectionOf, e.TransferPair, e.Settles, nullable(e.Reason), e.ContactID, e.ListingID,
		e.LeaseID, e.TenancyID, nullable(e.Branch), e.Description, e.Reference, e.PropertyID)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "ledger_entries_opening_idx" {
		return ErrOpeningBalanceExists
	}
	if err != nil {
		return fmt.Errorf("insert ledger entry: %w", err)
	}
	return nil
}

func applyLinks(ctx context.Context, tx pgx.Tx, list []LinkSet) error {
	for _, l := range list {
		tag, err := tx.Exec(ctx, `UPDATE ledger_entries SET reversed_by = COALESCE($2, reversed_by), corrected_by = COALESCE($3, corrected_by),
			transfer_pair = COALESCE($4, transfer_pair) WHERE id = $1`, l.EntryID, l.ReversedBy, l.CorrectedBy, l.TransferPair)
		if err != nil {
			return fmt.Errorf("link ledger entry: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrEntryNotFound
		}
	}
	return nil
}

// ApplyTx writes a Change inside the caller's transaction (the store, the
// tenancies module's rent receipts and the development seed). It does not
// run the reversal hook: only the store's Apply does.
func ApplyTx(ctx context.Context, tx pgx.Tx, ch Change) error {
	if len(ch.NotReversed) > 0 {
		rows, err := tx.Query(ctx, `SELECT id, reversed_by FROM ledger_entries WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ch.NotReversed)
		if err != nil {
			return fmt.Errorf("lock ledger entries: %w", err)
		}
		seen, reversed := 0, false
		for rows.Next() {
			var id uuid.UUID
			var by *uuid.UUID
			if err := rows.Scan(&id, &by); err != nil {
				rows.Close()
				return err
			}
			seen++
			reversed = reversed || by != nil
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if seen != len(ch.NotReversed) {
			return ErrEntryNotFound
		}
		if reversed {
			return ErrAlreadyReversed
		}
	}
	for _, e := range ch.Reversals {
		if err := insertEntry(ctx, tx, e); err != nil {
			return err
		}
	}
	if err := applyLinks(ctx, tx, ch.Links); err != nil {
		return err
	}
	for _, e := range ch.Entries {
		if err := insertEntry(ctx, tx, e); err != nil {
			return err
		}
	}
	if err := applyLinks(ctx, tx, ch.PostLinks); err != nil {
		return err
	}
	if r := ch.Reconcile; r != nil {
		var err error
		if r.Status == StatusReconciled {
			_, err = tx.Exec(ctx, `UPDATE ledger_entries SET status = 'RECONCILED', reconciled_at = $2, reconciled_by = $3, reconciled_by_name = $4,
				reconcile_note = $5 WHERE id = $1`, r.EntryID, r.At, r.By, r.ByName, nullable(r.Note))
		} else {
			_, err = tx.Exec(ctx, `UPDATE ledger_entries SET status = 'USER_RECORDED_UNVERIFIED', reconciled_at = NULL, reconciled_by = NULL,
				reconciled_by_name = '', reconcile_note = NULL WHERE id = $1`, r.EntryID)
		}
		if err != nil {
			return fmt.Errorf("reconcile ledger entry: %w", err)
		}
	}
	for _, n := range ch.Notes {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_entry_notes (id, entry_id, text, author_user_id, author_name, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, n.ID, n.EntryID, n.Text, n.AuthorUserID, n.AuthorName, n.CreatedAt); err != nil {
			return fmt.Errorf("insert ledger note: %w", err)
		}
	}
	for _, a := range ch.Attachments {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_attachments (id, entry_id, file_name, content_type, size_bytes, sha256, object_key,
			uploaded_by, uploaded_by_name, uploaded_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			a.ID, a.EntryID, a.FileName, a.ContentType, a.Size, a.SHA256, a.ObjectKey, a.UploadedBy, a.UploadedByName, a.UploadedAt); err != nil {
			return fmt.Errorf("insert ledger attachment: %w", err)
		}
	}
	for _, e := range ch.Audit {
		if err := orgaudit.Insert(ctx, tx, e); err != nil {
			return err
		}
	}
	return nil
}
