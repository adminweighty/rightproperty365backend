package availability

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store holds the calendars. The database is the source of truth for
// availability: the search index holds none of it.
type Store interface {
	// States returns the calendar of each listing asked for (a listing
	// without ranges has an empty one).
	States(ctx context.Context, listingIDs []uuid.UUID) (map[uuid.UUID]State, error)
	// Update changes one listing's calendar atomically: fn receives the
	// current state and returns the new ranges and the audit entries, and
	// nobody else can change that calendar in between. An error from fn
	// changes nothing. It returns the new state.
	Update(ctx context.Context, listingID uuid.UUID, at time.Time, fn func(State) ([]Range, []Audit, error)) (State, error)
	// Overlapping returns the listings that have at least one unavailable
	// night in first..last.
	Overlapping(ctx context.Context, first, last Date) ([]uuid.UUID, error)
}

// MemoryStore is the in-memory Store for development and tests.
type MemoryStore struct {
	mu     sync.RWMutex
	states map[uuid.UUID]State
	audits []Audit
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{states: map[uuid.UUID]State{}} }

func cloneState(s State) State {
	out := State{Ranges: append([]Range{}, s.Ranges...)}
	if s.UpdatedAt != nil {
		t := *s.UpdatedAt
		out.UpdatedAt = &t
	}
	return out
}

// Put sets a listing's calendar (seed and test helper).
func (s *MemoryStore) Put(listingID uuid.UUID, ranges []Range, updatedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sorted := append([]Range{}, ranges...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].First < sorted[j].First })
	s.states[listingID] = State{Ranges: sorted, UpdatedAt: &updatedAt}
}

// Audits returns the audit trail, oldest first (tests).
func (s *MemoryStore) Audits() []Audit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Audit{}, s.audits...)
}

// States implements Store.
func (s *MemoryStore) States(_ context.Context, listingIDs []uuid.UUID) (map[uuid.UUID]State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[uuid.UUID]State, len(listingIDs))
	for _, id := range listingIDs {
		out[id] = cloneState(s.states[id])
	}
	return out, nil
}

// Update implements Store.
func (s *MemoryStore) Update(_ context.Context, listingID uuid.UUID, at time.Time, fn func(State) ([]Range, []Audit, error)) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ranges, audits, err := fn(cloneState(s.states[listingID]))
	if err != nil {
		return State{}, err
	}
	s.states[listingID] = State{Ranges: append([]Range{}, ranges...), UpdatedAt: &at}
	s.audits = append(s.audits, audits...)
	return cloneState(s.states[listingID]), nil
}

