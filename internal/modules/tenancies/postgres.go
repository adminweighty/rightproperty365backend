package tenancies

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

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/platform/money"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// PostgresStore is the production Store (migration 0035). The guards are
// the database's own: the partial unique index tenancies_one_active_idx
// (one ACTIVE tenancy per unit or whole property), the live label index
// property_units_live_label_idx and the composite foreign key that keeps a
// tenancy's unit inside its property. Property rows are locked while a
// tenancy is written or a property or unit archived, so neither can race
// the other.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const propertyColumns = `p.id, p.organisation_id, p.book_user_id, p.name, p.address, p.market_code, p.listing_id, p.notes, p.created_by,
	p.created_by_name, p.created_at, p.updated_at, p.deleted_at`

func scanProperty(row pgx.Row) (Property, error) {
	var p Property
	if err := row.Scan(&p.ID, &p.Book.OrganisationID, &p.Book.UserID, &p.Name, &p.Address, &p.MarketCode, &p.ListingID, &p.Notes,
		&p.CreatedBy, &p.CreatedByName, &p.CreatedAt, &p.UpdatedAt, &p.DeletedAt); err != nil {
		return Property{}, err
	}
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	if p.DeletedAt != nil {
		t := p.DeletedAt.UTC()
		p.DeletedAt = &t
	}
	return p, nil
}

const tenancyColumns = `t.id, t.organisation_id, t.book_user_id, t.book_name, t.property_id, t.unit_id, t.lease_id, t.rent_amount::text,
	t.rent_currency, t.rent_frequency, t.rent_due_day, COALESCE(t.deposit_amount::text, ''), COALESCE(t.deposit_currency, ''),
	t.start_date::text, COALESCE(t.end_date::text, ''), COALESCE(t.ended_on::text, ''), t.status, t.notes, COALESCE(t.end_reason, ''),
	t.activated_at, t.activated_by, t.activated_by_name, t.ended_at, t.ended_by, t.ended_by_name, t.created_by, t.created_by_name,
	t.created_at, t.updated_at`

func scanTenancy(row pgx.Row) (Tenancy, error) {
	var t Tenancy
	var dueDay int16
	if err := row.Scan(&t.ID, &t.Book.OrganisationID, &t.Book.UserID, &t.BookName, &t.PropertyID, &t.UnitID, &t.LeaseID, &t.RentAmount,
		&t.RentCurrency, &t.RentFrequency, &dueDay, &t.DepositAmount, &t.DepositCurrency, &t.StartDate, &t.EndDate, &t.EndedOn, &t.Status,
		&t.Notes, &t.EndReason, &t.ActivatedAt, &t.ActivatedBy, &t.ActivatedByName, &t.EndedAt, &t.EndedBy, &t.EndedByName, &t.CreatedBy,
		&t.CreatedByName, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return Tenancy{}, err
	}
	t.RentDueDay = int(dueDay)
	t.RentCurrency, t.DepositCurrency = strings.TrimSpace(t.RentCurrency), strings.TrimSpace(t.DepositCurrency)
	t.RentAmount = money.Normalise(t.RentAmount, t.RentCurrency)
	if t.DepositAmount != "" {
		t.DepositAmount = money.Normalise(t.DepositAmount, t.DepositCurrency)
	}
	t.CreatedAt, t.UpdatedAt = t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	if t.ActivatedAt != nil {
		v := t.ActivatedAt.UTC()
		t.ActivatedAt = &v
	}
	if t.EndedAt != nil {
		v := t.EndedAt.UTC()
		t.EndedAt = &v
	}
	return t, nil
}

func bookWhere(alias string, book Book, args *[]any) string {
	*args = append(*args, book.ID())
	n := strconv.Itoa(len(*args))
	if book.OrganisationID != nil {
		return alias + ".organisation_id = $" + n
	}
	return alias + ".book_user_id = $" + n
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func limitClause(limit, offset int, args *[]any) string {
	out := ""
	if limit > 0 {
		*args = append(*args, limit)
		out += " LIMIT $" + strconv.Itoa(len(*args))
	}
	if offset > 0 {
		*args = append(*args, offset)
		out += " OFFSET $" + strconv.Itoa(len(*args))
	}
	return out
}

// withUnits loads the units of the properties (archived ones too).
func withUnits(ctx context.Context, q querier, list []Property) ([]Property, error) {
	if len(list) == 0 {
		return list, nil
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	rows, err := q.Query(ctx, `SELECT id, property_id, label, position, created_at, updated_at, deleted_at FROM property_units
		WHERE property_id = ANY($1) ORDER BY position, id`, ids)
	if err != nil {
		return nil, fmt.Errorf("read units: %w", err)
	}
	units, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Unit, error) {
		var u Unit
		err := row.Scan(&u.ID, &u.PropertyID, &u.Label, &u.Position, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt)
		u.CreatedAt, u.UpdatedAt = u.CreatedAt.UTC(), u.UpdatedAt.UTC()
		if u.DeletedAt != nil {
			t := u.DeletedAt.UTC()
			u.DeletedAt = &t
		}
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("read units: %w", err)
	}
	for i := range list {
		list[i].Units = []Unit{}
		for _, u := range units {
			if u.PropertyID == list[i].ID {
				list[i].Units = append(list[i].Units, u)
			}
		}
	}
	return list, nil
}

func (s *PostgresStore) properties(ctx context.Context, sql string, args ...any) ([]Property, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read properties: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Property, error) { return scanProperty(row) })
	if err != nil {
		return nil, fmt.Errorf("read properties: %w", err)
	}
	return withUnits(ctx, s.pool, list)
}

// Property implements Store.
func (s *PostgresStore) Property(ctx context.Context, id uuid.UUID) (Property, error) {
	list, err := s.properties(ctx, `SELECT `+propertyColumns+` FROM properties p WHERE p.id = $1`, id)
	if err != nil {
		return Property{}, err
	}
	if len(list) == 0 {
		return Property{}, ErrPropertyNotFound
	}
	return list[0], nil
}

// PropertiesByIDs implements Store.
func (s *PostgresStore) PropertiesByIDs(ctx context.Context, ids []uuid.UUID) ([]Property, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.properties(ctx, `SELECT `+propertyColumns+` FROM properties p WHERE p.id = ANY($1)`, ids)
}

// Properties implements Store.
func (s *PostgresStore) Properties(ctx context.Context, book Book, f PropertyFilter) ([]Property, int, error) {
	var args []any
	conds := []string{bookWhere("p", book, &args), "p.deleted_at IS NULL"}
	if f.MarketCode != "" {
		args = append(args, f.MarketCode)
		conds = append(conds, "p.market_code = $"+strconv.Itoa(len(args)))
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		args = append(args, "%"+likeEscape(strings.ToLower(q))+"%")
		n := strconv.Itoa(len(args))
		conds = append(conds, "(lower(p.name) LIKE $"+n+" OR lower(p.address) LIKE $"+n+")")
	}
	where := strings.Join(conds, " AND ")
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM properties p WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count properties: %w", err)
	}
	list, err := s.properties(ctx, `SELECT `+propertyColumns+` FROM properties p WHERE `+where+
		` ORDER BY p.created_at DESC, p.id DESC`+limitClause(f.Limit, f.Offset, &args), args...)
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// Uses implements Store.
func (s *PostgresStore) Uses(ctx context.Context, ids []uuid.UUID) ([]Use, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, property_id, unit_id, status FROM tenancies
		WHERE property_id = ANY($1) AND status IN ('DRAFT', 'ACTIVE') ORDER BY id`, ids)
	if err != nil {
		return nil, fmt.Errorf("read property uses: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Use, error) {
		var u Use
		err := row.Scan(&u.TenancyID, &u.PropertyID, &u.UnitID, &u.Status)
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("read property uses: %w", err)
	}
	return out, nil
}

