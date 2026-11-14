package listings

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the Postgres Store and Search adapter (the listing columns
// are added by migrations/0008_listings_directory.up.sql). Search is done
// with ILIKE and GROUP BY, which is adequate for the first markets; the
// Search port is where a dedicated engine plugs in later.
//
// NOTE: compiled but not yet exercised against a real database.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the Postgres adapter.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// publicCondition is the FR-006 rule. It appears in every query that can
// return a listing to the public.
const publicCondition = `l.status = 'PUBLISHED' AND l.expires_at > $1 AND l.slug IS NOT NULL`

const recordColumns = `
	l.id, l.slug, COALESCE(l.reference, ''), l.title, COALESCE(l.deal, ''), COALESCE(l.category, ''), COALESCE(l.property_type, ''),
	l.price_amount::text, l.currency_code, COALESCE(l.price_period, ''), COALESCE(l.suburb_code, ''), COALESCE(l.city_code, ''), m.code,
	l.bedrooms, l.bathrooms, l.floor_area_m2, l.land_area_m2, COALESCE(l.summary, ''), COALESCE(l.description, ''), l.featured,
	l.status::text, l.published_at, l.expires_at, l.updated_at, l.organisation_id, l.agent_user_id, l.seller_user_id,
	COALESCE(l.private_seller_name, 'Private seller'),
	ag.slug, ag.name, ag.verified, at.slug, at.name, at.verified,
	COALESCE((SELECT array_agg(f.feature_code ORDER BY f.position) FROM listing_features f WHERE f.listing_id = l.id), '{}'),
	COALESCE((SELECT jsonb_agg(jsonb_build_object('url', p.url, 'alt', p.alt, 'width', p.width, 'height', p.height) ORDER BY p.position)
	          FROM listing_photos p WHERE p.listing_id = l.id), '[]'::jsonb)::text`

// recordFrom joins the public profiles of the listing's agency and agent.
// Only name, slug and the verified flag are read: profiles have no contact columns.
const recordFrom = `
	FROM listings l
	JOIN markets m ON m.id = l.market_id
	LEFT JOIN directory_profiles ag ON ag.kind = 'agencies' AND ag.organisation_id = l.organisation_id AND ag.status = 'ACTIVE'
	LEFT JOIN directory_profiles at ON at.kind = 'agents' AND at.agent_user_id = l.agent_user_id AND at.status = 'ACTIVE'`

func scanRecord(row pgx.CollectableRow) (Record, error) {
	var (
		r                            Record
		priceText, sellerName        string
		publishedAt, expiresAt       *time.Time
		agSlug, agName, atSlug, atNm *string
		agVerified, atVerified       *bool
		photos                       string
	)
	if err := row.Scan(&r.ID, &r.Slug, &r.Reference, &r.Title, &r.Deal, &r.Category, &r.PropertyType,
		&priceText, &r.Price.Currency, &r.PricePeriod, &r.Suburb, &r.City, &r.MarketCode,
		&r.Bedrooms, &r.Bathrooms, &r.FloorArea, &r.LandArea, &r.Summary, &r.Description, &r.Featured,
		&r.Status, &publishedAt, &expiresAt, &r.UpdatedAt, &r.OrganisationID, &r.AgentUserID, &r.SellerUserID,
		&sellerName, &agSlug, &agName, &agVerified, &atSlug, &atNm, &atVerified, &r.Features, &photos); err != nil {
		return Record{}, err
	}
	if v, ok := new(big.Rat).SetString(priceText); ok {
		r.Price.Amount = FormatAmount(v)
	}
	if publishedAt != nil {
		r.PublishedAt = publishedAt.UTC()
	}
	if expiresAt != nil {
		r.ExpiresAt = expiresAt.UTC()
	}
	if err := json.Unmarshal([]byte(photos), &r.Photos); err != nil {
		return Record{}, fmt.Errorf("decode photos: %w", err)
	}
	switch {
	case agSlug != nil && agName != nil:
		r.Advertiser = Advertiser{Type: AdvertiserAgency, Name: *agName, Verified: agVerified != nil && *agVerified,
			Agency: &ProfileRef{Kind: "agencies", Slug: *agSlug, Name: *agName, Verified: agVerified != nil && *agVerified}}
		if atSlug != nil && atNm != nil {
			r.Advertiser.Agent = &ProfileRef{Kind: "agents", Slug: *atSlug, Name: *atNm, Verified: atVerified != nil && *atVerified}
		}
	default:
		r.Advertiser = Advertiser{Type: AdvertiserPrivateSeller, Name: sellerName}
	}
	return r, nil
}

func (s *PostgresStore) records(ctx context.Context, where string, args ...any) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+recordColumns+recordFrom+` WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRecord)
}

func idStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// PublishedByIDs implements Store.
func (s *PostgresStore) PublishedByIDs(ctx context.Context, ids []uuid.UUID, now time.Time) ([]Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.records(ctx, publicCondition+` AND l.id IN (SELECT unnest($2::text[])::uuid)`, now, idStrings(ids))
}

// ByIDs implements Store.
func (s *PostgresStore) ByIDs(ctx context.Context, ids []uuid.UUID) ([]Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.records(ctx, `l.slug IS NOT NULL AND l.id IN (SELECT unnest($1::text[])::uuid)`, idStrings(ids))
}

// AllPublished implements Store.
func (s *PostgresStore) AllPublished(ctx context.Context, now time.Time) ([]Record, error) {
	return s.records(ctx, publicCondition+` ORDER BY l.published_at DESC, l.id`, now)
}

// BySlug implements Store. It returns the record whatever its status; the
// Service decides between "found", "no longer available" and "not found".
func (s *PostgresStore) BySlug(ctx context.Context, slug string) (Record, error) {
	records, err := s.records(ctx, `l.slug = $1`, slug)
	if err != nil {
		return Record{}, err
	}
	if len(records) == 0 {
		return Record{}, ErrNotFound
	}
	return records[0], nil
}

// PublishedByOwner implements Store.
func (s *PostgresStore) PublishedByOwner(ctx context.Context, organisationID, agentUserID *uuid.UUID, now time.Time) ([]Record, error) {
	switch {
	case agentUserID != nil:
		return s.records(ctx, publicCondition+` AND l.agent_user_id = $2 ORDER BY l.published_at DESC, l.id`, now, *agentUserID)
	case organisationID != nil:
		return s.records(ctx, publicCondition+` AND l.organisation_id = $2 ORDER BY l.published_at DESC, l.id`, now, *organisationID)
	}
	return nil, nil
}

// PublishedSlugs implements Store.
func (s *PostgresStore) PublishedSlugs(ctx context.Context, now time.Time) ([]SlugEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT l.slug, l.updated_at FROM listings l WHERE `+publicCondition+` ORDER BY l.published_at DESC, l.id`, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (SlugEntry, error) {
		var e SlugEntry
		err := row.Scan(&e.Slug, &e.UpdatedAt)
		e.UpdatedAt = e.UpdatedAt.UTC()
		return e, err
	})
}

