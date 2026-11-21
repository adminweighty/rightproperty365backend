package packages

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/money"
)

// PostgresStore keeps packages in listing_packages, listing_package_prices
// and listing_package_assignments (migration 0040).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the Postgres adapter.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// querier is a pool or a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *PostgresStore) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// lockAccount takes the transaction-scoped advisory lock of an account and
// market: the listing approval and every assignment change take it, so a
// count and the change it decides are never interleaved with another.
func lockAccount(ctx context.Context, q querier, acct Account, market string) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('listing_quota:' || $1, 0))`, acct.key(market)); err != nil {
		return fmt.Errorf("lock listing account: %w", err)
	}
	return nil
}

const packageColumns = `p.id, p.market_code, p.code, p.name, p.description, p.listing_limit, p.active, p.is_default, p.sort_order,
	p.created_by, p.created_at, p.updated_at`

func scanPackage(row pgx.CollectableRow) (Package, error) {
	var p Package
	err := row.Scan(&p.ID, &p.MarketCode, &p.Code, &p.Name, &p.Description, &p.ListingLimit, &p.Active, &p.Default, &p.Sort,
		&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	return p, err
}

// withPrices loads the prices of the packages.
func withPrices(ctx context.Context, q querier, list []Package) ([]Package, error) {
	if len(list) == 0 {
		return list, nil
	}
	ids := make([]uuid.UUID, len(list))
	for i, p := range list {
		ids[i] = p.ID
	}
	rows, err := q.Query(ctx, `SELECT package_id, billing_term, amount::text, currency_code, sample
		FROM listing_package_prices WHERE package_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("read package prices: %w", err)
	}
	type priced struct {
		id uuid.UUID
		Price
	}
	prices, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (priced, error) {
		var x priced
		err := row.Scan(&x.id, &x.Term, &x.Amount, &x.Currency, &x.Sample)
		x.Amount = money.Normalise(x.Amount, x.Currency)
		return x, err
	})
	if err != nil {
		return nil, fmt.Errorf("read package prices: %w", err)
	}
	for i := range list {
		list[i].Prices = []Price{}
		for _, x := range prices {
			if x.id == list[i].ID {
				list[i].Prices = append(list[i].Prices, x.Price)
			}
		}
		sortPrices(list[i].Prices)
	}
	return list, nil
}

func sortPrices(prices []Price) {
	sort.SliceStable(prices, func(i, j int) bool { return termMonths[prices[i].Term] < termMonths[prices[j].Term] })
}

