package reference

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MemoryStore is an in-memory Store for development and tests.
type MemoryStore struct {
	mu   sync.RWMutex
	sets map[string][]Item
}

// NewMemoryStore returns a store holding the given rows.
func NewMemoryStore(sets map[string][]Item) *MemoryStore {
	s := &MemoryStore{sets: make(map[string][]Item)}
	for set, items := range sets {
		s.sets[set] = append([]Item(nil), items...)
	}
	return s
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, set string, f Filter) ([]Item, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Item{}
	for _, it := range s.sets[set] {
		if !it.Active && !f.IncludeInactive {
			continue
		}
		if f.Market != "" && it.MarketCode != nil && *it.MarketCode != f.Market {
			continue
		}
		out = append(out, it)
	}
	Order(out)
	return out, nil
}

// Put adds or replaces a row (what an administrator's edit would do).
func (s *MemoryStore) Put(set string, item Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.sets[set] {
		if it.Code == item.Code {
			s.sets[set][i] = item
			return
		}
	}
	s.sets[set] = append(s.sets[set], item)
}

// SetActive activates or deactivates a row. It reports whether the row exists.
func (s *MemoryStore) SetActive(set, code string, active bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.sets[set] {
		if it.Code == code {
			s.sets[set][i].Active = active
			return true
		}
	}
	return false
}

// PostgresStore reads reference_items (migrations/0007_reference_data.up.sql).
//
// NOTE: compiled but not yet exercised against a real database.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, set string, f Filter) ([]Item, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.code, r.label, r.singular_label, r.description, r.sort_order, m.code, r.parent_code,
		       r.group_code, r.group_label, r.group_sort_order, r.requires_verification, r.active
		FROM reference_items r
		LEFT JOIN markets m ON m.id = r.market_id
		WHERE r.set_code = $1
		  AND (r.active OR $2)
		  AND ($3 = '' OR r.market_id IS NULL OR m.code = $3)
		ORDER BY COALESCE(r.group_sort_order, 0), r.sort_order, r.label`,
		set, f.IncludeInactive, f.Market)
	if err != nil {
		return nil, fmt.Errorf("list reference set %s: %w", set, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) {
		var it Item
		var groupCode, groupLabel *string
		var groupSort *int
		if err := row.Scan(&it.Code, &it.Label, &it.SingularLabel, &it.Description, &it.SortOrder, &it.MarketCode, &it.ParentCode,
			&groupCode, &groupLabel, &groupSort, &it.RequiresVerification, &it.Active); err != nil {
			return Item{}, err
		}
		if groupCode != nil && groupLabel != nil {
			it.Group = &Group{Code: *groupCode, Label: *groupLabel}
			if groupSort != nil {
				it.Group.SortOrder = *groupSort
			}
		}
		return it, nil
	})
}