// searchFrom adds the labels that free-text search matches against.
const searchFrom = `
	FROM listings l
	LEFT JOIN reference_items sb ON sb.set_code = 'suburbs' AND sb.code = l.suburb_code
	LEFT JOIN reference_items ct ON ct.set_code = 'cities' AND ct.code = l.city_code
	LEFT JOIN reference_items pt ON pt.set_code = 'property_types' AND pt.code = l.property_type
	LEFT JOIN markets mk ON mk.id = l.market_id`

// regionColumn is a listing's region: the parent of its city in the
// reference data. The hierarchy has one source, so nothing is stored twice.
const regionColumn = `ct.parent_code`

// builder accumulates a WHERE clause and its arguments.
type builder struct {
	conds []string
	args  []any
}

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

func (b *builder) add(format string, v any) { b.conds = append(b.conds, fmt.Sprintf(format, b.arg(v))) }

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func likePattern(word string) string { return "%" + likeEscaper.Replace(word) + "%" }

// where builds the conditions of a query, leaving out the filter named by
// skip (used for that filter's facet). The public condition is always first.
func where(q Query, now time.Time, skip string) *builder {
	b := &builder{conds: []string{publicCondition}, args: []any{now}}
	for _, w := range terms(q.Q) {
		p := b.arg(likePattern(w))
		b.conds = append(b.conds, fmt.Sprintf(
			`(l.title ILIKE %[1]s OR sb.label ILIKE %[1]s OR ct.label ILIKE %[1]s OR pt.label ILIKE %[1]s OR l.summary ILIKE %[1]s OR l.description ILIKE %[1]s OR l.reference ILIKE %[1]s)`, p))
	}
	eq := func(name, column, value string) {
		if skip != name && value != "" {
			b.add(column+` = %s`, value)
		}
	}
	eq("deal", "l.deal", q.Deal)
	eq("category", "l.category", q.Category)
	eq("property_type", "l.property_type", q.PropertyType)
	eq("city", "l.city_code", q.City)
	eq("suburb", "l.suburb_code", q.Suburb)
	eq("country", "mk.code", q.Country)
	eq("region", regionColumn, q.Region)
	if skip != "bedrooms" && q.BedroomsMax != nil {
		b.add(`l.bedrooms <= %s`, *q.BedroomsMax)
	}
	for _, r := range []struct {
		column string
		min    *int
		max    *int
	}{
		{"l.bathrooms", nil, q.BathroomsMax},
		{"l.floor_area_m2", q.FloorAreaMin, q.FloorAreaMax},
		{"l.land_area_m2", q.LandAreaMin, q.LandAreaMax},
	} {
		if r.min != nil {
			b.add(r.column+` >= %s`, *r.min)
		}
		if r.max != nil {
			b.add(r.column+` <= %s`, *r.max)
		}
	}
	if q.ListedWithinDays != nil {
		b.add(`l.published_at >= %s`, listedSince(now, *q.ListedWithinDays))
	}
	switch q.AdvertiserType {
	case AdvertiserFilterAgency:
		b.conds = append(b.conds, `l.organisation_id IS NOT NULL`)
	case AdvertiserFilterPrivate:
		b.conds = append(b.conds, `l.organisation_id IS NULL`)
	}
	if q.Reference != "" {
		b.add(`upper(l.reference) = upper(%s)`, q.Reference)
	}
	if q.HasPhotos {
		b.conds = append(b.conds, `EXISTS (SELECT 1 FROM listing_photos lp WHERE lp.listing_id = l.id)`)
	}
	if q.Featured != nil {
		b.add(`l.featured = %s`, *q.Featured)
	}
	if q.BathroomsMin != nil {
		b.add(`l.bathrooms >= %s`, *q.BathroomsMin)
	}
	if skip != "bedrooms" && q.BedroomsMin != nil {
		b.add(`l.bedrooms >= %s`, *q.BedroomsMin)
	}
	for _, f := range q.Features {
		b.add(`EXISTS (SELECT 1 FROM listing_features lf WHERE lf.listing_id = l.id AND lf.feature_code = %s)`, f)
	}
	if skip != "price" && q.priced() {
		b.add(`l.currency_code = %s`, currencyOf(q))
		if q.MinPrice != nil {
			b.add(`l.price_amount >= %s::text::numeric`, q.MinPrice.FloatString(4))
		}
		if q.MaxPrice != nil {
			b.add(`l.price_amount <= %s::text::numeric`, q.MaxPrice.FloatString(4))
		}
	}
	return b
}