func readPackages(ctx context.Context, q querier, where string, args ...any) ([]Package, error) {
	rows, err := q.Query(ctx, `SELECT `+packageColumns+` FROM listing_packages p WHERE `+where+
		` ORDER BY p.market_code, p.sort_order, p.name, p.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read packages: %w", err)
	}
	list, err := pgx.CollectRows(rows, scanPackage)
	if err != nil {
		return nil, fmt.Errorf("read packages: %w", err)
	}
	return withPrices(ctx, q, list)
}

func readPackage(ctx context.Context, q querier, id uuid.UUID, lock bool) (Package, error) {
	where := `p.id = $1`
	if lock {
		where += ` FOR UPDATE`
	}
	rows, err := q.Query(ctx, `SELECT `+packageColumns+` FROM listing_packages p WHERE `+where, id)
	if err != nil {
		return Package{}, fmt.Errorf("read package: %w", err)
	}
	list, err := pgx.CollectRows(rows, scanPackage)
	if err != nil {
		return Package{}, fmt.Errorf("read package: %w", err)
	}
	if len(list) == 0 {
		return Package{}, ErrPackageNotFound
	}
	list, err = withPrices(ctx, q, list)
	if err != nil {
		return Package{}, err
	}
	return list[0], nil
}

// Packages implements Store.
func (s *PostgresStore) Packages(ctx context.Context, f PackageFilter) ([]Package, error) {
	where, args := `($1 OR p.active)`, []any{f.IncludeInactive}
	if f.Markets != nil {
		args = append(args, f.Markets)
		where += fmt.Sprintf(` AND p.market_code = ANY($%d)`, len(args))
	}
	if f.Market != "" {
		args = append(args, f.Market)
		where += fmt.Sprintf(` AND p.market_code = $%d`, len(args))
	}
	return readPackages(ctx, s.pool, where, args...)
}

// Package implements Store.
func (s *PostgresStore) Package(ctx context.Context, id uuid.UUID) (Package, error) {
	return readPackage(ctx, s.pool, id, false)
}

func duplicate(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func writePrices(ctx context.Context, q querier, p Package) error {
	if _, err := q.Exec(ctx, `DELETE FROM listing_package_prices WHERE package_id = $1`, p.ID); err != nil {
		return fmt.Errorf("replace package prices: %w", err)
	}
	for _, pr := range p.Prices {
		if _, err := q.Exec(ctx, `INSERT INTO listing_package_prices (package_id, billing_term, amount, currency_code, sample)
			VALUES ($1, $2, $3::text::numeric, $4, $5)`, p.ID, pr.Term, pr.Amount, pr.Currency, pr.Sample); err != nil {
			return fmt.Errorf("write package price: %w", err)
		}
	}
	return nil
}

// clearDefault takes the default flag from every other package of the market.
func clearDefault(ctx context.Context, q querier, p Package) error {
	if !p.Default {
		return nil
	}
	if _, err := q.Exec(ctx, `UPDATE listing_packages SET is_default = false, updated_at = $3
		WHERE market_code = $1 AND is_default AND id <> $2`, p.MarketCode, p.ID, p.UpdatedAt); err != nil {
		return fmt.Errorf("move the default package: %w", err)
	}
	return nil
}

// CreatePackage implements Store.
func (s *PostgresStore) CreatePackage(ctx context.Context, p Package, audit adminaudit.Event) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := clearDefault(ctx, tx, p); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO listing_packages (id, market_code, code, name, description, listing_limit, active, is_default, sort_order,
				created_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			p.ID, p.MarketCode, p.Code, p.Name, p.Description, p.ListingLimit, p.Active, p.Default, p.Sort, p.CreatedBy, p.CreatedAt, p.UpdatedAt); err != nil {
			if duplicate(err) {
				return ErrCodeTaken
			}
			return fmt.Errorf("create package: %w", err)
		}
		if err := writePrices(ctx, tx, p); err != nil {
			return err
		}
		return adminaudit.Insert(ctx, tx, audit)
	})
}

// ChangePackage implements Store.
func (s *PostgresStore) ChangePackage(ctx context.Context, id uuid.UUID, change func(p *Package) (adminaudit.Event, error)) (Package, error) {
	var out Package
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := readPackage(ctx, tx, id, true)
		if err != nil {
			return err
		}
		audit, err := change(&p)
		if err != nil {
			return err
		}
		if err := clearDefault(ctx, tx, p); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE listing_packages SET name = $2, description = $3, listing_limit = $4, active = $5, is_default = $6, sort_order = $7,
				updated_at = $8 WHERE id = $1`,
			p.ID, p.Name, p.Description, p.ListingLimit, p.Active, p.Default, p.Sort, p.UpdatedAt); err != nil {
			return fmt.Errorf("update package: %w", err)
		}
		if err := writePrices(ctx, tx, p); err != nil {
			return err
		}
		if err := adminaudit.Insert(ctx, tx, audit); err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, err
}