// withParties loads the parties of the tenancies.
func withParties(ctx context.Context, q querier, list []Tenancy) ([]Tenancy, error) {
	if len(list) == 0 {
		return list, nil
	}
	ids := make([]uuid.UUID, 0, len(list))
	for _, t := range list {
		ids = append(ids, t.ID)
	}
	rows, err := q.Query(ctx, `SELECT tenancy_id, contact_id, role, name FROM tenancy_parties WHERE tenancy_id = ANY($1)
		ORDER BY tenancy_id, position`, ids)
	if err != nil {
		return nil, fmt.Errorf("read tenancy parties: %w", err)
	}
	type row struct {
		tenancy uuid.UUID
		party   Party
	}
	parties, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.tenancy, &x.party.ContactID, &x.party.Role, &x.party.Name)
		return x, err
	})
	if err != nil {
		return nil, fmt.Errorf("read tenancy parties: %w", err)
	}
	for i := range list {
		list[i].Parties = []Party{}
		for _, x := range parties {
			if x.tenancy == list[i].ID {
				list[i].Parties = append(list[i].Parties, x.party)
			}
		}
	}
	return list, nil
}

func (s *PostgresStore) tenancies(ctx context.Context, sql string, args ...any) ([]Tenancy, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read tenancies: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Tenancy, error) { return scanTenancy(row) })
	if err != nil {
		return nil, fmt.Errorf("read tenancies: %w", err)
	}
	return withParties(ctx, s.pool, list)
}