func (b *builder) sql() string { return strings.Join(b.conds, " AND ") }

func (s *PostgresStore) countBy(ctx context.Context, column string, b *builder) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+column+`, count(*) `+searchFrom+` WHERE `+b.sql()+` AND `+column+` IS NOT NULL GROUP BY 1`, b.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var code string
		var n int
		if err := rows.Scan(&code, &n); err != nil {
			return nil, err
		}
		out[code] = n
	}
	return out, rows.Err()
}

// Search implements Search.
func (s *PostgresStore) Search(ctx context.Context, q Query, now time.Time) (Hits, error) {
	all := where(q, now, "")
	hits := Hits{IDs: []uuid.UUID{}, Facets: RawFacets{Bedrooms: map[int]int{}}}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) `+searchFrom+` WHERE `+all.sql(), all.args...).Scan(&hits.Total); err != nil {
		return Hits{}, fmt.Errorf("count: %w", err)
	}

	// Order. Relevance scores each term: title, place, type, then the rest.
	order := `l.published_at DESC, l.id`
	page := where(q, now, "")
	switch q.Sort {
	case SortPriceAsc:
		order = `l.price_amount ASC, ` + order
	case SortPriceDesc:
		order = `l.price_amount DESC, ` + order
	case SortOldest:
		order = `l.published_at ASC, l.id`
	case SortAreaDesc:
		order = `l.floor_area_m2 DESC NULLS LAST, ` + order
	case SortRelevance:
		var parts []string
		for _, w := range terms(q.Q) {
			p := page.arg(likePattern(w))
			parts = append(parts, fmt.Sprintf(
				`(CASE WHEN l.title ILIKE %[1]s THEN 4 ELSE 0 END + CASE WHEN sb.label ILIKE %[1]s OR ct.label ILIKE %[1]s THEN 3 ELSE 0 END + CASE WHEN pt.label ILIKE %[1]s THEN 2 ELSE 0 END + CASE WHEN l.summary ILIKE %[1]s OR l.description ILIKE %[1]s OR l.reference ILIKE %[1]s THEN 1 ELSE 0 END)`, p))
		}
		if len(parts) > 0 {
			order = `(` + strings.Join(parts, " + ") + `) DESC, l.featured DESC, ` + order
		} else {
			order = `l.featured DESC, ` + order
		}
	}
	limit, offset := page.arg(q.PageSize), page.arg((q.Page-1)*q.PageSize)
	rows, err := s.pool.Query(ctx, `SELECT l.id `+searchFrom+` WHERE `+page.sql()+` ORDER BY `+order+` LIMIT `+limit+` OFFSET `+offset, page.args...)
	if err != nil {
		return Hits{}, fmt.Errorf("page: %w", err)
	}
	if hits.IDs, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
		return Hits{}, fmt.Errorf("page: %w", err)
	}
	if hits.IDs == nil {
		hits.IDs = []uuid.UUID{}
	}

	// Facets: each one leaves out its own filter.
	for _, f := range []struct {
		name, column string
		dst          *map[string]int
	}{
		{"deal", "l.deal", &hits.Facets.Deal}, {"category", "l.category", &hits.Facets.Category},
		{"property_type", "l.property_type", &hits.Facets.PropertyType},
		{"city", "l.city_code", &hits.Facets.City}, {"suburb", "l.suburb_code", &hits.Facets.Suburb},
		{"country", "mk.code", &hits.Facets.Country}, {"region", regionColumn, &hits.Facets.Region},
	} {
		if *f.dst, err = s.countBy(ctx, f.column, where(q, now, f.name)); err != nil {
			return Hits{}, fmt.Errorf("facet %s: %w", f.name, err)
		}
	}
	beds, err := s.countBy(ctx, "l.bedrooms::text", where(q, now, "bedrooms"))
	if err != nil {
		return Hits{}, fmt.Errorf("facet bedrooms: %w", err)
	}
	for code, n := range beds {
		var bedrooms int
		if _, err := fmt.Sscanf(code, "%d", &bedrooms); err == nil {
			hits.Facets.Bedrooms[bedrooms] = n
		}
	}
	featureRows, err := s.pool.Query(ctx, `SELECT lf2.feature_code, count(*) `+searchFrom+
		` JOIN listing_features lf2 ON lf2.listing_id = l.id WHERE `+all.sql()+` GROUP BY 1`, all.args...)
	if err != nil {
		return Hits{}, fmt.Errorf("facet features: %w", err)
	}
	hits.Facets.Features = map[string]int{}
	for featureRows.Next() {
		var code string
		var n int
		if err := featureRows.Scan(&code, &n); err != nil {
			featureRows.Close()
			return Hits{}, err
		}
		hits.Facets.Features[code] = n
	}
	featureRows.Close()
	if err := featureRows.Err(); err != nil {
		return Hits{}, err
	}

	price := where(q, now, "price")
	price.add(`l.currency_code = %s`, currencyOf(q))
	var lo, hi *string
	if err := s.pool.QueryRow(ctx, `SELECT min(l.price_amount)::text, max(l.price_amount)::text `+searchFrom+` WHERE `+price.sql(), price.args...).Scan(&lo, &hi); err != nil {
		return Hits{}, fmt.Errorf("facet price: %w", err)
	}
	if lo != nil && hi != nil {
		hits.Facets.PriceMin, _ = new(big.Rat).SetString(*lo)
		hits.Facets.PriceMax, _ = new(big.Rat).SetString(*hi)
	}
	return hits, nil
}

// Suggest implements Search.
func (s *PostgresStore) Suggest(ctx context.Context, term string, limit int, now time.Time) ([]Suggestion, error) {
	pattern, prefix := likePattern(strings.TrimSpace(term)), likeEscaper.Replace(strings.TrimSpace(term))+"%"
	out := []Suggestion{}
	for _, query := range []string{
		`SELECT DISTINCT 'suburb', sb.label || ', ' || ct.label, l.suburb_code, sb.label ILIKE $3 ` + searchFrom + `
		 WHERE ` + publicCondition + ` AND sb.label ILIKE $2 ORDER BY 4 DESC, 2 LIMIT $4`,
		`SELECT DISTINCT 'city', ct.label, l.city_code, ct.label ILIKE $3 ` + searchFrom + `
		 WHERE ` + publicCondition + ` AND ct.label ILIKE $2 ORDER BY 4 DESC, 2 LIMIT $4`,
		`SELECT 'listing', l.title, l.slug, l.title ILIKE $3 ` + searchFrom + `
		 WHERE ` + publicCondition + ` AND l.title ILIKE $2 ORDER BY 4 DESC, l.published_at DESC, l.id LIMIT $4`,
	} {
		rows, err := s.pool.Query(ctx, query, now, pattern, prefix, limit)
		if err != nil {
			return nil, err
		}
		found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Suggestion, error) {
			var sg Suggestion
			var startsWith bool
			err := row.Scan(&sg.Type, &sg.Label, &sg.Value, &startsWith)
			return sg, err
		})
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
