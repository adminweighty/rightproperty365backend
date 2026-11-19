package listings

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

	"rightproperty365.com/backend/internal/modules/packages"
)

// The Postgres side of listing management (migration 0020).
//
// NOTE: compiled but not yet exercised against a real database.

// photoRow is a photo as the record query aggregates it.
type photoRow struct {
	ID       uuid.UUID       `json:"id"`
	URL      *string         `json:"url"`
	Alt      string          `json:"alt"`
	Width    int             `json:"width"`
	Height   int             `json:"height"`
	Checksum *string         `json:"checksum"`
	Variants []StoredVariant `json:"variants"`
}

// decodePublicPhotos turns the aggregated approved photos of a record into
// their public form. Object keys are read but never leave this function.
func decodePublicPhotos(raw string) ([]Photo, error) {
	var rows []photoRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, fmt.Errorf("decode photos: %w", err)
	}
	stored := make([]StoredPhoto, 0, len(rows))
	approved := time.Unix(0, 0)
	for i, r := range rows {
		p := StoredPhoto{ID: r.ID, Position: i, Alt: r.Alt, Width: r.Width, Height: r.Height, Variants: r.Variants, ApprovedAt: &approved}
		if r.URL != nil {
			p.URL = *r.URL
		}
		if r.Checksum != nil {
			p.Checksum = *r.Checksum
		}
		stored = append(stored, p)
	}
	return PublicPhotos(stored), nil
}

func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

const insertOutbox = `
	INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
	VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`

// writeChange records the change's timeline entry and outbox events in tx.
func writeChange(ctx context.Context, tx pgx.Tx, id uuid.UUID, ch Change, now time.Time) error {
	if e := ch.Entry; e != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO listing_timeline (id, listing_id, actor_user_id, actor_kind, action, from_status, to_status, review_kind, reason, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			e.ID, id, e.ActorUserID, e.ActorKind, e.Action, nullText(e.FromStatus), nullText(e.ToStatus), nullText(e.ReviewKind),
			nullText(e.Reason), e.CreatedAt); err != nil {
			return fmt.Errorf("listing timeline: %w", err)
		}
	}
	for _, ev := range ch.Events {
		if _, err := tx.Exec(ctx, insertOutbox, uuid.New(), ev.Type, AggregateListing, id, string(ev.Payload), now); err != nil {
			return fmt.Errorf("listing event %s: %w", ev.Type, err)
		}
	}
	return nil
}