// Tenancy implements Store.
func (s *PostgresStore) Tenancy(ctx context.Context, id uuid.UUID) (Tenancy, error) {
	list, err := s.tenancies(ctx, `SELECT `+tenancyColumns+` FROM tenancies t WHERE t.id = $1`, id)
	if err != nil {
		return Tenancy{}, err
	}
	if len(list) == 0 {
		return Tenancy{}, ErrTenancyNotFound
	}
	return list[0], nil
}

// Tenancies implements Store.
func (s *PostgresStore) Tenancies(ctx context.Context, book Book, f TenancyFilter) ([]Tenancy, int, map[string]int, error) {
	var args []any
	conds := []string{bookWhere("t", book, &args)}
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.PropertyID != nil {
		add("t.property_id = ?", *f.PropertyID)
	}
	if f.UnitID != nil {
		add("t.unit_id = ?", *f.UnitID)
	}
	if f.ContactID != nil {
		add("t.id IN (SELECT tenancy_id FROM tenancy_parties WHERE contact_id = ?)", *f.ContactID)
	}
	rows, err := s.pool.Query(ctx, `SELECT t.status, count(*) FROM tenancies t WHERE `+strings.Join(conds, " AND ")+` GROUP BY t.status`, args...)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("count tenancies: %w", err)
	}
	counts := map[string]int{}
	type count struct {
		status string
		n      int
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (count, error) {
		var c count
		err := r.Scan(&c.status, &c.n)
		return c, err
	})
	if err != nil {
		return nil, 0, nil, fmt.Errorf("count tenancies: %w", err)
	}
	total := 0
	for _, c := range got {
		counts[c.status] = c.n
		if f.Status == "" || f.Status == c.status {
			total += c.n
		}
	}
	if f.Status != "" {
		add("t.status = ?", f.Status)
	}
	list, err := s.tenancies(ctx, `SELECT `+tenancyColumns+` FROM tenancies t WHERE `+strings.Join(conds, " AND ")+
		` ORDER BY t.created_at DESC, t.id DESC`+limitClause(f.Limit, f.Offset, &args), args...)
	if err != nil {
		return nil, 0, nil, err
	}
	return list, total, counts, nil
}

// TenanciesOfTenants implements Store.
func (s *PostgresStore) TenanciesOfTenants(ctx context.Context, contactIDs []uuid.UUID) ([]Tenancy, error) {
	if len(contactIDs) == 0 {
		return nil, nil
	}
	return s.tenancies(ctx, `SELECT `+tenancyColumns+` FROM tenancies t
		WHERE t.status <> 'DRAFT' AND t.id IN (SELECT tenancy_id FROM tenancy_parties WHERE role = 'TENANT' AND contact_id = ANY($1))
		ORDER BY t.created_at DESC, t.id DESC`, contactIDs)
}

// Apply implements Store.
func (s *PostgresStore) Apply(ctx context.Context, ch Change) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := ApplyTx(ctx, tx, ch); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return translate(fmt.Errorf("commit: %w", err))
	}
	return nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// translate turns the database guards into the store's errors.
func translate(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	switch {
	case pg.Code == "23505" && pg.ConstraintName == "tenancies_one_active_idx":
		return ErrUnitOccupied
	case pg.Code == "23505" && pg.ConstraintName == "property_units_live_label_idx":
		return ErrUnitLabelTaken
	case pg.Code == "23503" && pg.ConstraintName == "tenancies_unit_of_property_fkey":
		return ErrUnitNotFound
	}
	return err
}

