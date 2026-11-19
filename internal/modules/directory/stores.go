package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

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

// copyOf returns a record that shares nothing mutable with the stored one.
func copyOf(r Record) Record {
	if r.Logo != nil {
		logo := *r.Logo
		r.Logo = &logo
	}
	return r
}

// OwnProfile returns the published (ACTIVE) profile of an organisation
// itself — an agency's, a law firm's, a host's, never an agent's — whether
// or not it is verified: a verified one first, then an agency's. It is what
// a listing names as its advertiser.
func (s *MemoryStore) OwnProfile(organisationID uuid.UUID) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var found *Record
	rank := func(r Record) int {
		n := 0
		if r.Verified {
			n += 2
		}
		if r.Kind == KindAgencies {
			n++
		}
		return n
	}
	for i, r := range s.records {
		if r.Kind != KindAgents && r.OrganisationID == organisationID && r.Status == StatusActive && (found == nil || rank(r) > rank(*found)) {
			found = &s.records[i]
		}
	}
	if found == nil {
		return Record{}, false
	}
	return copyOf(*found), true
}

// AgentProfile returns the published (ACTIVE) agent profile of a user,
// whether or not it is verified.
func (s *MemoryStore) AgentProfile(userID uuid.UUID) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.records {
		if r.Kind == KindAgents && r.AgentUserID != nil && *r.AgentUserID == userID && r.Status == StatusActive {
			return copyOf(r), true
		}
	}
	return Record{}, false
}

// VerifiedOrganisations implements Store.
func (s *MemoryStore) VerifiedOrganisations(context.Context) (int, error) {
	seen := map[uuid.UUID]bool{}
	for _, r := range s.active() {
		if r.Kind != KindAgents {
			seen[r.OrganisationID] = true
		}
	}
	return len(seen), nil
}

// Find implements Store.
func (s *MemoryStore) Find(_ context.Context, kind, slug string) (Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.records {
		if r.Kind == kind && r.Slug == slug {
			return copyOf(r), nil
		}
	}
	return Record{}, ErrProfileNotFound
}

// Managed implements Store.
func (s *MemoryStore) Managed(_ context.Context, organisationIDs []uuid.UUID, agentUserID uuid.UUID) ([]Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Record
	for _, r := range s.records {
		if (r.Kind != KindAgents && slices.Contains(organisationIDs, r.OrganisationID)) ||
			(r.Kind == KindAgents && r.AgentUserID != nil && *r.AgentUserID == agentUserID) {
			out = append(out, copyOf(r))
		}
	}
	return out, nil
}

// SetLogo implements Store.
func (s *MemoryStore) SetLogo(_ context.Context, id uuid.UUID, logo *Logo, now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		if s.records[i].ID != id {
			continue
		}
		previous := ""
		if s.records[i].Logo != nil {
			previous = s.records[i].Logo.ObjectKey
		}
		s.records[i].Logo, s.records[i].UpdatedAt = nil, now
		if logo != nil {
			cp := *logo
			s.records[i].Logo = &cp
		}
		return previous, nil
	}
	return "", ErrProfileNotFound
}

func (s *MemoryStore) active() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Record
	for _, r := range s.records {
		// Public means published (ACTIVE) and verified.
		if r.Status == StatusActive && r.Verified {
			out = append(out, copyOf(r))
		}
	}
	return out
}

