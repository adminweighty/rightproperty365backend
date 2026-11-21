package branches

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// MemoryDeps connects the memory store to the other memory stores.
type MemoryDeps struct {
	// Member returns the name of an organisation member (ok=false otherwise).
	Member func(org, user uuid.UUID) (name string, ok bool)
	// Listing returns a listing's organisation and label (ok=false when unknown).
	Listing func(listing uuid.UUID) (org *uuid.UUID, label string, ok bool)
	// OnListingBranch tells the listing (and request) stores a listing's
	// new branch.
	OnListingBranch func(listing uuid.UUID, branch *uuid.UUID)
	Audit           orgaudit.Sink
}

// MemoryStore is the in-memory Store.
type MemoryStore struct {
	deps     MemoryDeps
	mu       sync.Mutex
	branches map[uuid.UUID]Branch
	pins     map[[2]uuid.UUID]uuid.UUID // (org, user) -> branch
}

// NewMemoryStore returns an empty store.
func NewMemoryStore(deps MemoryDeps) *MemoryStore {
	return &MemoryStore{deps: deps, branches: map[uuid.UUID]Branch{}, pins: map[[2]uuid.UUID]uuid.UUID{}}
}

// SetDeps replaces the dependencies (wiring after construction).
func (s *MemoryStore) SetDeps(d MemoryDeps) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deps = d
}

// BranchOf is the identity memory store's lookup of a member's pin.
func (s *MemoryStore) BranchOf(org, user uuid.UUID) *uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.pins[[2]uuid.UUID{org, user}]; ok {
		return &b
	}
	return nil
}

func (s *MemoryStore) audit(e orgaudit.Event) {
	if s.deps.Audit != nil {
		s.deps.Audit(e)
	}
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, org uuid.UUID) ([]Branch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Branch{}
	for _, b := range s.branches {
		if b.OrganisationID == org {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, org, id uuid.UUID) (Branch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.branches[id]
	if !ok || b.OrganisationID != org {
		return Branch{}, ErrNotFound
	}
	return b, nil
}

func (s *MemoryStore) taken(b Branch) bool {
	for _, o := range s.branches {
		if o.ID != b.ID && o.OrganisationID == b.OrganisationID && strings.EqualFold(o.Name, b.Name) {
			return true
		}
	}
	return false
}

// Create implements Store.
func (s *MemoryStore) Create(_ context.Context, b Branch, audit orgaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.taken(b) {
		return ErrNameTaken
	}
	n := 0
	for _, o := range s.branches {
		if o.OrganisationID == b.OrganisationID {
			n++
		}
	}
	if n >= MaxBranches {
		return ErrTooMany
	}
	s.branches[b.ID] = b
	s.audit(audit)
	return nil
}

// Update implements Store.
func (s *MemoryStore) Update(_ context.Context, b Branch, audit orgaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.branches[b.ID]; !ok {
		return ErrNotFound
	}
	if s.taken(b) {
		return ErrNameTaken
	}
	s.branches[b.ID] = b
	s.audit(audit)
	return nil
}

// PinMember implements Store.
func (s *MemoryStore) PinMember(_ context.Context, org, user uuid.UUID, branch *uuid.UUID, audit func(string) orgaudit.Event) error {
	name, ok := "", false
	if s.deps.Member != nil {
		name, ok = s.deps.Member(org, user)
	}
	if !ok {
		return ErrNotMember
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := [2]uuid.UUID{org, user}
	if branch == nil {
		delete(s.pins, key)
	} else {
		s.pins[key] = *branch
	}
	s.audit(audit(name))
	return nil
}

// TieListing implements Store.
func (s *MemoryStore) TieListing(_ context.Context, org, listing uuid.UUID, branch *uuid.UUID, audit func(string) orgaudit.Event) error {
	if s.deps.Listing == nil {
		return ErrNoListing
	}
	owner, label, ok := s.deps.Listing(listing)
	if !ok || owner == nil || *owner != org {
		return ErrNoListing
	}
	if s.deps.OnListingBranch != nil {
		s.deps.OnListingBranch(listing, branch)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit(audit(label))
	return nil
}

// Pins implements Store.
func (s *MemoryStore) Pins(_ context.Context, org uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]uuid.UUID{}
	for k, b := range s.pins {
		if k[0] == org {
			out[k[1]] = b
		}
	}
	return out, nil
}

// PostgresStore is the production Store.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func scan(row pgx.Row) (Branch, error) {
	var b Branch
	err := row.Scan(&b.ID, &b.OrganisationID, &b.Name, &b.Active, &b.CreatedAt, &b.UpdatedAt)
	b.CreatedAt, b.UpdatedAt = b.CreatedAt.UTC(), b.UpdatedAt.UTC()
	return b, err
}

const columns = `id, organisation_id, name, active, created_at, updated_at`

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, org uuid.UUID) ([]Branch, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM organisation_branches WHERE organisation_id = $1 ORDER BY lower(name), id`, org)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Branch, error) { return scan(row) })
	if out == nil {
		out = []Branch{}
	}
	return out, err
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, org, id uuid.UUID) (Branch, error) {
	b, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM organisation_branches WHERE organisation_id = $1 AND id = $2`, org, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Branch{}, ErrNotFound
	}
	return b, err
}