func duplicate(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// listingArgs are the values of the content columns, in the order of
// contentColumns.
const contentColumns = `title, price_amount, currency_code, deal, category, property_type, price_period, city_code, suburb_code,
	bedrooms, bathrooms, floor_area_m2, land_area_m2, summary, description, max_guests, min_nights, city_name, region_name,
	youtube_video_id, youtube_start_seconds, latitude, longitude, location_precision`

func contentArgs(r Record) []any {
	return []any{r.Title, nullText(r.Price.Amount), r.Price.Currency, nullText(r.Deal), nullText(r.Category), nullText(r.PropertyType),
		nullText(r.PricePeriod), nullText(r.City), nullText(r.Suburb), r.Bedrooms, r.Bathrooms, r.FloorArea, r.LandArea,
		nullText(r.Summary), nullText(r.Description), r.MaxGuests, r.MinNights, nullText(r.CityName), nullText(r.RegionName),
		nullText(r.YouTubeVideoID), r.YouTubeStartSeconds, r.Latitude, r.Longitude, NormalPrecision(r.LocationPrecision)}
}

func revisionJSON(r *Revision) (*string, *string, error) {
	if r == nil {
		return nil, nil, nil
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, nil, err
	}
	s, status := string(raw), r.Status
	return &s, &status, nil
}

func writeFeatures(ctx context.Context, tx pgx.Tx, id uuid.UUID, features []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM listing_features WHERE listing_id = $1`, id); err != nil {
		return fmt.Errorf("listing features: %w", err)
	}
	for i, f := range features {
		if _, err := tx.Exec(ctx, `INSERT INTO listing_features (listing_id, feature_code, position) VALUES ($1, $2, $3)`, id, f, i); err != nil {
			return fmt.Errorf("listing feature %s: %w", f, err)
		}
	}
	return nil
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// CreateListing implements ManageStore.
func (s *PostgresStore) CreateListing(ctx context.Context, m ManagedListing, ch Change) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	revision, revisionStatus, err := revisionJSON(m.Revision)
	if err != nil {
		return err
	}
	args := append([]any{m.ID, m.MarketCode, m.Status, m.OrganisationID, m.AgentUserID, m.SellerUserID, m.CreatedAt, m.UpdatedAt,
		m.Slug, m.Reference, m.CreatedBy, revision, revisionStatus, timeOrNil(m.PublishedAt), timeOrNil(m.ExpiresAt)}, contentArgs(m.Record)...)
	if _, err := tx.Exec(ctx, `
		INSERT INTO listings (id, market_id, status, organisation_id, agent_user_id, seller_user_id, created_at, updated_at,
		                      slug, reference, created_by, revision, revision_status, published_at, expires_at, featured, `+contentColumns+`)
		VALUES ($1, (SELECT id FROM markets WHERE code = $2), $3::text::listing_status, $4, $5, $6, $7, $8,
		        $9, $10, $11, $12::jsonb, $13, $14, $15, false,
		        $16, $17::text::numeric, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39)`,
		args...); err != nil {
		if duplicate(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("create listing: %w", err)
	}
	if err := writeFeatures(ctx, tx, m.ID, m.Features); err != nil {
		return err
	}
	for _, p := range m.AllPhotos {
		if err := insertPhoto(ctx, tx, m.ID, p); err != nil {
			return err
		}
	}
	if err := writeChange(ctx, tx, m.ID, ch, m.UpdatedAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertPhoto(ctx context.Context, tx pgx.Tx, listingID uuid.UUID, p StoredPhoto) error {
	var variants *string
	if p.Uploaded() {
		raw, err := json.Marshal(p.Variants)
		if err != nil {
			return err
		}
		s := string(raw)
		variants = &s
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO listing_photos (id, listing_id, position, url, alt, width, height, checksum, variants, approved_at, uploaded_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12)`,
		p.ID, listingID, p.Position, nullText(p.URL), p.Alt, p.Width, p.Height, nullText(p.Checksum), variants, p.ApprovedAt, p.UploadedBy, p.CreatedAt); err != nil {
		return fmt.Errorf("insert photo: %w", err)
	}
	return nil
}

// photoColumns are the columns scanPhoto reads.
const photoColumns = `id, listing_id, position, COALESCE(url, ''), alt, width, height, COALESCE(checksum, ''), COALESCE(variants, '[]'::jsonb)::text,
	approved_at, uploaded_by, created_at`

func scanPhoto(row pgx.CollectableRow) (struct {
	ListingID uuid.UUID
	Photo     StoredPhoto
}, error) {
	var out struct {
		ListingID uuid.UUID
		Photo     StoredPhoto
	}
	var variants string
	p := &out.Photo
	if err := row.Scan(&p.ID, &out.ListingID, &p.Position, &p.URL, &p.Alt, &p.Width, &p.Height, &p.Checksum, &variants,
		&p.ApprovedAt, &p.UploadedBy, &p.CreatedAt); err != nil {
		return out, err
	}
	if err := json.Unmarshal([]byte(variants), &p.Variants); err != nil {
		return out, fmt.Errorf("decode photo variants: %w", err)
	}
	if !p.Uploaded() && len(p.Variants) == 0 {
		p.Variants = nil
	}
	return out, nil
}

