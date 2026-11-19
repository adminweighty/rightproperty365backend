package tax

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// Store holds the tax rules. Create and Change run the overlap check (check
// receives every other rule of the same market and use) in the same
// transaction as the write, so two concurrent writes cannot both pass it.
type Store interface {
	Rules(ctx context.Context, f Filter) ([]Rule, error)
	Rule(ctx context.Context, id uuid.UUID) (Rule, error)
	Create(ctx context.Context, r Rule, check func(others []Rule) (adminaudit.Event, error)) error
	Change(ctx context.Context, id uuid.UUID, change func(r *Rule, others func() ([]Rule, error)) (adminaudit.Event, error)) (Rule, error)
}

// ---------------------------------------------------------------------------
// Memory
// ---------------------------------------------------------------------------

// MemoryStore keeps the rules in memory (development and tests).
type MemoryStore struct {
	mu    sync.Mutex
	rules []Rule
	audit *adminaudit.MemoryLog
}

// NewMemoryStore returns a store holding rules (SeedRules in development);
// audit receives the administrators' records (nil: none).
func NewMemoryStore(rules []Rule, audit *adminaudit.MemoryLog) *MemoryStore {
	s := &MemoryStore{audit: audit}
	for _, r := range rules {
		s.rules = append(s.rules, r.Clone())
	}
	return s
}

func (s *MemoryStore) othersLocked(r Rule) []Rule {
	out := []Rule{}
	for _, o := range s.rules {
		if o.ID != r.ID && o.MarketCode == r.MarketCode && o.AppliesTo == r.AppliesTo {
			out = append(out, o.Clone())
		}
	}
	return out
}

// Rules implements Store.
func (s *MemoryStore) Rules(_ context.Context, f Filter) ([]Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Rule{}
	for _, r := range s.rules {
		if f.Matches(r) {
			out = append(out, r.Clone())
		}
	}
	SortRules(out)
	return out, nil
}

// Rule implements Store.
func (s *MemoryStore) Rule(_ context.Context, id uuid.UUID) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rules {
		if r.ID == id {
			return r.Clone(), nil
		}
	}
	return Rule{}, ErrNotFound
}

// Create implements Store.
func (s *MemoryStore) Create(_ context.Context, r Rule, check func(others []Rule) (adminaudit.Event, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	audit, err := check(s.othersLocked(r))
	if err != nil {
		return err
	}
	s.rules = append(s.rules, r.Clone())
	if s.audit != nil {
		s.audit.Append(audit)
	}
	return nil
}

// Change implements Store.
func (s *MemoryStore) Change(_ context.Context, id uuid.UUID, change func(r *Rule, others func() ([]Rule, error)) (adminaudit.Event, error)) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, have := range s.rules {
		if have.ID != id {
			continue
		}
		next := have.Clone()
		audit, err := change(&next, func() ([]Rule, error) { return s.othersLocked(next), nil })
		if err != nil {
			return Rule{}, err
		}
		s.rules[i] = next.Clone()
		if s.audit != nil {
			s.audit.Append(audit)
		}
		return next, nil
	}
	return Rule{}, ErrNotFound
}

// ---------------------------------------------------------------------------
// Postgres
// ---------------------------------------------------------------------------

// PostgresStore reads and writes tax_rules (migration 0045).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const ruleColumns = `id, market_code, tax_name, rate::text, applies_to, inclusive, effective_from, effective_to, registration_number_label,
	status, is_sample, notes, created_at, updated_at`

func scanRule(row pgx.Row) (Rule, error) {
	var r Rule
	var rate string
	if err := row.Scan(&r.ID, &r.MarketCode, &r.TaxName, &rate, &r.AppliesTo, &r.Inclusive, &r.EffectiveFrom, &r.EffectiveTo,
		&r.RegistrationNumberLabel, &r.Status, &r.IsSample, &r.Notes, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return Rule{}, err
	}
	canonical, ok := ParseRate(rate)
	if !ok {
		return Rule{}, fmt.Errorf("tax rule %s: unexpected rate %q", r.ID, rate)
	}
	r.Rate = canonical
	r.EffectiveFrom, r.CreatedAt, r.UpdatedAt = r.EffectiveFrom.UTC(), r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	if r.EffectiveTo != nil {
		t := r.EffectiveTo.UTC()
		r.EffectiveTo = &t
	}
	return r, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func pgRules(ctx context.Context, q querier, f Filter) ([]Rule, error) {
	rows, err := q.Query(ctx, `SELECT `+ruleColumns+` FROM tax_rules
		WHERE ($1 = '' OR market_code = $1) AND ($2 = '' OR applies_to = $2) AND ($3 = '' OR status = $3)`,
		f.Market, f.AppliesTo, f.Status)
	if err != nil {
		return nil, fmt.Errorf("list tax rules: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Rule, error) { return scanRule(row) })
	if err != nil {
		return nil, fmt.Errorf("list tax rules: %w", err)
	}
	if out == nil {
		out = []Rule{}
	}
	SortRules(out)
	return out, nil
}

// Rules implements Store.
func (s *PostgresStore) Rules(ctx context.Context, f Filter) ([]Rule, error) {
	return pgRules(ctx, s.pool, f)
}

func pgRule(ctx context.Context, q querier, id uuid.UUID, lock bool) (Rule, error) {
	sql := `SELECT ` + ruleColumns + ` FROM tax_rules WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	r, err := scanRule(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	if err != nil {
		return Rule{}, fmt.Errorf("load tax rule: %w", err)
	}
	return r, nil
}