func nameTaken(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Create implements Store.
func (s *PostgresStore) Create(ctx context.Context, b Branch, audit orgaudit.Event) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, "branches:"+b.OrganisationID.String()); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM organisation_branches WHERE organisation_id = $1`, b.OrganisationID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxBranches {
		return ErrTooMany
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organisation_branches (id, organisation_id, name, active, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		b.ID, b.OrganisationID, b.Name, b.Active, b.CreatedAt, b.UpdatedAt); err != nil {
		if nameTaken(err) {
			return ErrNameTaken
		}
		return err
	}
	if err := orgaudit.Insert(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Update implements Store.
func (s *PostgresStore) Update(ctx context.Context, b Branch, audit orgaudit.Event) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	tag, err := tx.Exec(ctx, `UPDATE organisation_branches SET name = $3, active = $4, updated_at = $5 WHERE organisation_id = $1 AND id = $2`,
		b.OrganisationID, b.ID, b.Name, b.Active, b.UpdatedAt)
	if err != nil {
		if nameTaken(err) {
			return ErrNameTaken
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := orgaudit.Insert(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PinMember implements Store.
func (s *PostgresStore) PinMember(ctx context.Context, org, user uuid.UUID, branch *uuid.UUID, audit func(string) orgaudit.Event) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var name string
	err = tx.QueryRow(ctx, `
		UPDATE memberships m SET branch_id = $3 FROM users u
		WHERE m.organisation_id = $1 AND m.user_id = $2 AND m.status <> 'REMOVED' AND u.id = m.user_id
		RETURNING u.full_name`, org, user, branch).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotMember
	}
	if err != nil {
		return err
	}
	if err := orgaudit.Insert(ctx, tx, audit(name)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TieListing implements Store. The branch is private: the search index
// does not hold it, so nothing else changes.
func (s *PostgresStore) TieListing(ctx context.Context, org, listing uuid.UUID, branch *uuid.UUID, audit func(string) orgaudit.Event) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var label string
	err = tx.QueryRow(ctx, `UPDATE listings SET branch_id = $3 WHERE id = $2 AND organisation_id = $1
		RETURNING COALESCE(reference, '') || ' ' || title`, org, listing, branch).Scan(&label)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoListing
	}
	if err != nil {
		return err
	}
	if err := orgaudit.Insert(ctx, tx, audit(label)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Pins implements Store.
func (s *PostgresStore) Pins(ctx context.Context, org uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT user_id, branch_id FROM memberships WHERE organisation_id = $1 AND branch_id IS NOT NULL AND status <> 'REMOVED'`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]uuid.UUID{}
	for rows.Next() {
		var u, b uuid.UUID
		if err := rows.Scan(&u, &b); err != nil {
			return nil, err
		}
		out[u] = b
	}
	return out, rows.Err()
}
