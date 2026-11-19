package developments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// PostgresStore is the production Store (migration 0046). A project is
// written in one transaction with its children; child rows are upserted by
// ID and the ones that went are deleted, so an enquiry's reference to a
// unit type survives an unrelated edit.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const (
	uniqueViolation = "23505"
	slugConstraint  = "developments_market_slug_unique"
	linkPrimaryKey  = "development_listings_pkey"
	developmentCols = `d.id, d.organisation_id, d.profile_id, d.market_code, d.slug, d.name, d.summary, d.description, d.status,
		d.moderation_status, COALESCE(d.completion_estimate, ''), COALESCE(d.city_code, ''), COALESCE(d.suburb_code, ''),
		COALESCE(d.city_name, ''), d.latitude::float8, d.longitude::float8, d.location_precision, d.amenities, d.highlights,
		COALESCE(d.youtube_video_id, ''), d.youtube_start_seconds, COALESCE(d.review_reason, ''), d.submitted_at, d.submitted_by,
		d.reviewed_at, d.reviewed_by, d.published_at, d.created_by, d.created_at, d.updated_at`
	// publicJoin keeps the projects whose developer profile is public and
	// whose organisation is ACTIVE.
	publicJoin = ` FROM developments d
		JOIN directory_profiles p ON p.id = d.profile_id AND p.status = 'ACTIVE' AND p.verified
		JOIN organisations o ON o.id = d.organisation_id AND o.status = 'ACTIVE'
		WHERE d.moderation_status = 'PUBLISHED'`
)

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func scanDevelopment(row pgx.CollectableRow) (Development, error) {
	var d Development
	var highlights []byte
	err := row.Scan(&d.ID, &d.OrganisationID, &d.ProfileID, &d.MarketCode, &d.Slug, &d.Name, &d.Summary, &d.Description, &d.Status,
		&d.Moderation, &d.Completion, &d.CityCode, &d.SuburbCode, &d.CityName, &d.Latitude, &d.Longitude, &d.LocationPrecision,
		&d.Amenities, &highlights, &d.YouTubeVideoID, &d.YouTubeStartSec, &d.ReviewReason, &d.SubmittedAt, &d.SubmittedBy,
		&d.ReviewedAt, &d.ReviewedBy, &d.PublishedAt, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return Development{}, err
	}
	if len(highlights) > 0 {
		if err := json.Unmarshal(highlights, &d.Highlights); err != nil {
			return Development{}, fmt.Errorf("decode highlights: %w", err)
		}
	}
	if d.Highlights == nil {
		d.Highlights = []Highlight{}
	}
	if d.Amenities == nil {
		d.Amenities = []string{}
	}
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	d.SubmittedAt, d.ReviewedAt, d.PublishedAt = utc(d.SubmittedAt), utc(d.ReviewedAt), utc(d.PublishedAt)
	return d, nil
}