// Rule implements Store.
func (s *PostgresStore) Rule(ctx context.Context, id uuid.UUID) (Rule, error) {
	return pgRule(ctx, s.pool, id, false)
}

// lockScope serialises the writes of one market and use (the overlap check).
func lockScope(ctx context.Context, tx pgx.Tx, market, appliesTo string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('tax_rules:' || $1::text))`, market+":"+appliesTo); err != nil {
		return fmt.Errorf("lock tax rules: %w", err)
	}
	return nil
}

func others(ctx context.Context, tx pgx.Tx, r Rule) ([]Rule, error) {
	all, err := pgRules(ctx, tx, Filter{Market: r.MarketCode, AppliesTo: r.AppliesTo})
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, o := range all {
		if o.ID != r.ID {
			out = append(out, o)
		}
	}
	return out, nil
}

// Create implements Store.
func (s *PostgresStore) Create(ctx context.Context, r Rule, check func(others []Rule) (adminaudit.Event, error)) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, r.MarketCode, r.AppliesTo); err != nil {
		return err
	}
	list, err := others(ctx, tx, r)
	if err != nil {
		return err
	}
	audit, err := check(list)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tax_rules (id, market_code, tax_name, rate, applies_to, inclusive, effective_from, effective_to, registration_number_label,
			status, is_sample, notes, created_at, updated_at)
		VALUES ($1, $2, $3, $4::text::numeric, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		r.ID, r.MarketCode, r.TaxName, r.Rate, r.AppliesTo, r.Inclusive, r.EffectiveFrom, r.EffectiveTo, r.RegistrationNumberLabel,
		r.Status, r.IsSample, r.Notes, r.CreatedAt, r.UpdatedAt); err != nil {
		return fmt.Errorf("insert tax rule: %w", err)
	}
	if err := adminaudit.Insert(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Change implements Store.
func (s *PostgresStore) Change(ctx context.Context, id uuid.UUID, change func(r *Rule, others func() ([]Rule, error)) (adminaudit.Event, error)) (Rule, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Rule{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Market and use never change, so the scope can be locked from the row.
	var market, appliesTo string
	if err := tx.QueryRow(ctx, `SELECT market_code, applies_to FROM tax_rules WHERE id = $1`, id).Scan(&market, &appliesTo); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Rule{}, ErrNotFound
		}
		return Rule{}, fmt.Errorf("load tax rule: %w", err)
	}
	if err := lockScope(ctx, tx, market, appliesTo); err != nil {
		return Rule{}, err
	}
	cur, err := pgRule(ctx, tx, id, true)
	if err != nil {
		return Rule{}, err
	}
	next := cur.Clone()
	audit, err := change(&next, func() ([]Rule, error) { return others(ctx, tx, next) })
	if err != nil {
		return Rule{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tax_rules SET tax_name = $2, rate = $3::text::numeric, inclusive = $4, effective_from = $5, effective_to = $6,
			registration_number_label = $7, status = $8, is_sample = $9, notes = $10, updated_at = $11
		WHERE id = $1`,
		id, next.TaxName, next.Rate, next.Inclusive, next.EffectiveFrom, next.EffectiveTo, next.RegistrationNumberLabel, next.Status,
		next.IsSample, next.Notes, next.UpdatedAt); err != nil {
		return Rule{}, fmt.Errorf("update tax rule: %w", err)
	}
	if err := adminaudit.Insert(ctx, tx, audit); err != nil {
		return Rule{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Rule{}, err
	}
	return next, nil
}

// normaliseMarket upper-cases a market code.
func normaliseMarket(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