// lockProperty locks a property row and reports whether it is live.
func lockProperty(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error) {
	var live bool
	err := tx.QueryRow(ctx, `SELECT deleted_at IS NULL FROM properties WHERE id = $1 FOR UPDATE`, id).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrPropertyNotFound
	}
	if err != nil {
		return false, fmt.Errorf("lock property: %w", err)
	}
	return live, nil
}

// ApplyTx writes a Change inside the caller's transaction (see Change for the order).
func ApplyTx(ctx context.Context, tx pgx.Tx, ch Change) error {
	for id, status := range ch.ExpectStatus {
		var got string
		err := tx.QueryRow(ctx, `SELECT status FROM tenancies WHERE id = $1 FOR UPDATE`, id).Scan(&got)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && got != status) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("lock tenancy: %w", err)
		}
	}
	if ch.Ledger != nil {
		if err := ledger.ApplyTx(ctx, tx, *ch.Ledger); err != nil {
			return err
		}
	}
	for _, id := range ch.PropertyFree {
		if _, err := lockProperty(ctx, tx, id); err != nil {
			return err
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenancies WHERE property_id = $1 AND status IN ('DRAFT', 'ACTIVE'))`,
			id).Scan(&used); err != nil {
			return fmt.Errorf("check property use: %w", err)
		}
		if used {
			return ErrPropertyInUse
		}
	}
	for _, id := range ch.UnitFree {
		var property uuid.UUID
		err := tx.QueryRow(ctx, `SELECT property_id FROM property_units WHERE id = $1`, id).Scan(&property)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnitNotFound
		}
		if err != nil {
			return fmt.Errorf("read unit: %w", err)
		}
		if _, err := lockProperty(ctx, tx, property); err != nil {
			return err
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenancies WHERE unit_id = $1 AND status IN ('DRAFT', 'ACTIVE'))`,
			id).Scan(&used); err != nil {
			return fmt.Errorf("check unit use: %w", err)
		}
		if used {
			return ErrUnitInUse
		}
	}
	for _, p := range ch.Properties {
		if _, err := tx.Exec(ctx, `
			INSERT INTO properties (id, organisation_id, book_user_id, name, address, market_code, listing_id, notes, created_by,
				created_by_name, created_at, updated_at, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, address = EXCLUDED.address, market_code = EXCLUDED.market_code,
				listing_id = EXCLUDED.listing_id, notes = EXCLUDED.notes, updated_at = EXCLUDED.updated_at, deleted_at = EXCLUDED.deleted_at`,
			p.ID, p.Book.OrganisationID, p.Book.UserID, p.Name, p.Address, p.MarketCode, p.ListingID, p.Notes, p.CreatedBy, p.CreatedByName,
			p.CreatedAt, p.UpdatedAt, p.DeletedAt); err != nil {
			return translate(fmt.Errorf("write property: %w", err))
		}
	}
	for _, u := range ch.Units {
		if _, err := tx.Exec(ctx, `
			INSERT INTO property_units (id, property_id, label, position, created_at, updated_at, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (id) DO UPDATE SET label = EXCLUDED.label, position = EXCLUDED.position, updated_at = EXCLUDED.updated_at,
				deleted_at = EXCLUDED.deleted_at`,
			u.ID, u.PropertyID, u.Label, u.Position, u.CreatedAt, u.UpdatedAt, u.DeletedAt); err != nil {
			return translate(fmt.Errorf("write unit: %w", err))
		}
	}
	if len(ch.DeleteTenancies) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM tenancies WHERE id = ANY($1)`, ch.DeleteTenancies); err != nil {
			return fmt.Errorf("delete tenancies: %w", err)
		}
	}
	for _, t := range ch.Tenancies {
		live, err := lockProperty(ctx, tx, t.PropertyID)
		if err != nil {
			return err
		}
		if !live && Open(t.Status) {
			return ErrPropertyNotFound
		}
		if err := writeTenancy(ctx, tx, t); err != nil {
			return err
		}
	}
	if err := applyRentTx(ctx, tx, ch); err != nil {
		return err
	}
	for _, e := range ch.Audit {
		if err := orgaudit.Insert(ctx, tx, e); err != nil {
			return err
		}
	}
	return nil
}

func writeTenancy(ctx context.Context, tx pgx.Tx, t Tenancy) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO tenancies (id, organisation_id, book_user_id, book_name, property_id, unit_id, lease_id, rent_amount, rent_currency,
			rent_frequency, rent_due_day, deposit_amount, deposit_currency, start_date, end_date, ended_on, status, notes, end_reason,
			activated_at, activated_by, activated_by_name, ended_at, ended_by, ended_by_name, created_by, created_by_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11, $12::numeric, $13, $14::date, $15::date, $16::date, $17, $18, $19,
			$20, $21, $22, $23, $24, $25, $26, $27, $28, $29)
		ON CONFLICT (id) DO UPDATE SET property_id = EXCLUDED.property_id, unit_id = EXCLUDED.unit_id, lease_id = EXCLUDED.lease_id,
			rent_amount = EXCLUDED.rent_amount, rent_currency = EXCLUDED.rent_currency, rent_frequency = EXCLUDED.rent_frequency,
			rent_due_day = EXCLUDED.rent_due_day, deposit_amount = EXCLUDED.deposit_amount, deposit_currency = EXCLUDED.deposit_currency,
			start_date = EXCLUDED.start_date, end_date = EXCLUDED.end_date, ended_on = EXCLUDED.ended_on, status = EXCLUDED.status,
			notes = EXCLUDED.notes, end_reason = EXCLUDED.end_reason, activated_at = EXCLUDED.activated_at,
			activated_by = EXCLUDED.activated_by, activated_by_name = EXCLUDED.activated_by_name, ended_at = EXCLUDED.ended_at,
			ended_by = EXCLUDED.ended_by, ended_by_name = EXCLUDED.ended_by_name, updated_at = EXCLUDED.updated_at`,
		t.ID, t.Book.OrganisationID, t.Book.UserID, t.BookName, t.PropertyID, t.UnitID, t.LeaseID, t.RentAmount, t.RentCurrency,
		t.RentFrequency, t.RentDueDay, nullable(t.DepositAmount), nullable(t.DepositCurrency), t.StartDate, nullable(t.EndDate),
		nullable(t.EndedOn), t.Status, t.Notes, nullable(t.EndReason), t.ActivatedAt, t.ActivatedBy, t.ActivatedByName, t.EndedAt,
		t.EndedBy, t.EndedByName, t.CreatedBy, t.CreatedByName, t.CreatedAt, t.UpdatedAt); err != nil {
		return translate(fmt.Errorf("write tenancy: %w", err))
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tenancy_parties WHERE tenancy_id = $1`, t.ID); err != nil {
		return fmt.Errorf("replace tenancy parties: %w", err)
	}
	for i, p := range t.Parties {
		if _, err := tx.Exec(ctx, `INSERT INTO tenancy_parties (tenancy_id, contact_id, role, position, name) VALUES ($1, $2, $3, $4, $5)`,
			t.ID, p.ContactID, p.Role, i, p.Name); err != nil {
			return fmt.Errorf("write tenancy party: %w", err)
		}
	}
	return nil
}

// ForgetUserTx handles a deleted account's rows inside the account
// deletion's transaction (the users row is anonymised, so nothing
// cascades): the direct owner's DRAFT tenancies are deleted, the manager
// name shown to the tenants of their personal book is replaced with
// deletedName, and the person is unlinked as creator, activator and ender
// (and as who recorded a rent receipt or waived a due; the names stay, as
// the ledger's author names do).
func ForgetUserTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, deletedName string) error {
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM tenancies WHERE book_user_id = $1 AND status = 'DRAFT'`, []any{userID}},
		{`UPDATE tenancies SET book_name = $2 WHERE book_user_id = $1`, []any{userID, deletedName}},
		{`UPDATE tenancies SET created_by = NULL WHERE created_by = $1`, []any{userID}},
		{`UPDATE tenancies SET activated_by = NULL WHERE activated_by = $1`, []any{userID}},
		{`UPDATE tenancies SET ended_by = NULL WHERE ended_by = $1`, []any{userID}},
		{`UPDATE properties SET created_by = NULL WHERE created_by = $1`, []any{userID}},
		{`UPDATE rent_dues SET waived_by = NULL WHERE waived_by = $1`, []any{userID}},
		{`UPDATE rent_receipts SET recorded_by = NULL WHERE recorded_by = $1`, []any{userID}},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
			return fmt.Errorf("forget the deleted account's tenancies: %w", err)
		}
	}
	return nil
}