// fill loads the children of the projects.
func fill(ctx context.Context, q querier, rows []Development) ([]Development, error) {
	if len(rows) == 0 {
		return []Development{}, nil
	}
	ids := make([]uuid.UUID, len(rows))
	index := map[uuid.UUID]int{}
	for i := range rows {
		ids[i], index[rows[i].ID] = rows[i].ID, i
		rows[i].Phases, rows[i].Photos, rows[i].Links = []Phase{}, []listings.StoredPhoto{}, []Link{}
	}
	phaseRows, err := q.Query(ctx, `
		SELECT id, development_id, name, status, COALESCE(completion_estimate, ''), position, created_at
		FROM development_phases WHERE development_id = ANY($1::uuid[]) ORDER BY position, created_at, id`, idStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("load phases: %w", err)
	}
	phaseOf := map[uuid.UUID][2]int{}
	_, err = pgx.CollectRows(phaseRows, func(row pgx.CollectableRow) (struct{}, error) {
		var ph Phase
		var dev uuid.UUID
		if err := row.Scan(&ph.ID, &dev, &ph.Name, &ph.Status, &ph.Completion, &ph.Position, &ph.CreatedAt); err != nil {
			return struct{}{}, err
		}
		ph.CreatedAt, ph.UnitTypes = ph.CreatedAt.UTC(), []UnitType{}
		i := index[dev]
		rows[i].Phases = append(rows[i].Phases, ph)
		phaseOf[ph.ID] = [2]int{i, len(rows[i].Phases) - 1}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("load phases: %w", err)
	}
	unitRows, err := q.Query(ctx, `
		SELECT id, phase_id, name, bedrooms, bathrooms, size_min_m2, size_max_m2, price_on_request,
		       COALESCE(price_from::text, ''), COALESCE(price_to::text, ''), COALESCE(currency_code, ''),
		       units_total, units_available, position, created_at
		FROM development_unit_types WHERE development_id = ANY($1::uuid[]) ORDER BY position, created_at, id`, idStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("load unit types: %w", err)
	}
	_, err = pgx.CollectRows(unitRows, func(row pgx.CollectableRow) (struct{}, error) {
		var u UnitType
		if err := row.Scan(&u.ID, &u.PhaseID, &u.Name, &u.Bedrooms, &u.Bathrooms, &u.SizeMin, &u.SizeMax, &u.PriceOnRequest,
			&u.PriceFrom, &u.PriceTo, &u.Currency, &u.UnitsTotal, &u.UnitsAvailable, &u.Position, &u.CreatedAt); err != nil {
			return struct{}{}, err
		}
		u.CreatedAt = u.CreatedAt.UTC()
		u.PriceFrom, u.PriceTo = normalAmount(u.PriceFrom), normalAmount(u.PriceTo)
		at, ok := phaseOf[u.PhaseID]
		if ok {
			rows[at[0]].Phases[at[1]].UnitTypes = append(rows[at[0]].Phases[at[1]].UnitTypes, u)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("load unit types: %w", err)
	}
	photoRows, err := q.Query(ctx, `
		SELECT id, development_id, position, COALESCE(url, ''), alt, width, height, COALESCE(checksum, ''), variants, approved_at, uploaded_by, created_at
		FROM development_photos WHERE development_id = ANY($1::uuid[]) ORDER BY position, created_at, id`, idStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("load photos: %w", err)
	}
	_, err = pgx.CollectRows(photoRows, func(row pgx.CollectableRow) (struct{}, error) {
		var ph listings.StoredPhoto
		var dev uuid.UUID
		var variants []byte
		if err := row.Scan(&ph.ID, &dev, &ph.Position, &ph.URL, &ph.Alt, &ph.Width, &ph.Height, &ph.Checksum, &variants,
			&ph.ApprovedAt, &ph.UploadedBy, &ph.CreatedAt); err != nil {
			return struct{}{}, err
		}
		if len(variants) > 0 {
			if err := json.Unmarshal(variants, &ph.Variants); err != nil {
				return struct{}{}, err
			}
		}
		ph.CreatedAt, ph.ApprovedAt = ph.CreatedAt.UTC(), utc(ph.ApprovedAt)
		i := index[dev]
		rows[i].Photos = append(rows[i].Photos, ph)
		return struct{}{}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("load photos: %w", err)
	}
	linkRows, err := q.Query(ctx, `
		SELECT listing_id, development_id, unit_type_id, created_at
		FROM development_listings WHERE development_id = ANY($1::uuid[]) ORDER BY created_at, listing_id`, idStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("load listing links: %w", err)
	}
	_, err = pgx.CollectRows(linkRows, func(row pgx.CollectableRow) (struct{}, error) {
		var l Link
		var dev uuid.UUID
		if err := row.Scan(&l.ListingID, &dev, &l.UnitTypeID, &l.CreatedAt); err != nil {
			return struct{}{}, err
		}
		l.CreatedAt = l.CreatedAt.UTC()
		i := index[dev]
		rows[i].Links = append(rows[i].Links, l)
		return struct{}{}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("load listing links: %w", err)
	}
	return rows, nil
}

// normalAmount renders a numeric(20,4) as the API's two-decimal string.
func normalAmount(s string) string {
	if s == "" {
		return ""
	}
	r, err := listings.ParseAmount(s)
	if err != nil {
		return s
	}
	return listings.FormatAmount(r)
}

func (s *PostgresStore) query(ctx context.Context, q querier, sql string, args ...any) ([]Development, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, scanDevelopment)
	if err != nil {
		return nil, err
	}
	return fill(ctx, q, out)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		switch pgErr.ConstraintName {
		case slugConstraint:
			return ErrSlugTaken
		case linkPrimaryKey:
			return ErrListingLinked
		}
	}
	return err
}

// writeRow inserts or updates the project row itself.
func writeRow(ctx context.Context, tx pgx.Tx, d Development, insert bool, expect time.Time) (bool, error) {
	highlights, err := json.Marshal(d.Highlights)
	if err != nil {
		return false, err
	}
	if d.Highlights == nil {
		highlights = []byte("[]")
	}
	amenities := d.Amenities
	if amenities == nil {
		amenities = []string{}
	}
	args := []any{d.ID, d.OrganisationID, d.ProfileID, d.MarketCode, d.Slug, d.Name, d.Summary, d.Description, d.Status, d.Moderation,
		nullable(d.Completion), nullable(d.CityCode), nullable(d.SuburbCode), nullable(d.CityName), d.Latitude, d.Longitude,
		listings.NormalPrecision(d.LocationPrecision), amenities, string(highlights), nullable(d.YouTubeVideoID), d.YouTubeStartSec,
		nullable(d.ReviewReason), d.SubmittedAt, d.SubmittedBy, d.ReviewedAt, d.ReviewedBy, d.PublishedAt, d.CreatedBy, d.CreatedAt, d.UpdatedAt}
	if insert {
		_, err := tx.Exec(ctx, `
			INSERT INTO developments (id, organisation_id, profile_id, market_code, slug, name, summary, description, status, moderation_status,
				completion_estimate, city_code, suburb_code, city_name, latitude, longitude, location_precision, amenities, highlights,
				youtube_video_id, youtube_start_seconds, review_reason, submitted_at, submitted_by, reviewed_at, reviewed_by, published_at,
				created_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19::jsonb, $20, $21, $22, $23, $24,
				$25, $26, $27, $28, $29, $30)`, args...)
		return err == nil, mapErr(err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE developments SET organisation_id = $2, profile_id = $3, market_code = $4, slug = $5, name = $6, summary = $7,
			description = $8, status = $9, moderation_status = $10, completion_estimate = $11, city_code = $12, suburb_code = $13,
			city_name = $14, latitude = $15, longitude = $16, location_precision = $17, amenities = $18, highlights = $19::jsonb,
			youtube_video_id = $20, youtube_start_seconds = $21, review_reason = $22, submitted_at = $23, submitted_by = $24,
			reviewed_at = $25, reviewed_by = $26, published_at = $27, created_by = $28, created_at = $29, updated_at = $30
		WHERE id = $1 AND updated_at = $31`, append(args, expect)...)
	if err != nil {
		return false, mapErr(err)
	}
	return tag.RowsAffected() == 1, nil
}

// writeChildren upserts the phases, unit types, photos and links of d and
// deletes the ones that went. It returns the photos deleted.
func writeChildren(ctx context.Context, tx pgx.Tx, d Development) ([]listings.StoredPhoto, error) {
	var phaseIDs, unitIDs, photoIDs, linkIDs []uuid.UUID
	for _, ph := range d.Phases {
		phaseIDs = append(phaseIDs, ph.ID)
		if _, err := tx.Exec(ctx, `
			INSERT INTO development_phases (id, development_id, name, status, completion_estimate, position, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, status = EXCLUDED.status,
				completion_estimate = EXCLUDED.completion_estimate, position = EXCLUDED.position
			WHERE development_phases.development_id = EXCLUDED.development_id`,
			ph.ID, d.ID, ph.Name, ph.Status, nullable(ph.Completion), ph.Position, ph.CreatedAt); err != nil {
			return nil, fmt.Errorf("save phase: %w", err)
		}
	}
	for _, ph := range d.Phases {
		for _, u := range ph.UnitTypes {
			unitIDs = append(unitIDs, u.ID)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM development_unit_types WHERE development_id = $1 AND id <> ALL($2::uuid[])`, d.ID, nonNil(unitIDs)); err != nil {
		return nil, fmt.Errorf("delete unit types: %w", err)
	}
	for _, ph := range d.Phases {
		for _, u := range ph.UnitTypes {
			if _, err := tx.Exec(ctx, `
				INSERT INTO development_unit_types (id, development_id, phase_id, name, bedrooms, bathrooms, size_min_m2, size_max_m2,
					price_on_request, price_from, price_to, currency_code, units_total, units_available, position, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::numeric, $11::numeric, $12, $13, $14, $15, $16)
				ON CONFLICT (id) DO UPDATE SET phase_id = EXCLUDED.phase_id, name = EXCLUDED.name, bedrooms = EXCLUDED.bedrooms,
					bathrooms = EXCLUDED.bathrooms, size_min_m2 = EXCLUDED.size_min_m2, size_max_m2 = EXCLUDED.size_max_m2,
					price_on_request = EXCLUDED.price_on_request, price_from = EXCLUDED.price_from, price_to = EXCLUDED.price_to,
					currency_code = EXCLUDED.currency_code, units_total = EXCLUDED.units_total,
					units_available = EXCLUDED.units_available, position = EXCLUDED.position
				WHERE development_unit_types.development_id = EXCLUDED.development_id`,
				u.ID, d.ID, ph.ID, u.Name, u.Bedrooms, u.Bathrooms, u.SizeMin, u.SizeMax, u.PriceOnRequest, nullable(u.PriceFrom),
				nullable(u.PriceTo), nullable(u.Currency), u.UnitsTotal, u.UnitsAvailable, u.Position, u.CreatedAt); err != nil {
				return nil, fmt.Errorf("save unit type: %w", err)
			}
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM development_phases WHERE development_id = $1 AND id <> ALL($2::uuid[])`, d.ID, nonNil(phaseIDs)); err != nil {
		return nil, fmt.Errorf("delete phases: %w", err)
	}

	for _, ph := range d.Photos {
		photoIDs = append(photoIDs, ph.ID)
	}
	rows, err := tx.Query(ctx, `
		DELETE FROM development_photos WHERE development_id = $1 AND id <> ALL($2::uuid[])
		RETURNING id, COALESCE(url, ''), COALESCE(checksum, ''), variants`, d.ID, nonNil(photoIDs))
	if err != nil {
		return nil, fmt.Errorf("delete photos: %w", err)
	}
	removed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (listings.StoredPhoto, error) {
		var ph listings.StoredPhoto
		var variants []byte
		if err := row.Scan(&ph.ID, &ph.URL, &ph.Checksum, &variants); err != nil {
			return ph, err
		}
		if len(variants) > 0 {
			if err := json.Unmarshal(variants, &ph.Variants); err != nil {
				return ph, err
			}
		}
		return ph, nil
	})
	if err != nil {
		return nil, fmt.Errorf("delete photos: %w", err)
	}
	for _, ph := range d.Photos {
		var url, checksum, variants *string
		if ph.Uploaded() {
			raw, err := json.Marshal(ph.Variants)
			if err != nil {
				return nil, err
			}
			v := string(raw)
			variants, checksum = &v, &ph.Checksum
		} else {
			url = &ph.URL
			if ph.Checksum != "" {
				checksum = &ph.Checksum
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO development_photos (id, development_id, position, url, alt, width, height, checksum, variants, approved_at, uploaded_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12)
			ON CONFLICT (id) DO UPDATE SET position = EXCLUDED.position, alt = EXCLUDED.alt, approved_at = EXCLUDED.approved_at,
				uploaded_by = EXCLUDED.uploaded_by
			WHERE development_photos.development_id = EXCLUDED.development_id`,
			ph.ID, d.ID, ph.Position, url, ph.Alt, ph.Width, ph.Height, checksum, variants, ph.ApprovedAt, ph.UploadedBy, ph.CreatedAt); err != nil {
			return nil, fmt.Errorf("save photo: %w", err)
		}
	}

	for _, l := range d.Links {
		linkIDs = append(linkIDs, l.ListingID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM development_listings WHERE development_id = $1 AND listing_id <> ALL($2::uuid[])`, d.ID, nonNil(linkIDs)); err != nil {
		return nil, fmt.Errorf("delete listing links: %w", err)
	}
	for _, l := range d.Links {
		tag, err := tx.Exec(ctx, `
			INSERT INTO development_listings (listing_id, development_id, unit_type_id, created_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (listing_id) DO UPDATE SET unit_type_id = EXCLUDED.unit_type_id
			WHERE development_listings.development_id = EXCLUDED.development_id`,
			l.ListingID, d.ID, l.UnitTypeID, l.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("save listing link: %w", mapErr(err))
		}
		if tag.RowsAffected() == 0 {
			return nil, ErrListingLinked
		}
	}
	return removed, nil
}

// nonNil is the text array of ids (cast to uuid[] in SQL), never NULL.
func nonNil(ids []uuid.UUID) []string { return idStrings(ids) }

func idStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func (s *PostgresStore) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Create implements Store.
func (s *PostgresStore) Create(ctx context.Context, d Development) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := writeRow(ctx, tx, d, true, time.Time{}); err != nil {
			return err
		}
		_, err := writeChildren(ctx, tx, d)
		return err
	})
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Development, error) {
	out, err := s.query(ctx, s.pool, `SELECT `+developmentCols+` FROM developments d WHERE d.id = $1`, id)
	if err != nil {
		return Development{}, err
	}
	if len(out) == 0 {
		return Development{}, ErrNotFound
	}
	return out[0], nil
}

// Save implements Store.
func (s *PostgresStore) Save(ctx context.Context, d Development, expect time.Time, audit *adminaudit.Event) ([]listings.StoredPhoto, error) {
	var removed []listings.StoredPhoto
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		ok, err := writeRow(ctx, tx, d, false, expect)
		if err != nil {
			return err
		}
		if !ok {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM developments WHERE id = $1)`, d.ID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return ErrConflict
			}
			return ErrNotFound
		}
		if removed, err = writeChildren(ctx, tx, d); err != nil {
			return err
		}
		if audit != nil {
			return adminaudit.Insert(ctx, tx, *audit)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// Delete implements Store.
func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID, expect time.Time) ([]listings.StoredPhoto, error) {
	var photos []listings.StoredPhoto
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		found, err := s.query(ctx, tx, `SELECT `+developmentCols+` FROM developments d WHERE d.id = $1 FOR UPDATE`, id)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			return ErrNotFound
		}
		if !found[0].UpdatedAt.Equal(expect.UTC()) {
			return ErrConflict
		}
		photos = found[0].Photos
		_, err = tx.Exec(ctx, `DELETE FROM developments WHERE id = $1`, id)
		return err
	})
	return photos, err
}

// ByOrganisation implements Store.
func (s *PostgresStore) ByOrganisation(ctx context.Context, organisationID uuid.UUID) ([]Development, error) {
	return s.query(ctx, s.pool, `SELECT `+developmentCols+` FROM developments d WHERE d.organisation_id = $1
		ORDER BY d.updated_at DESC, d.id DESC LIMIT 500`, organisationID)
}

func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// Public implements Store.
func (s *PostgresStore) Public(ctx context.Context, f PublicFilter) ([]Development, int, error) {
	where := publicJoin + `
		AND ($1 = '' OR d.market_code = $1) AND ($2 = '' OR d.city_code = $2) AND ($3 = '' OR d.status = $3)
		AND ($4 = '' OR d.name ILIKE $5 OR d.summary ILIKE $5 OR d.description ILIKE $5 OR d.city_name ILIKE $5
		     OR d.city_code = ANY($6::text[]) OR d.suburb_code = ANY($6::text[]))`
	places := f.QPlaces
	if places == nil {
		places = []string{}
	}
	args := []any{f.Market, f.City, f.Status, f.Q, likePattern(f.Q), places}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count developments: %w", err)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	out, err := s.query(ctx, s.pool, `SELECT `+developmentCols+where+`
		ORDER BY d.published_at DESC, d.id DESC LIMIT $7 OFFSET $8`, append(args, limit, max(f.Offset, 0))...)
	if err != nil {
		return nil, 0, fmt.Errorf("list developments: %w", err)
	}
	return out, total, nil
}

// PublicBySlug implements Store.
func (s *PostgresStore) PublicBySlug(ctx context.Context, market, slug string) (Development, error) {
	out, err := s.query(ctx, s.pool, `SELECT `+developmentCols+publicJoin+` AND d.market_code = $1 AND d.slug = $2`, market, slug)
	if err != nil {
		return Development{}, err
	}
	if len(out) == 0 {
		return Development{}, ErrNotFound
	}
	return out[0], nil
}

// PublicByProfile implements Store.
func (s *PostgresStore) PublicByProfile(ctx context.Context, profileID uuid.UUID) ([]Development, error) {
	return s.query(ctx, s.pool, `SELECT `+developmentCols+publicJoin+` AND d.profile_id = $1
		ORDER BY d.published_at DESC, d.id DESC LIMIT 200`, profileID)
}

// PublicEntries implements Store.
func (s *PostgresStore) PublicEntries(ctx context.Context) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `SELECT d.market_code, d.slug, d.updated_at`+publicJoin+` ORDER BY d.market_code, d.slug`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := row.Scan(&e.Market, &e.Slug, &e.UpdatedAt)
		e.UpdatedAt = e.UpdatedAt.UTC()
		return e, err
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []Entry{}
	}
	return out, nil
}

// Queue implements Store.
func (s *PostgresStore) Queue(ctx context.Context, f QueueFilter) ([]Development, int, error) {
	status := f.Moderation
	if status == "" {
		status = ModerationPending
	}
	all := f.Markets == nil
	markets := f.Markets
	if markets == nil {
		markets = []string{}
	}
	where := ` FROM developments d WHERE d.moderation_status = $1 AND ($2 OR d.market_code = ANY($3::text[])) AND ($4 = '' OR d.market_code = $4)`
	args := []any{status, all, markets, f.Market}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count review queue: %w", err)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	out, err := s.query(ctx, s.pool, `SELECT `+developmentCols+where+`
		ORDER BY d.submitted_at NULLS LAST, d.id LIMIT $5 OFFSET $6`, append(args, limit, max(f.Offset, 0))...)
	if err != nil {
		return nil, 0, fmt.Errorf("list review queue: %w", err)
	}
	return out, total, nil
}

// PhotoByID implements Store.
func (s *PostgresStore) PhotoByID(ctx context.Context, photoID uuid.UUID) (uuid.UUID, listings.StoredPhoto, error) {
	var dev uuid.UUID
	var ph listings.StoredPhoto
	var variants []byte
	err := s.pool.QueryRow(ctx, `
		SELECT development_id, id, position, COALESCE(url, ''), alt, width, height, COALESCE(checksum, ''), variants, approved_at, uploaded_by, created_at
		FROM development_photos WHERE id = $1`, photoID).Scan(&dev, &ph.ID, &ph.Position, &ph.URL, &ph.Alt, &ph.Width, &ph.Height,
		&ph.Checksum, &variants, &ph.ApprovedAt, &ph.UploadedBy, &ph.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, listings.StoredPhoto{}, listings.ErrPhotoNotFound
	}
	if err != nil {
		return uuid.Nil, listings.StoredPhoto{}, err
	}
	if len(variants) > 0 {
		if err := json.Unmarshal(variants, &ph.Variants); err != nil {
			return uuid.Nil, listings.StoredPhoto{}, err
		}
	}
	return dev, ph, nil
}

// Execer is a pgx transaction (account deletion runs in one).
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ForgetUserTx unlinks a deleted account from every project inside the
// account deletion's transaction: the users row is anonymised, not deleted,
// so ON DELETE SET NULL never fires. The projects stay with their
// organisation.
func ForgetUserTx(ctx context.Context, tx Execer, userID uuid.UUID) error {
	for _, sql := range []string{
		`UPDATE developments SET created_by = NULL WHERE created_by = $1`,
		`UPDATE developments SET submitted_by = NULL WHERE submitted_by = $1`,
		`UPDATE developments SET reviewed_by = NULL WHERE reviewed_by = $1`,
		`UPDATE development_photos SET uploaded_by = NULL WHERE uploaded_by = $1`,
	} {
		if _, err := tx.Exec(ctx, sql, userID); err != nil {
			return fmt.Errorf("unlink user from developments: %w", err)
		}
	}
	return nil
}