// DeletePackage implements Store.
func (s *PostgresStore) DeletePackage(ctx context.Context, id uuid.UUID, check func(p Package, assignments int) (adminaudit.Event, error)) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		p, err := readPackage(ctx, tx, id, true)
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM listing_package_assignments WHERE package_id = $1`, id).Scan(&n); err != nil {
			return fmt.Errorf("count package assignments: %w", err)
		}
		audit, err := check(p, n)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM listing_packages WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete package: %w", err)
		}
		return adminaudit.Insert(ctx, tx, audit)
	})
}

// PackageUses implements Store.
func (s *PostgresStore) PackageUses(ctx context.Context, ids []uuid.UUID, today time.Time) (map[uuid.UUID]Uses, error) {
	out := map[uuid.UUID]Uses{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT package_id, count(*),
		       count(*) FILTER (WHERE cancelled_at IS NULL AND starts_on <= $2::date AND (ends_on IS NULL OR ends_on >= $2::date))
		FROM listing_package_assignments WHERE package_id = ANY($1) GROUP BY package_id`, ids, today)
	if err != nil {
		return nil, fmt.Errorf("count package uses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var u Uses
		if err := rows.Scan(&id, &u.Total, &u.Current); err != nil {
			return nil, err
		}
		out[id] = u
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Assignments
// ---------------------------------------------------------------------------

const assignmentColumns = `a.id, a.organisation_id, a.user_id, a.market_code, a.package_id, a.starts_on, a.ends_on, a.source, a.note,
	a.assigned_by, a.assigned_by_name, a.created_at, a.cancelled_at, a.cancelled_by, a.subscription_id`

func scanAssignment(row pgx.CollectableRow) (Assignment, error) {
	var a Assignment
	var org, user *uuid.UUID
	if err := row.Scan(&a.ID, &org, &user, &a.MarketCode, &a.PackageID, &a.StartsOn, &a.EndsOn, &a.Source, &a.Note,
		&a.AssignedBy, &a.AssignedByName, &a.CreatedAt, &a.CancelledAt, &a.CancelledBy, &a.SubscriptionID); err != nil {
		return Assignment{}, err
	}
	if org != nil {
		a.Account = Account{Kind: KindOrganisation, ID: *org}
	} else if user != nil {
		a.Account = Account{Kind: KindDirectOwner, ID: *user}
	}
	a.StartsOn = Day(a.StartsOn)
	if a.EndsOn != nil {
		d := Day(*a.EndsOn)
		a.EndsOn = &d
	}
	a.CreatedAt = a.CreatedAt.UTC()
	if a.CancelledAt != nil {
		c := a.CancelledAt.UTC()
		a.CancelledAt = &c
	}
	return a, nil
}

// accountColumn is the assignments column that holds the account's id.
func accountColumn(acct Account) string {
	if acct.Kind == KindOrganisation {
		return "a.organisation_id"
	}
	return "a.user_id"
}

func readAssignments(ctx context.Context, q querier, where string, args ...any) ([]Assignment, error) {
	rows, err := q.Query(ctx, `SELECT `+assignmentColumns+` FROM listing_package_assignments a WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("read package assignments: %w", err)
	}
	list, err := pgx.CollectRows(rows, scanAssignment)
	if err != nil {
		return nil, fmt.Errorf("read package assignments: %w", err)
	}
	return list, nil
}

// resolve is Resolve on a pool or inside a transaction.
func resolve(ctx context.Context, q querier, acct Account, market string, today time.Time) (Resolved, error) {
	list, err := readAssignments(ctx, q, accountColumn(acct)+` = $1 AND a.market_code = $2 AND a.cancelled_at IS NULL
		AND (a.ends_on IS NULL OR a.ends_on >= $3::date) ORDER BY a.starts_on`, acct.ID, market, today)
	if err != nil {
		return Resolved{}, err
	}
	out := Resolved{Basis: BasisNone}
	for i := range list {
		a := list[i]
		switch {
		case a.InForce(today) && out.Assignment == nil:
			out.Assignment = &a
		case a.StartsOn.After(today) && out.Scheduled == nil:
			out.Scheduled = &a
		}
	}
	if out.Assignment != nil {
		p, err := readPackage(ctx, q, out.Assignment.PackageID, false)
		if err != nil {
			return Resolved{}, err
		}
		out.Basis, out.Package = BasisAssigned, &p
	} else {
		defaults, err := readPackages(ctx, q, `p.market_code = $1 AND p.is_default`, market)
		if err != nil {
			return Resolved{}, err
		}
		if len(defaults) > 0 {
			out.Basis, out.Package = BasisDefault, &defaults[0]
		}
	}
	if out.Scheduled != nil {
		p, err := readPackage(ctx, q, out.Scheduled.PackageID, false)
		if err != nil {
			return Resolved{}, err
		}
		out.ScheduledPackage = &p
	}
	return out, nil
}

// Resolve implements Store.
func (s *PostgresStore) Resolve(ctx context.Context, acct Account, market string, today time.Time) (Resolved, error) {
	return resolve(ctx, s.pool, acct, market, Day(today))
}

// Assignments implements Store.
func (s *PostgresStore) Assignments(ctx context.Context, acct Account) ([]Assignment, error) {
	return readAssignments(ctx, s.pool, accountColumn(acct)+` = $1 ORDER BY a.created_at DESC, a.id LIMIT $2`, acct.ID, MaxAssignmentsShown)
}

// replaceFrom cancels the account's assignments in the market that start on
// or after from and ends the one in force on from the day before (cancelled
// when it starts that day). It returns them as they were.
func replaceFrom(ctx context.Context, q querier, acct Account, market string, from time.Time, by *uuid.UUID, at time.Time) ([]Assignment, error) {
	col := accountColumn(acct)
	before, err := readAssignments(ctx, q, col+` = $1 AND a.market_code = $2 AND a.cancelled_at IS NULL
		AND (a.ends_on IS NULL OR a.ends_on >= $3::date) ORDER BY a.starts_on`, acct.ID, market, from)
	if err != nil {
		return nil, err
	}
	if _, err := q.Exec(ctx, `UPDATE listing_package_assignments a SET cancelled_at = $4, cancelled_by = $5
		WHERE `+col+` = $1 AND a.market_code = $2 AND a.cancelled_at IS NULL AND a.starts_on >= $3::date`,
		acct.ID, market, from, at, by); err != nil {
		return nil, fmt.Errorf("cancel package assignments: %w", err)
	}
	if _, err := q.Exec(ctx, `UPDATE listing_package_assignments a SET ends_on = $3::date - 1
		WHERE `+col+` = $1 AND a.market_code = $2 AND a.cancelled_at IS NULL AND a.starts_on < $3::date
		  AND (a.ends_on IS NULL OR a.ends_on >= $3::date)`, acct.ID, market, from); err != nil {
		return nil, fmt.Errorf("end package assignment: %w", err)
	}
	return before, nil
}

// Assign implements Store.
func (s *PostgresStore) Assign(ctx context.Context, a Assignment, audit func(replaced []Assignment) adminaudit.Event) (AssignResult, error) {
	var out AssignResult
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockAccount(ctx, tx, a.Account, a.MarketCode); err != nil {
			return err
		}
		p, err := readPackage(ctx, tx, a.PackageID, false)
		if err != nil {
			return err
		}
		if !p.Active {
			return ErrInactive
		}
		replaced, err := replaceFrom(ctx, tx, a.Account, a.MarketCode, a.StartsOn, a.AssignedBy, a.CreatedAt)
		if err != nil {
			return err
		}
		if err := insertAssignment(ctx, tx, a); err != nil {
			return err
		}
		out.Replaced = replaced
		return adminaudit.Insert(ctx, tx, audit(replaced))
	})
	return out, err
}

func insertAssignment(ctx context.Context, q querier, a Assignment) error {
	var org, user *uuid.UUID
	if a.Account.Kind == KindOrganisation {
		org = &a.Account.ID
	} else {
		user = &a.Account.ID
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO listing_package_assignments (id, organisation_id, user_id, market_code, package_id, starts_on, ends_on, source, note,
			assigned_by, assigned_by_name, created_at, subscription_id)
		VALUES ($1, $2, $3, $4, $5, $6::date, $7::date, $8, $9, $10, $11, $12, $13)`,
		a.ID, org, user, a.MarketCode, a.PackageID, a.StartsOn, a.EndsOn, a.Source, a.Note, a.AssignedBy, a.AssignedByName, a.CreatedAt,
		a.SubscriptionID); err != nil {
		return fmt.Errorf("create package assignment: %w", err)
	}
	return nil
}

// GrantInvoiceTx gives the account the package a paid subscription grants,
// inside the caller's transaction (the subscription's activation): under the
// account and market's advisory lock, the active package g.PackageCode of
// the market replaces what overlaps from g.StartsOn on (like an
// administrator's assignment), with source INVOICE and the subscription's
// id, and a PACKAGE_ASSIGNED audit record. It returns false and does nothing
// when the subscription already has its assignment (idempotent) or the
// market has no active package with that code.
func GrantInvoiceTx(ctx context.Context, tx pgx.Tx, g InvoiceGrant) (bool, error) {
	if err := lockAccount(ctx, tx, g.Account, g.MarketCode); err != nil {
		return false, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM listing_package_assignments WHERE subscription_id = $1)`,
		g.SubscriptionID).Scan(&exists); err != nil {
		return false, fmt.Errorf("find invoice assignment: %w", err)
	}
	if exists {
		return false, nil
	}
	pkgs, err := readPackages(ctx, tx, `p.market_code = $1 AND p.code = $2 AND p.active`, g.MarketCode, g.PackageCode)
	if err != nil || len(pkgs) == 0 {
		return false, err
	}
	a := g.assignment(pkgs[0])
	replaced, err := replaceFrom(ctx, tx, g.Account, g.MarketCode, a.StartsOn, nil, a.CreatedAt)
	if err != nil {
		return false, err
	}
	if err := insertAssignment(ctx, tx, a); err != nil {
		return false, err
	}
	return true, adminaudit.Insert(ctx, tx, g.audit(a, pkgs[0], replaced))
}

// EndInvoiceTx ends the INVOICE assignment of a subscription (cancelled,
// refunded in full) inside the caller's transaction: cancelled when it
// starts today or later, else ended yesterday, so the market default applies
// from today; with a PACKAGE_ASSIGNMENT_ENDED audit record. False when there
// is none in force or scheduled.
func EndInvoiceTx(ctx context.Context, tx pgx.Tx, subscriptionID uuid.UUID, label string, at time.Time) (bool, error) {
	list, err := readAssignments(ctx, tx, `a.subscription_id = $1`, subscriptionID)
	if err != nil || len(list) == 0 {
		return false, err
	}
	if err := lockAccount(ctx, tx, list[0].Account, list[0].MarketCode); err != nil {
		return false, err
	}
	// Read again under the lock (an administrator may have replaced it).
	if list, err = readAssignments(ctx, tx, `a.subscription_id = $1`, subscriptionID); err != nil || len(list) == 0 {
		return false, err
	}
	a := list[0]
	if !endInvoice(&a, at) {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE listing_package_assignments SET ends_on = $2::date, cancelled_at = $3 WHERE id = $1`,
		a.ID, a.EndsOn, a.CancelledAt); err != nil {
		return false, fmt.Errorf("end invoice assignment: %w", err)
	}
	return true, adminaudit.Insert(ctx, tx, endInvoiceAudit(a, label, at))
}

// EndAssignments implements Store.
func (s *PostgresStore) EndAssignments(ctx context.Context, acct Account, market string, today time.Time, by *uuid.UUID, at time.Time,
	audit func(ended []Assignment) adminaudit.Event) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockAccount(ctx, tx, acct, market); err != nil {
			return err
		}
		ended, err := replaceFrom(ctx, tx, acct, market, Day(today), by, at)
		if err != nil {
			return err
		}
		if len(ended) == 0 {
			return ErrAssignmentNotFound
		}
		return adminaudit.Insert(ctx, tx, audit(ended))
	})
}

