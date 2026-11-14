package directory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MemoryStore is the in-memory Store for development and tests.
type MemoryStore struct {
	mu      sync.RWMutex
	records []Record
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Put adds or replaces a profile (seed and test helper).
func (s *MemoryStore) Put(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		if s.records[i].Kind == r.Kind && s.records[i].Slug == r.Slug {
			s.records[i] = r
			return
		}
	}
	s.records = append(s.records, r)
}

// Has reports whether a profile with that address exists in ANY status (a
// new profile's slug must not collide with a hidden one either).
func (s *MemoryStore) Has(kind, slug string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.records {
		if r.Kind == kind && r.Slug == slug {
			return true
		}
	}
	return false
}

func (s *MemoryStore) active() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Record
	for _, r := range s.records {
		// Public means published (ACTIVE) and verified.
		if r.Status == StatusActive && r.Verified {
			out = append(out, r)
		}
	}
	return out
}

func matchesText(r Record, q string) bool {
	haystack := strings.ToLower(strings.Join([]string{r.Name, r.Role, r.Tagline, r.Description, strings.Join(r.Services, " "), strings.Join(r.Areas, " ")}, " "))
	for _, w := range strings.Fields(strings.ToLower(q)) {
		if !strings.Contains(haystack, w) {
			return false
		}
	}
	return true
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, kind string, f Filter, limit, offset int) ([]Record, int, error) {
	var found []Record
	for _, r := range s.active() {
		if r.Kind == kind && (f.City == "" || r.CityCode == f.City) && (f.Trade == "" || r.TradeCode == f.Trade) && matchesText(r, f.Q) {
			found = append(found, r)
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].Sponsored != found[j].Sponsored {
			return found[i].Sponsored
		}
		return found[i].Name < found[j].Name
	})
	total := len(found)
	if offset > total {
		offset = total
	}
	found = found[offset:]
	if len(found) > limit {
		found = found[:limit]
	}
	return found, total, nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, kind, slug string) (Record, error) {
	for _, r := range s.active() {
		if r.Kind == kind && r.Slug == slug {
			return r, nil
		}
	}
	return Record{}, ErrProfileNotFound
}

// AgencyOf implements Store.
func (s *MemoryStore) AgencyOf(_ context.Context, organisationID uuid.UUID) (*Record, error) {
	for _, r := range s.active() {
		if r.Kind == KindAgencies && r.OrganisationID == organisationID {
			return &r, nil
		}
	}
	return nil, nil
}

// AgentsOf implements Store.
func (s *MemoryStore) AgentsOf(_ context.Context, organisationID uuid.UUID) ([]Record, error) {
	var out []Record
	for _, r := range s.active() {
		if r.Kind == KindAgents && r.OrganisationID == organisationID {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Counts implements Store.
func (s *MemoryStore) Counts(context.Context) (map[string]int, error) {
	out := map[string]int{}
	for _, r := range s.active() {
		out[r.Kind]++
	}
	return out, nil
}

// Entries implements Store.
func (s *MemoryStore) Entries(context.Context) ([]Entry, error) {
	out := []Entry{}
	for _, r := range s.active() {
		out = append(out, Entry{Kind: r.Kind, Slug: r.Slug, UpdatedAt: r.UpdatedAt.UTC()})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}

// PostgresStore reads directory_profiles (content columns are added by
// migrations/0008_listings_directory.up.sql). The table has no contact columns.
//
// NOTE: compiled but not yet exercised against a real database.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the Postgres adapter.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const profileColumns = `p.id, p.kind, p.slug, p.name, COALESCE(p.role_title, ''), COALESCE(p.tagline, ''), COALESCE(p.about, ''),
	COALESCE(p.city_code, ''), p.areas, p.services, p.offerings::text, COALESCE(p.trade_code, ''), p.verified, p.sponsored,
	p.since_year, p.status, p.updated_at, p.organisation_id, p.agent_user_id, COALESCE(p.specialism, '')`

func scanProfile(row pgx.CollectableRow) (Record, error) {
	var r Record
	var offerings string
	if err := row.Scan(&r.ID, &r.Kind, &r.Slug, &r.Name, &r.Role, &r.Tagline, &r.Description, &r.CityCode, &r.Areas, &r.Services,
		&offerings, &r.TradeCode, &r.Verified, &r.Sponsored, &r.Since, &r.Status, &r.UpdatedAt, &r.OrganisationID, &r.AgentUserID, &r.Specialism); err != nil {
		return Record{}, err
	}
	if err := json.Unmarshal([]byte(offerings), &r.Offerings); err != nil {
		return Record{}, fmt.Errorf("decode offerings: %w", err)
	}
	r.UpdatedAt = r.UpdatedAt.UTC()
	return r, nil
}

func (s *PostgresStore) profiles(ctx context.Context, tail string, args ...any) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+profileColumns+` FROM directory_profiles p WHERE p.status = 'ACTIVE' AND p.verified AND `+tail, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanProfile)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, kind string, f Filter, limit, offset int) ([]Record, int, error) {
	conds, args := []string{`p.kind = $1`}, []any{kind}
	add := func(format string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(format, len(args)))
	}
	if f.City != "" {
		add(`p.city_code = $%d`, f.City)
	}
	if f.Trade != "" {
		add(`p.trade_code = $%d`, f.Trade)
	}
	for _, w := range strings.Fields(f.Q) {
		add(`(p.name || ' ' || COALESCE(p.role_title, '') || ' ' || COALESCE(p.tagline, '') || ' ' || COALESCE(p.about, '') || ' ' ||
		     array_to_string(p.services, ' ') || ' ' || array_to_string(p.areas, ' ')) ILIKE $%d`, "%"+likeEscaper.Replace(w)+"%")
	}
	where := strings.Join(conds, " AND ")
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM directory_profiles p WHERE p.status = 'ACTIVE' AND p.verified AND `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	records, err := s.profiles(ctx, where+fmt.Sprintf(` ORDER BY p.sponsored DESC, p.name, p.id LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	return records, total, err
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, kind, slug string) (Record, error) {
	records, err := s.profiles(ctx, `p.kind = $1 AND p.slug = $2`, kind, slug)
	if err != nil {
		return Record{}, err
	}
	if len(records) == 0 {
		return Record{}, ErrProfileNotFound
	}
	return records[0], nil
}

// AgencyOf implements Store.
func (s *PostgresStore) AgencyOf(ctx context.Context, organisationID uuid.UUID) (*Record, error) {
	records, err := s.profiles(ctx, `p.kind = 'agencies' AND p.organisation_id = $1 ORDER BY p.name LIMIT 1`, organisationID)
	if err != nil || len(records) == 0 {
		return nil, err
	}
	return &records[0], nil
}

// AgentsOf implements Store.
func (s *PostgresStore) AgentsOf(ctx context.Context, organisationID uuid.UUID) ([]Record, error) {
	return s.profiles(ctx, `p.kind = 'agents' AND p.organisation_id = $1 ORDER BY p.name, p.id`, organisationID)
}

// Counts implements Store.
func (s *PostgresStore) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, count(*) FROM directory_profiles WHERE status = 'ACTIVE' AND verified GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		out[kind] = n
	}
	return out, rows.Err()
}

// Entries implements Store.
func (s *PostgresStore) Entries(ctx context.Context) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, slug, updated_at FROM directory_profiles WHERE status = 'ACTIVE' AND verified ORDER BY kind, slug`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := row.Scan(&e.Kind, &e.Slug, &e.UpdatedAt)
		e.UpdatedAt = e.UpdatedAt.UTC()
		return e, err
	})
}