func matchesText(r Record, q string) bool {
	haystack := strings.ToLower(strings.Join([]string{r.Name, r.Role, r.Tagline, r.Description, strings.Join(r.Services, " "), strings.Join(r.Areas, " "), r.CityName}, " "))
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
		if r.Kind == kind && (f.City == "" || r.CityCode == f.City) && (f.Cities == nil || slices.Contains(f.Cities, r.CityCode)) &&
			(f.Country == "" || r.MarketCode == f.Country) &&
			(f.Trade == "" || r.TradeCode == f.Trade) && matchesText(r, f.Q) {
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

// ByID implements Store.
func (s *MemoryStore) ByID(_ context.Context, id uuid.UUID) (Record, error) {
	for _, r := range s.active() {
		if r.ID == id {
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
	p.since_year, p.status, p.updated_at, p.organisation_id, p.agent_user_id, COALESCE(p.specialism, ''),
	COALESCE(mk.code, ''), COALESCE(p.city_name, ''), COALESCE(p.region_name, ''), p.verified_at,
	p.logo_object_key, COALESCE(p.logo_content_type, ''), COALESCE(p.logo_width, 0), COALESCE(p.logo_height, 0),
	COALESCE(p.logo_checksum, ''), COALESCE(p.logo_size_bytes, 0), p.logo_updated_at`

// profileFrom joins the profile's country (its market).
const profileFrom = ` FROM directory_profiles p LEFT JOIN markets mk ON mk.id = p.market_id `

func scanProfile(row pgx.CollectableRow) (Record, error) {
	var r Record
	var offerings string
	var logo Logo
	var logoKey *string
	var logoUpdatedAt *time.Time
	if err := row.Scan(&r.ID, &r.Kind, &r.Slug, &r.Name, &r.Role, &r.Tagline, &r.Description, &r.CityCode, &r.Areas, &r.Services,
		&offerings, &r.TradeCode, &r.Verified, &r.Sponsored, &r.Since, &r.Status, &r.UpdatedAt, &r.OrganisationID, &r.AgentUserID, &r.Specialism,
		&r.MarketCode, &r.CityName, &r.RegionName, &r.VerifiedAt,
		&logoKey, &logo.ContentType, &logo.Width, &logo.Height, &logo.Checksum, &logo.Size, &logoUpdatedAt); err != nil {
		return Record{}, err
	}
	if logoKey != nil && *logoKey != "" {
		logo.ObjectKey = *logoKey
		if logoUpdatedAt != nil {
			logo.UpdatedAt = logoUpdatedAt.UTC()
		}
		r.Logo = &logo
	}
	if err := json.Unmarshal([]byte(offerings), &r.Offerings); err != nil {
		return Record{}, fmt.Errorf("decode offerings: %w", err)
	}
	r.UpdatedAt = r.UpdatedAt.UTC()
	return r, nil
}

// anyStatus runs a query over profiles WHATEVER THEIR STATUS (their managers' routes only).
func (s *PostgresStore) anyStatus(ctx context.Context, where string, args ...any) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+profileColumns+profileFrom+`WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanProfile)
}

// Find implements Store.
func (s *PostgresStore) Find(ctx context.Context, kind, slug string) (Record, error) {
	records, err := s.anyStatus(ctx, `p.kind = $1 AND p.slug = $2`, kind, slug)
	if err != nil {
		return Record{}, err
	}
	if len(records) == 0 {
		return Record{}, ErrProfileNotFound
	}
	return records[0], nil
}

// Managed implements Store.
func (s *PostgresStore) Managed(ctx context.Context, organisationIDs []uuid.UUID, agentUserID uuid.UUID) ([]Record, error) {
	if organisationIDs == nil {
		organisationIDs = []uuid.UUID{}
	}
	return s.anyStatus(ctx, `(p.kind <> 'agents' AND p.organisation_id = ANY($1::uuid[])) OR (p.kind = 'agents' AND p.agent_user_id = $2)
		ORDER BY p.kind, p.name, p.id`, organisationIDs, agentUserID)
}

// SetLogo implements Store. The row is locked while the old key is read, so
// two uploads at once cannot both believe they replaced the same object.
func (s *PostgresStore) SetLogo(ctx context.Context, id uuid.UUID, logo *Logo, now time.Time) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var previous *string
	err = tx.QueryRow(ctx, `SELECT logo_object_key FROM directory_profiles WHERE id = $1 FOR UPDATE`, id).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrProfileNotFound
	}
	if err != nil {
		return "", err
	}
	if logo == nil {
		_, err = tx.Exec(ctx, `
			UPDATE directory_profiles
			SET logo_object_key = NULL, logo_content_type = NULL, logo_width = NULL, logo_height = NULL, logo_checksum = NULL,
			    logo_size_bytes = NULL, logo_updated_at = $2, updated_at = $2
			WHERE id = $1`, id, now)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE directory_profiles
			SET logo_object_key = $3, logo_content_type = $4, logo_width = $5, logo_height = $6, logo_checksum = $7,
			    logo_size_bytes = $8, logo_updated_at = $2, updated_at = $2
			WHERE id = $1`, id, now, logo.ObjectKey, logo.ContentType, logo.Width, logo.Height, logo.Checksum, logo.Size)
	}
	if err != nil {
		return "", fmt.Errorf("set logo: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	if previous == nil {
		return "", nil
	}
	return *previous, nil
}

// VerifiedOrganisations implements Store.
func (s *PostgresStore) VerifiedOrganisations(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT p.organisation_id) FROM directory_profiles p JOIN organisations o ON o.id = p.organisation_id
		WHERE p.kind <> 'agents' AND p.status = 'ACTIVE' AND p.verified AND o.status = 'ACTIVE'`).Scan(&n)
	return n, err
}

func (s *PostgresStore) profiles(ctx context.Context, tail string, args ...any) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+profileColumns+profileFrom+`WHERE p.status = 'ACTIVE' AND p.verified AND `+tail, args...)
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
	if f.Cities != nil { // the cities of the country and/or region asked for
		add(`p.city_code = ANY($%d::text[])`, f.Cities)
	}
	if f.Country != "" { // the profile's own country
		add(`mk.code = $%d`, f.Country)
	}
	if f.Trade != "" {
		add(`p.trade_code = $%d`, f.Trade)
	}
	for _, w := range strings.Fields(f.Q) {
		add(`(p.name || ' ' || COALESCE(p.role_title, '') || ' ' || COALESCE(p.tagline, '') || ' ' || COALESCE(p.about, '') || ' ' ||
		     array_to_string(p.services, ' ') || ' ' || array_to_string(p.areas, ' ') || ' ' || COALESCE(p.city_name, '')) ILIKE $%d`, "%"+likeEscaper.Replace(w)+"%")
	}
	where := strings.Join(conds, " AND ")
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+profileFrom+`WHERE p.status = 'ACTIVE' AND p.verified AND `+where, args...).Scan(&total); err != nil {
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

// ByID implements Store.
func (s *PostgresStore) ByID(ctx context.Context, id uuid.UUID) (Record, error) {
	records, err := s.profiles(ctx, `p.id = $1`, id)
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