// ---------------------------------------------------------------------------
// Counting and enforcement
// ---------------------------------------------------------------------------

// liveListings returns the expiries of the account's active listings in the
// market, earliest first, leaving out the listing exclude.
func liveListings(ctx context.Context, q querier, acct Account, market string, exclude uuid.UUID, now time.Time) ([]time.Time, error) {
	owner := `l.organisation_id = $1`
	if acct.Kind == KindDirectOwner {
		owner = `l.organisation_id IS NULL AND l.seller_user_id = $1`
	}
	rows, err := q.Query(ctx, `
		SELECT l.expires_at FROM listings l JOIN markets m ON m.id = l.market_id
		WHERE `+owner+` AND m.code = $2 AND l.status IN ('PUBLISHED', 'PAUSED') AND l.expires_at > $3 AND l.id <> $4
		ORDER BY l.expires_at`, acct.ID, market, now, exclude)
	if err != nil {
		return nil, fmt.Errorf("count active listings: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (time.Time, error) {
		var t time.Time
		err := row.Scan(&t)
		return t.UTC(), err
	})
	if err != nil {
		return nil, fmt.Errorf("count active listings: %w", err)
	}
	return list, nil
}

// LiveListings implements Store.
func (s *PostgresStore) LiveListings(ctx context.Context, acct Account, market string, now time.Time) ([]time.Time, error) {
	return liveListings(ctx, s.pool, acct, market, uuid.Nil, now)
}