// Overlapping implements Store.
func (s *MemoryStore) Overlapping(_ context.Context, first, last Date) ([]uuid.UUID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []uuid.UUID
	for id, state := range s.states {
		if Overlaps(state.Ranges, first, last) {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// PostgresStore keeps the ranges in listing_availability, the trail in
// listing_availability_audit and the last change in
// listings.availability_updated_at (migration 0015).
//
// NOTE: compiled but never run against a database.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func idStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// ranges loads the ranges of the listings, by listing, sorted by first night.
func loadRanges(ctx context.Context, q querier, listingIDs []uuid.UUID) (map[uuid.UUID][]Range, error) {
	rows, err := q.Query(ctx, `
		SELECT listing_id, id, first_night::text, last_night::text, reason, COALESCE(note, ''), updated_at, updated_by
		FROM listing_availability
		WHERE listing_id = ANY($1::uuid[])
		ORDER BY listing_id, first_night`, idStrings(listingIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]Range{}
	for rows.Next() {
		var listingID uuid.UUID
		var first, last string
		var r Range
		if err := rows.Scan(&listingID, &r.ID, &first, &last, &r.Reason, &r.Note, &r.UpdatedAt, &r.UpdatedBy); err != nil {
			return nil, err
		}
		if r.First, err = ParseDate(first); err != nil {
			return nil, fmt.Errorf("first_night %q: %w", first, err)
		}
		if r.Last, err = ParseDate(last); err != nil {
			return nil, fmt.Errorf("last_night %q: %w", last, err)
		}
		r.UpdatedAt = r.UpdatedAt.UTC()
		out[listingID] = append(out[listingID], r)
	}
	return out, rows.Err()
}

// States implements Store.
func (s *PostgresStore) States(ctx context.Context, listingIDs []uuid.UUID) (map[uuid.UUID]State, error) {
	out := make(map[uuid.UUID]State, len(listingIDs))
	if len(listingIDs) == 0 {
		return out, nil
	}
	ranges, err := loadRanges(ctx, s.pool, listingIDs)
	if err != nil {
		return nil, fmt.Errorf("load availability: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT id, availability_updated_at FROM listings WHERE id = ANY($1::uuid[])`, idStrings(listingIDs))
	if err != nil {
		return nil, fmt.Errorf("load availability_updated_at: %w", err)
	}
	defer rows.Close()
	updated := map[uuid.UUID]*time.Time{}
	for rows.Next() {
		var id uuid.UUID
		var at *time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		if at != nil {
			utc := at.UTC()
			at = &utc
		}
		updated[id] = at
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range listingIDs {
		state := State{Ranges: ranges[id], UpdatedAt: updated[id]}
		if state.Ranges == nil {
			state.Ranges = []Range{}
		}
		out[id] = state
	}
	return out, nil
}

// Update implements Store. The listing row is locked for the length of the
// transaction, so two changes to one calendar are applied one after the other.
func (s *PostgresStore) Update(ctx context.Context, listingID uuid.UUID, at time.Time, fn func(State) ([]Range, []Audit, error)) (State, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return State{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	current := State{}
	err = tx.QueryRow(ctx, `SELECT availability_updated_at FROM listings WHERE id = $1 FOR UPDATE`, listingID).Scan(&current.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, ErrNotFound
	}
	if err != nil {
		return State{}, fmt.Errorf("lock listing: %w", err)
	}
	loaded, err := loadRanges(ctx, tx, []uuid.UUID{listingID})
	if err != nil {
		return State{}, fmt.Errorf("load availability: %w", err)
	}
	current.Ranges = loaded[listingID]

	ranges, audits, err := fn(current)
	if err != nil {
		return State{}, err
	}
	// The ranges of a listing are few: replace them wholesale.
	if _, err := tx.Exec(ctx, `DELETE FROM listing_availability WHERE listing_id = $1`, listingID); err != nil {
		return State{}, fmt.Errorf("replace availability: %w", err)
	}
	for _, r := range ranges {
		if _, err := tx.Exec(ctx, `
			INSERT INTO listing_availability (id, listing_id, first_night, last_night, reason, note, updated_at, updated_by)
			VALUES ($1, $2, $3::text::date, $4::text::date, $5, NULLIF($6, ''), $7, $8)`,
			r.ID, listingID, r.First.String(), r.Last.String(), r.Reason, r.Note, r.UpdatedAt, r.UpdatedBy); err != nil {
			return State{}, fmt.Errorf("insert availability range: %w", err)
		}
	}
	for _, a := range audits {
		if _, err := tx.Exec(ctx, `
			INSERT INTO listing_availability_audit (id, listing_id, actor_user_id, first_night, last_night, status, note, created_at)
			VALUES ($1, $2, $3, $4::text::date, $5::text::date, $6, NULLIF($7, ''), $8)`,
			a.ID, listingID, a.ActorID, a.From.String(), a.To.String(), a.Status, a.Note, a.At); err != nil {
			return State{}, fmt.Errorf("insert availability audit: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE listings SET availability_updated_at = $2 WHERE id = $1`, listingID, at); err != nil {
		return State{}, fmt.Errorf("set availability_updated_at: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return State{}, err
	}
	return State{Ranges: ranges, UpdatedAt: &at}, nil
}

// Overlapping implements Store.
func (s *PostgresStore) Overlapping(ctx context.Context, first, last Date) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT listing_id FROM listing_availability
		WHERE first_night <= $2::text::date AND last_night >= $1::text::date
		ORDER BY listing_id`, first.String(), last.String())
	if err != nil {
		return nil, fmt.Errorf("find unavailable listings: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
