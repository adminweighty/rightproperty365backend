package devseed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/platform/storage"
)

// SeedPostgres writes the same sample data into a development Postgres
// database, in ONE transaction. It is idempotent: every row has a fixed ID (or
// a natural key) and is upserted, so re-running it only resets the sample rows
// to their defined state (including the shared password). Rows it does not
// own are never touched. Callers must have passed CheckEnvironment.
//
// It requires every migration up to 0022. If a sample email already belongs to a
// different user row the run fails and changes nothing.
//
// NOTE: compiled but NEVER run — there is no Postgres on the machine this was
// written on. Treat the first run as a test.
func SeedPostgres(ctx context.Context, pool *pgxpool.Pool, files storage.Storage, hasher identity.Hasher, password string, now time.Time) error {
	if err := identity.CheckPassword(password); err != nil {
		return fmt.Errorf("DEV_SEED_PASSWORD %w", err)
	}
	// The sample listings must meet the listing rules (a short stay is deal
	// rent priced per night); the database enforces the same (migration 0013).
	if err := CheckListings(); err != nil {
		return err
	}
	hash, err := hasher.Hash(password)
	if err != nil {
		return err
	}
	now = now.UTC().Truncate(time.Microsecond)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Countries: every ISO country (also upserted by migration 0014). A row
	// keeps its ID and its `enabled` flag; name, currency and zone are reset
	// to the table in internal/modules/markets.
	for _, m := range markets.Seed() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO markets (id, code, name, currency_code, timezone, status, sort_order)
			VALUES ($1, $2, $3, $4, $5, 'ACTIVE', $6)
			ON CONFLICT (code) DO UPDATE
			SET name = EXCLUDED.name, currency_code = EXCLUDED.currency_code, timezone = EXCLUDED.timezone,
			    status = 'ACTIVE', updated_at = now()`,
			m.ID, m.Code, m.Name, m.CurrencyCode, m.Timezone, m.SortOrder); err != nil {
			return fmt.Errorf("seed country %s: %w", m.Code, err)
		}
	}

	// Reference data: the baseline rows (also inserted by migration 0007).
	// Existing rows keep their `active` flag, so re-seeding does not undo an
	// administrator's deactivation; labels and order are reset to the baseline.
	for set, items := range ReferenceItems() {
		for _, it := range items {
			var groupCode, groupLabel *string
			var groupSort *int
			if it.Group != nil {
				groupCode, groupLabel, groupSort = &it.Group.Code, &it.Group.Label, &it.Group.SortOrder
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id,
				                             parent_code, group_code, group_label, group_sort_order, requires_verification)
				VALUES ($1, $2, $3, $4, $5, $6, (SELECT id FROM markets WHERE code = $7), $8, $9, $10, $11, $12)
				ON CONFLICT (set_code, code) DO UPDATE
				SET label = EXCLUDED.label, singular_label = EXCLUDED.singular_label, description = EXCLUDED.description,
				    sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code,
				    group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label,
				    group_sort_order = EXCLUDED.group_sort_order, requires_verification = EXCLUDED.requires_verification,
				    updated_at = now()`,
				set, it.Code, it.Label, it.SingularLabel, it.Description, it.SortOrder, it.MarketCode,
				it.ParentCode, groupCode, groupLabel, groupSort, it.RequiresVerification); err != nil {
				return fmt.Errorf("seed reference %s/%s: %w", set, it.Code, err)
			}
		}
	}

	for _, o := range Organisations() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO organisations (id, market_id, name, enquiry_email, kind, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'ACTIVE', $6, $6)
			ON CONFLICT (id) DO UPDATE
			SET market_id = EXCLUDED.market_id, name = EXCLUDED.name, enquiry_email = EXCLUDED.enquiry_email,
			    kind = EXCLUDED.kind, status = 'ACTIVE', updated_at = EXCLUDED.updated_at`,
			o.ID, MarketID(MarketOf(o)), o.Name, o.EnquiryEmail, o.Kind, now); err != nil {
			return fmt.Errorf("seed organisation %s: %w", o.Name, err)
		}
	}

	for _, a := range Accounts() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (id, email, full_name, status, password_hash, email_verified_at, created_at, updated_at)
			VALUES ($1, $2, $3, 'ACTIVE', $4, $5, $5, $5)
			ON CONFLICT (id) DO UPDATE
			SET email = EXCLUDED.email, full_name = EXCLUDED.full_name, status = 'ACTIVE',
			    password_hash = EXCLUDED.password_hash,
			    email_verified_at = COALESCE(users.email_verified_at, EXCLUDED.email_verified_at),
			    updated_at = EXCLUDED.updated_at`,
			a.ID, a.Email, a.Name, hash, now); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return fmt.Errorf("seed %s: the email already belongs to a different account in this database", a.Email)
			}
			return fmt.Errorf("seed user %s: %w", a.Email, err)
		}

		// Registration consent, once.
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_consents (id, user_id, consent_type, version, accepted_at, created_at)
			SELECT $1, $2, $3, $4, $5, $5
			WHERE NOT EXISTS (SELECT 1 FROM user_consents WHERE user_id = $2 AND consent_type = $3)`,
			uuid.New(), a.ID, identity.ConsentPrivacyNotice, privacyVersion, now); err != nil {
			return fmt.Errorf("seed consent for %s: %w", a.Email, err)
		}

		// The sample account holds exactly its listed roles: a role an earlier
		// seed gave it and this one does not (a seeker role next to a
		// professional one, before seeker accounts became seeker-only) goes.
		if _, err := tx.Exec(ctx, `DELETE FROM user_account_roles WHERE user_id = $1 AND role <> ALL($2::text[])`,
			a.ID, append([]string{}, a.AccountRoles...)); err != nil {
			return fmt.Errorf("remove stale account roles of %s: %w", a.Email, err)
		}
		for _, role := range a.AccountRoles {
			if _, err := tx.Exec(ctx, `
				INSERT INTO user_account_roles (id, user_id, role, status, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $5)
				ON CONFLICT (user_id, role) DO UPDATE SET status = EXCLUDED.status, updated_at = EXCLUDED.updated_at`,
				uuid.New(), a.ID, role, identity.AccountRoleActive, now); err != nil {
				return fmt.Errorf("seed account role %s for %s: %w", role, a.Email, err)
			}
		}

		if a.Organisation != nil {
			if _, err := tx.Exec(ctx, `
				INSERT INTO memberships (id, organisation_id, user_id, role, status, market_id, created_at, updated_at)
				VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $6, $6)
				ON CONFLICT (organisation_id, user_id) DO UPDATE
				SET role = EXCLUDED.role, status = 'ACTIVE', market_id = EXCLUDED.market_id, updated_at = EXCLUDED.updated_at`,
				uuid.New(), a.Organisation.ID, a.ID, a.MembershipRole, MarketID(MarketOf(a.Organisation)), now); err != nil {
				return fmt.Errorf("seed membership for %s: %w", a.Email, err)
			}
		}

		if a.PlatformRole != "" && a.PlatformMarket != "" {
			// A market-scoped role (MARKET_ADMIN); see the partial unique index.
			if _, err := tx.Exec(ctx, `
				INSERT INTO platform_role_assignments (id, user_id, role, market_id, created_at)
				SELECT $1, $2, $3, m.id, $5 FROM markets m
				WHERE m.code = $4 AND NOT EXISTS (
					SELECT 1 FROM platform_role_assignments WHERE user_id = $2 AND role = $3 AND market_id = m.id)`,
				uuid.New(), a.ID, a.PlatformRole, a.PlatformMarket, now); err != nil {
				return fmt.Errorf("seed platform role for %s: %w", a.Email, err)
			}
		} else if a.PlatformRole != "" {
			// Global platform roles only (no market); see the partial unique index.
			if _, err := tx.Exec(ctx, `
				INSERT INTO platform_role_assignments (id, user_id, role, created_at)
				SELECT $1, $2, $3, $4
				WHERE NOT EXISTS (
					SELECT 1 FROM platform_role_assignments WHERE user_id = $2 AND role = $3 AND market_id IS NULL)`,
				uuid.New(), a.ID, a.PlatformRole, now); err != nil {
				return fmt.Errorf("seed platform role for %s: %w", a.Email, err)
			}
		}
	}

	// Listings: the web's real titles, prices, descriptions, features and
	// photos, each tied to its organisation and agent (or direct seller).
	for _, l := range ListingRecords(now) {
		var period, sellerName *string
		if l.PricePeriod != "" {
			period = &l.PricePeriod
		}
		if l.Advertiser.Type == listings.AdvertiserPrivateSeller {
			sellerName = &l.Advertiser.Name
		}
		// The sample draft and the listing waiting for review were never
		// published: no publication dates.
		var publishedAt, expiresAt *time.Time
		if !l.PublishedAt.IsZero() {
			publishedAt, expiresAt = &l.PublishedAt, &l.ExpiresAt
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO listings (id, market_id, title, status, price_amount, currency_code,
			                      organisation_id, agent_user_id, seller_user_id, published_at, expires_at, created_at, updated_at,
			                      slug, reference, deal, category, property_type, price_period, city_code, suburb_code,
			                      bedrooms, bathrooms, floor_area_m2, land_area_m2, summary, description, featured, private_seller_name,
			                      max_guests, min_nights, city_name, region_name, reviewed_at, reviewed_by)
			VALUES ($1, (SELECT id FROM markets WHERE code = $2), $3, $4::text::listing_status, $5::text::numeric, $6, $7, $8, $9, $10, $11, $12, $12,
			        $13, $14, $15, $16, $17, $18, NULLIF($19, ''), NULLIF($20, ''), $21, $22, $23, $24, $25, $26, $27, $28, $29, $30,
			        NULLIF($31, ''), NULLIF($32, ''), $33, $34)
			ON CONFLICT (id) DO UPDATE
			SET market_id = EXCLUDED.market_id, title = EXCLUDED.title, status = EXCLUDED.status,
			    price_amount = EXCLUDED.price_amount, currency_code = EXCLUDED.currency_code,
			    organisation_id = EXCLUDED.organisation_id, agent_user_id = EXCLUDED.agent_user_id,
			    seller_user_id = EXCLUDED.seller_user_id, published_at = EXCLUDED.published_at,
			    expires_at = EXCLUDED.expires_at, updated_at = EXCLUDED.updated_at,
			    slug = EXCLUDED.slug, reference = EXCLUDED.reference, deal = EXCLUDED.deal, category = EXCLUDED.category,
			    property_type = EXCLUDED.property_type, price_period = EXCLUDED.price_period,
			    city_code = EXCLUDED.city_code, suburb_code = EXCLUDED.suburb_code,
			    bedrooms = EXCLUDED.bedrooms, bathrooms = EXCLUDED.bathrooms,
			    floor_area_m2 = EXCLUDED.floor_area_m2, land_area_m2 = EXCLUDED.land_area_m2,
			    summary = EXCLUDED.summary, description = EXCLUDED.description, featured = EXCLUDED.featured,
			    private_seller_name = EXCLUDED.private_seller_name,
			    max_guests = EXCLUDED.max_guests, min_nights = EXCLUDED.min_nights,
			    city_name = EXCLUDED.city_name, region_name = EXCLUDED.region_name,
			    reviewed_at = EXCLUDED.reviewed_at, reviewed_by = EXCLUDED.reviewed_by,
			    -- Listing management (0020): a re-run drops pending changes and reviews.
			    revision = NULL, revision_status = NULL, review_reason = NULL, submitted_at = NULL, submitted_by = NULL`,
			l.ID, l.MarketCode, l.Title, l.Status, l.Price.Amount, l.Price.Currency,
			l.OrganisationID, l.AgentUserID, l.SellerUserID, publishedAt, expiresAt, l.UpdatedAt,
			l.Slug, l.Reference, l.Deal, l.Category, l.PropertyType, period, l.City, l.Suburb,
			l.Bedrooms, l.Bathrooms, l.FloorArea, l.LandArea, l.Summary, l.Description, l.Featured, sellerName,
			l.MaxGuests, l.MinNights, l.CityName, l.RegionName, l.ReviewedAt, l.ReviewedBy); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return fmt.Errorf("seed listing %s: its slug or reference already belongs to a different listing in this database", l.Slug)
			}
			return fmt.Errorf("seed listing %s: %w", l.Slug, err)
		}
		// The listing waiting for review joined the queue when it was last changed.
		if _, err := tx.Exec(ctx, `UPDATE listings SET submitted_at = updated_at WHERE id = $1 AND status = 'PENDING_REVIEW'`, l.ID); err != nil {
			return fmt.Errorf("seed listing %s submission: %w", l.Slug, err)
		}
		// Tell the search index: the worker turns this into an upsert or a delete.
		event := listings.EventListingPublished
		if l.Status != listings.StatusPublished {
			event = listings.EventListingExpired
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
			uuid.New(), event, listings.AggregateListing, l.ID, fmt.Sprintf(`{"listing_id":%q}`, l.ID), now); err != nil {
			return fmt.Errorf("seed listing %s event: %w", l.Slug, err)
		}
		// Features and photos are replaced wholesale so a re-run resets them.
		if _, err := tx.Exec(ctx, `DELETE FROM listing_features WHERE listing_id = $1`, l.ID); err != nil {
			return fmt.Errorf("seed listing %s features: %w", l.Slug, err)
		}
		for i, f := range l.Features {
			if _, err := tx.Exec(ctx, `INSERT INTO listing_features (listing_id, feature_code, position) VALUES ($1, $2, $3)`, l.ID, f, i); err != nil {
				return fmt.Errorf("seed listing %s feature %s: %w", l.Slug, f, err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM listing_photos WHERE listing_id = $1`, l.ID); err != nil {
			return fmt.Errorf("seed listing %s photos: %w", l.Slug, err)
		}
		// The photos of a listing that has been published are approved; those
		// of the sample draft and the listing waiting for review are not.
		var approved *time.Time
		if !l.PublishedAt.IsZero() {
			approved = &l.PublishedAt
		}
		for i, ph := range l.Photos {
			if _, err := tx.Exec(ctx, `
				INSERT INTO listing_photos (id, listing_id, position, url, alt, width, height, approved_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				uuid.New(), l.ID, i, ph.URL, ph.Alt, ph.Width, ph.Height, approved); err != nil {
				return fmt.Errorf("seed listing %s photo: %w", l.Slug, err)
			}
		}
	}

	// Directory profiles with the web's public content. No contact details:
	// the table has no such columns.
	orgMarket := map[uuid.UUID]uuid.UUID{}
	for _, o := range Organisations() {
		orgMarket[o.ID] = MarketID(MarketOf(o))
	}
	for _, p := range ProfileRecords(now) {
		offerings, err := json.Marshal(p.Offerings)
		if err != nil {
			return err
		}
		if p.Offerings == nil {
			offerings = []byte("[]")
		}
		var trade *string
		if p.TradeCode != "" {
			trade = &p.TradeCode
		}
		areas, services := p.Areas, p.Services
		if areas == nil {
			areas = []string{}
		}
		if services == nil {
			services = []string{}
		}
		// The sample logo (needs migration 0016). Re-seeding resets it, like
		// the rest of the sample profile: a logo uploaded to a SAMPLE profile
		// in this database is replaced (its object is left behind).
		logo := directory.Logo{}
		var logoKey *string
		var logoAt *time.Time
		if p.Logo != nil {
			logo, logoKey, logoAt = *p.Logo, &p.Logo.ObjectKey, &p.Logo.UpdatedAt
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO directory_profiles (id, market_id, kind, slug, name, organisation_id, agent_user_id, status, created_at, updated_at,
			                                role_title, tagline, about, city_code, trade_code, verified, sponsored, since_year,
			                                areas, services, offerings, verified_at,
			                                logo_object_key, logo_content_type, logo_width, logo_height, logo_checksum, logo_size_bytes, logo_updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'ACTIVE', $8, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19::jsonb, $20,
			        $21, NULLIF($22, ''), NULLIF($23, 0), NULLIF($24, 0), NULLIF($25, ''), NULLIF($26::bigint, 0), $27)
			ON CONFLICT (id) DO UPDATE
			SET market_id = EXCLUDED.market_id, kind = EXCLUDED.kind, slug = EXCLUDED.slug, name = EXCLUDED.name,
			    organisation_id = EXCLUDED.organisation_id, agent_user_id = EXCLUDED.agent_user_id,
			    status = 'ACTIVE', updated_at = EXCLUDED.updated_at,
			    role_title = EXCLUDED.role_title, tagline = EXCLUDED.tagline, about = EXCLUDED.about,
			    city_code = EXCLUDED.city_code, trade_code = EXCLUDED.trade_code, verified = EXCLUDED.verified,
			    sponsored = EXCLUDED.sponsored, since_year = EXCLUDED.since_year,
			    areas = EXCLUDED.areas, services = EXCLUDED.services, offerings = EXCLUDED.offerings,
			    verified_at = COALESCE(directory_profiles.verified_at, EXCLUDED.verified_at),
			    logo_object_key = EXCLUDED.logo_object_key, logo_content_type = EXCLUDED.logo_content_type,
			    logo_width = EXCLUDED.logo_width, logo_height = EXCLUDED.logo_height, logo_checksum = EXCLUDED.logo_checksum,
			    logo_size_bytes = EXCLUDED.logo_size_bytes, logo_updated_at = EXCLUDED.logo_updated_at`,
			p.ID, orgMarket[p.OrganisationID], p.Kind, p.Slug, p.Name, p.OrganisationID, p.AgentUserID, p.UpdatedAt,
			p.Role, p.Tagline, p.Description, p.CityCode, trade, p.Verified, p.Sponsored, p.Since,
			areas, services, string(offerings), p.VerifiedAt,
			logoKey, logo.ContentType, logo.Width, logo.Height, logo.Checksum, logo.Size, logoAt); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return fmt.Errorf("seed profile %s/%s: that kind and slug already belong to a different profile in this database", p.Kind, p.Slug)
			}
			return fmt.Errorf("seed profile %s/%s: %w", p.Kind, p.Slug, err)
		}
	}

	// The images the sample profiles' logos point to.
	if err := SeedLogoFiles(ctx, files); err != nil {
		return err
	}

	// Sample appointments (needs migration 0011), replaced on every run.
	// No events: nobody is emailed about sample data.
	for _, s := range Appointments(now) {
		if err := appointments.SeedTx(ctx, tx, s.Appointment, s.Timeline...); err != nil {
			return fmt.Errorf("seed appointment %s: %w", s.Appointment.Reference, err)
		}
	}

	// Availability of the sample short stays (needs migration 0015): each
	// one's calendar is replaced, so a re-run resets it. Nothing else is touched.
	for _, c := range AvailabilitySamples(now) {
		if _, err := tx.Exec(ctx, `DELETE FROM listing_availability WHERE listing_id = $1`, c.ListingID); err != nil {
			return fmt.Errorf("seed availability of %s: %w", c.ListingID, err)
		}
		for _, r := range c.Ranges {
			if _, err := tx.Exec(ctx, `
				INSERT INTO listing_availability (id, listing_id, first_night, last_night, reason, note, updated_at, updated_by)
				VALUES ($1, $2, $3::text::date, $4::text::date, $5, NULLIF($6, ''), $7, $8)`,
				r.ID, c.ListingID, r.First.String(), r.Last.String(), r.Reason, r.Note, r.UpdatedAt, r.UpdatedBy); err != nil {
				return fmt.Errorf("seed availability of %s: %w", c.ListingID, err)
			}
		}
		var updated *time.Time
		if len(c.Ranges) > 0 {
			updated = &now
		}
		if _, err := tx.Exec(ctx, `UPDATE listings SET availability_updated_at = $2 WHERE id = $1`, c.ListingID, updated); err != nil {
			return fmt.Errorf("seed availability of %s: %w", c.ListingID, err)
		}
	}

	// The two onboarding demo accounts (needs migration 0009). Their sample
	// documents are written to the configured file storage.
	if err := seedPostgresOnboarding(ctx, tx, files, hash, now); err != nil {
		return err
	}

	// The sample team's pending invitation and join request (needs migration 0021).
	if err := seedPostgresTeams(ctx, tx, files, hash, now); err != nil {
		return err
	}

	// The sample lease templates and leases (needs migration 0022).
	if err := seedPostgresLeases(ctx, tx, files, now); err != nil {
		return err
	}

	// The sample tenant's saved listings (needs migration 0026).
	if err := seedPostgresSaved(ctx, tx, now); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