// EnforceTx is the quota check of a go-live, inside the transaction that
// makes listing live: it takes the account and market's advisory lock,
// resolves the package and counts the account's other active listings in
// the market. A *QuotaError when they already reach the limit; nil when
// there is room or no limit applies. The lock is held until the
// transaction ends, so a concurrent approval waits and then counts this one.
func EnforceTx(ctx context.Context, tx pgx.Tx, acct Account, market string, listingID uuid.UUID, now time.Time) error {
	if err := lockAccount(ctx, tx, acct, market); err != nil {
		return err
	}
	r, err := resolve(ctx, tx, acct, market, Day(now))
	if err != nil {
		return err
	}
	if r.Package == nil {
		return nil
	}
	live, err := liveListings(ctx, tx, acct, market, listingID, now)
	if err != nil {
		return err
	}
	if len(live) >= r.Package.ListingLimit {
		return &QuotaError{Limit: r.Package.ListingLimit, Used: len(live), Package: *r.Package, MarketCode: market, Account: acct}
	}
	return nil
}

// AccountMarkets implements Store.
func (s *PostgresStore) AccountMarkets(ctx context.Context, acct Account, today time.Time) ([]string, error) {
	owner, col, home := `l.organisation_id = $1`, `organisation_id`, `SELECT m.code FROM organisations o JOIN markets m ON m.id = o.market_id WHERE o.id = $1`
	if acct.Kind == KindDirectOwner {
		owner, col, home = `l.organisation_id IS NULL AND l.seller_user_id = $1`, `user_id`, `SELECT NULL::text WHERE false`
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT code FROM (
			SELECT m.code FROM listings l JOIN markets m ON m.id = l.market_id WHERE `+owner+`
			UNION SELECT market_code FROM listing_package_assignments
			      WHERE `+col+` = $1 AND cancelled_at IS NULL AND (ends_on IS NULL OR ends_on >= $2::date)
			UNION `+home+`
		) x WHERE code IS NOT NULL ORDER BY code`, acct.ID, Day(today))
	if err != nil {
		return nil, fmt.Errorf("read account markets: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// AccountName implements Store.
func (s *PostgresStore) AccountName(ctx context.Context, acct Account) (string, error) {
	sql := `SELECT name FROM organisations WHERE id = $1`
	if acct.Kind == KindDirectOwner {
		sql = `SELECT full_name FROM users WHERE id = $1 AND status <> 'DELETED'`
	}
	var name string
	if err := s.pool.QueryRow(ctx, sql, acct.ID).Scan(&name); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrAccountNotFound
		}
		return "", fmt.Errorf("read account name: %w", err)
	}
	return name, nil
}

// ForgetUserTx handles a deleted account's rows inside the account
// deletion's transaction (the users row is anonymised, not deleted, so no
// ON DELETE fires): the person's own current and scheduled assignments are
// ended or cancelled (their listings are withdrawn), and they are unlinked
// as the administrator who assigned or cancelled any assignment, their name
// replaced.
func ForgetUserTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, deletedName string, at time.Time) error {
	markets, err := readAssignments(ctx, tx, `a.user_id = $1 AND a.cancelled_at IS NULL AND (a.ends_on IS NULL OR a.ends_on >= $2::date)`,
		userID, Day(at))
	if err != nil {
		return err
	}
	seen := []string{}
	for _, a := range markets {
		if slices.Contains(seen, a.MarketCode) {
			continue
		}
		seen = append(seen, a.MarketCode)
		if _, err := replaceFrom(ctx, tx, Account{Kind: KindDirectOwner, ID: userID}, a.MarketCode, Day(at), nil, at); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE listing_package_assignments SET assigned_by = NULL, assigned_by_name = $2 WHERE assigned_by = $1`,
		userID, deletedName); err != nil {
		return fmt.Errorf("unlink package assigner: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE listing_package_assignments SET cancelled_by = NULL WHERE cancelled_by = $1`, userID); err != nil {
		return fmt.Errorf("unlink package canceller: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE listing_packages SET created_by = NULL WHERE created_by = $1`, userID); err != nil {
		return fmt.Errorf("unlink package creator: %w", err)
	}
	return nil
}