// photosOf returns every photo of the listings, in order.
func photosOf(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, ids []uuid.UUID) (map[uuid.UUID][]StoredPhoto, error) {
	out := map[uuid.UUID][]StoredPhoto{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT `+photoColumns+` FROM listing_photos WHERE listing_id = ANY($1::uuid[]) ORDER BY listing_id, position, created_at`, idStrings(ids))
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, scanPhoto)
	if err != nil {
		return nil, err
	}
	for _, f := range found {
		out[f.ListingID] = append(out[f.ListingID], f.Photo)
	}
	return out, nil
}

// managed completes records with their management fields and photos.
func (s *PostgresStore) managed(ctx context.Context, records []Record) ([]ManagedListing, error) {
	ids := make([]uuid.UUID, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	photos, err := photosOf(ctx, s.pool, ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, created_at, created_by, submitted_at, submitted_by, COALESCE(review_reason, ''), revision::text
		FROM listings WHERE id = ANY($1::uuid[])`, idStrings(ids))
	if err != nil {
		return nil, err
	}
	type meta struct {
		createdAt              time.Time
		createdBy, submittedBy *uuid.UUID
		submittedAt            *time.Time
		reason                 string
		revision               *string
	}
	metas := map[uuid.UUID]meta{}
	for rows.Next() {
		var id uuid.UUID
		var m meta
		if err := rows.Scan(&id, &m.createdAt, &m.createdBy, &m.submittedAt, &m.submittedBy, &m.reason, &m.revision); err != nil {
			rows.Close()
			return nil, err
		}
		metas[id] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ManagedListing, 0, len(records))
	for _, r := range records {
		m := metas[r.ID]
		ml := ManagedListing{Record: r, CreatedAt: m.createdAt.UTC(), CreatedBy: m.createdBy, SubmittedAt: utcTime(m.submittedAt),
			SubmittedBy: m.submittedBy, ReviewReason: m.reason, AllPhotos: photos[r.ID]}
		if m.revision != nil {
			var rev Revision
			if err := json.Unmarshal([]byte(*m.revision), &rev); err != nil {
				return nil, fmt.Errorf("decode revision: %w", err)
			}
			ml.Revision = &rev
		}
		out = append(out, ml)
	}
	return out, nil
}

// Managed implements ManageStore.
func (s *PostgresStore) Managed(ctx context.Context, id uuid.UUID) (ManagedListing, error) {
	records, err := s.records(ctx, `l.id = $1`, id)
	if err != nil {
		return ManagedListing{}, err
	}
	if len(records) == 0 {
		return ManagedListing{}, ErrNotFound
	}
	out, err := s.managed(ctx, records)
	if err != nil {
		return ManagedListing{}, err
	}
	return out[0], nil
}

// lockListing locks the listing row and checks its version.
func lockListing(ctx context.Context, tx pgx.Tx, id uuid.UUID, expect time.Time) error {
	var updated time.Time
	err := tx.QueryRow(ctx, `SELECT updated_at FROM listings WHERE id = $1 FOR UPDATE`, id).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !updated.Equal(expect) {
		return ErrConflict
	}
	return nil
}

// SaveListing implements ManageStore.
func (s *PostgresStore) SaveListing(ctx context.Context, m ManagedListing, expect time.Time, ch Change) ([]StoredPhoto, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockListing(ctx, tx, m.ID, expect); err != nil {
		return nil, err
	}
	// A go-live: the quota is checked under the account and market's
	// advisory lock, held until this transaction ends (FR-008).
	if q := ch.Quota; q != nil {
		if err := packages.EnforceTx(ctx, tx, q.Account, q.Market, m.ID, q.Now); err != nil {
			return nil, err
		}
	}
	revision, revisionStatus, err := revisionJSON(m.Revision)
	if err != nil {
		return nil, err
	}
	args := append([]any{m.ID, m.MarketCode, m.Status, m.UpdatedAt, m.Slug, timeOrNil(m.PublishedAt), timeOrNil(m.ExpiresAt),
		m.ReviewedAt, m.ReviewedBy, m.SubmittedAt, m.SubmittedBy, nullText(m.ReviewReason), revision, revisionStatus, m.AgentUserID}, contentArgs(m.Record)...)
	if _, err := tx.Exec(ctx, `
		UPDATE listings SET market_id = (SELECT id FROM markets WHERE code = $2), status = $3::text::listing_status, updated_at = $4,
		       slug = $5, published_at = $6, expires_at = $7, reviewed_at = $8, reviewed_by = $9, submitted_at = $10, submitted_by = $11,
		       review_reason = $12, revision = $13::jsonb, revision_status = $14, agent_user_id = $15,
		       title = $16, price_amount = $17::text::numeric, currency_code = $18, deal = $19, category = $20, property_type = $21,
		       price_period = $22, city_code = $23, suburb_code = $24, bedrooms = $25, bathrooms = $26, floor_area_m2 = $27,
		       land_area_m2 = $28, summary = $29, description = $30, max_guests = $31, min_nights = $32, city_name = $33, region_name = $34,
		       youtube_video_id = $35, youtube_start_seconds = $36, latitude = $37, longitude = $38, location_precision = $39
		WHERE id = $1`, args...); err != nil {
		if duplicate(err) {
			return nil, ErrDuplicate
		}
		return nil, fmt.Errorf("save listing: %w", err)
	}
	if err := writeFeatures(ctx, tx, m.ID, m.Features); err != nil {
		return nil, err
	}

	// Photos: those not kept are deleted (and returned); the others get
	// their alt text, position and approval. Positions are first moved out
	// of the way, because (listing_id, position) is unique.
	current, err := photosOf(ctx, tx, []uuid.UUID{m.ID})
	if err != nil {
		return nil, err
	}
	keep := map[uuid.UUID]StoredPhoto{}
	for _, p := range m.AllPhotos {
		keep[p.ID] = p
	}
	var removed []StoredPhoto
	for _, p := range current[m.ID] {
		if _, ok := keep[p.ID]; !ok {
			if _, err := tx.Exec(ctx, `DELETE FROM listing_photos WHERE id = $1`, p.ID); err != nil {
				return nil, fmt.Errorf("delete photo: %w", err)
			}
			removed = append(removed, p)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE listing_photos SET position = -1 - position WHERE listing_id = $1`, m.ID); err != nil {
		return nil, fmt.Errorf("move photos: %w", err)
	}
	for _, p := range current[m.ID] {
		k, ok := keep[p.ID]
		if !ok {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE listing_photos SET position = $2, alt = $3, approved_at = $4 WHERE id = $1`,
			p.ID, k.Position, k.Alt, k.ApprovedAt); err != nil {
			return nil, fmt.Errorf("update photo: %w", err)
		}
	}
	if ch.NewCycle {
		if _, err := tx.Exec(ctx, `
			INSERT INTO publication_cycles (listing_id, relist_no, published_at, expires_at)
			VALUES ($1, (SELECT COALESCE(max(relist_no) + 1, 0) FROM publication_cycles WHERE listing_id = $1), $2, $3)`,
			m.ID, m.PublishedAt, m.ExpiresAt); err != nil {
			return nil, fmt.Errorf("publication cycle: %w", err)
		}
	}
	if err := writeChange(ctx, tx, m.ID, ch, m.UpdatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return removed, nil
}

// DeleteListing implements ManageStore.
func (s *PostgresStore) DeleteListing(ctx context.Context, id uuid.UUID, expect time.Time) ([]StoredPhoto, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockListing(ctx, tx, id, expect); err != nil {
		return nil, err
	}
	photos, err := photosOf(ctx, tx, []uuid.UUID{id})
	if err != nil {
		return nil, err
	}
	// Features, photos, the timeline, the calendar and the publication
	// cycles go with the listing (ON DELETE CASCADE).
	if _, err := tx.Exec(ctx, `DELETE FROM listings WHERE id = $1`, id); err != nil {
		return nil, fmt.Errorf("delete listing: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return photos[id], nil
}

// AddPhoto implements ManageStore. The listing row is locked, so the count
// and the insert cannot race with another upload.
func (s *PostgresStore) AddPhoto(ctx context.Context, listingID uuid.UUID, p StoredPhoto, limit int, expect, now time.Time, ch Change) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockListing(ctx, tx, listingID, expect); err != nil {
		return err
	}
	var count, next int
	if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(max(position) + 1, 0) FROM listing_photos WHERE listing_id = $1`, listingID).
		Scan(&count, &next); err != nil {
		return err
	}
	if count >= limit {
		return ErrPhotoLimit
	}
	p.Position = next
	if err := insertPhoto(ctx, tx, listingID, p); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE listings SET updated_at = $2 WHERE id = $1`, listingID, now); err != nil {
		return err
	}
	if err := writeChange(ctx, tx, listingID, ch, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PhotoByID implements ManageStore.
func (s *PostgresStore) PhotoByID(ctx context.Context, photoID uuid.UUID) (uuid.UUID, StoredPhoto, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+photoColumns+` FROM listing_photos WHERE id = $1`, photoID)
	if err != nil {
		return uuid.Nil, StoredPhoto{}, err
	}
	found, err := pgx.CollectRows(rows, scanPhoto)
	if err != nil {
		return uuid.Nil, StoredPhoto{}, err
	}
	if len(found) == 0 {
		return uuid.Nil, StoredPhoto{}, ErrPhotoNotFound
	}
	return found[0].ListingID, found[0].Photo, nil
}

// ReviewQueue implements ManageStore.
func (s *PostgresStore) ReviewQueue(ctx context.Context, f ReviewFilter) ([]ManagedListing, int, error) {
	status := f.Status
	if status == "" {
		status = StatusPendingReview
	}
	b := &builder{}
	switch f.Kind {
	case ReviewKindNew:
		b.add(`l.status::text = %s`, status)
	case ReviewKindEdit:
		b.add(`l.revision_status = %s`, status)
	default:
		p := b.arg(status)
		b.conds = append(b.conds, `(l.status::text = `+p+` OR l.revision_status = `+p+`)`)
	}
	if f.Markets != nil {
		b.add(`m.code = ANY(%s::text[])`, f.Markets)
	}
	if f.Country != "" {
		b.add(`m.code = %s`, f.Country)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		p := b.arg(likePattern(q))
		e := b.arg(q)
		b.conds = append(b.conds, `(l.title ILIKE `+p+` OR upper(l.reference) = upper(`+e+`) OR l.revision->'content'->>'title' ILIKE `+p+`)`)
	}
	where := b.sql()
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM listings l JOIN markets m ON m.id = l.market_id WHERE `+where, b.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("review queue count: %w", err)
	}
	limit, offset := b.arg(f.PageSize), b.arg((f.Page-1)*f.PageSize)
	rows, err := s.pool.Query(ctx, `SELECT l.id FROM listings l JOIN markets m ON m.id = l.market_id WHERE `+where+
		` ORDER BY COALESCE(l.submitted_at, l.updated_at), l.id LIMIT `+limit+` OFFSET `+offset, b.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("review queue: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, 0, err
	}
	if len(ids) == 0 {
		return nil, total, nil
	}
	records, err := s.records(ctx, `l.id = ANY($1::uuid[])`, idStrings(ids))
	if err != nil {
		return nil, 0, err
	}
	byID := map[uuid.UUID]Record{}
	for _, r := range records {
		byID[r.ID] = r
	}
	ordered := make([]Record, 0, len(ids))
	for _, id := range ids {
		if r, ok := byID[id]; ok {
			ordered = append(ordered, r)
		}
	}
	out, err := s.managed(ctx, ordered)
	return out, total, err
}

// Timeline implements ManageStore.
func (s *PostgresStore) Timeline(ctx context.Context, listingID uuid.UUID) ([]TimelineEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, listing_id, actor_user_id, actor_kind, action, COALESCE(from_status, ''), COALESCE(to_status, ''),
		       COALESCE(review_kind, ''), COALESCE(reason, ''), created_at
		FROM listing_timeline WHERE listing_id = $1 ORDER BY created_at, id`, listingID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TimelineEntry, error) {
		var e TimelineEntry
		err := row.Scan(&e.ID, &e.ListingID, &e.ActorUserID, &e.ActorKind, &e.Action, &e.FromStatus, &e.ToStatus, &e.ReviewKind, &e.Reason, &e.CreatedAt)
		e.CreatedAt = e.CreatedAt.UTC()
		return e, err
	})
}

// Both stores implement listing management.
var (
	_ ManageStore = (*PostgresStore)(nil)
	_ ManageStore = (*MemoryStore)(nil)
)
