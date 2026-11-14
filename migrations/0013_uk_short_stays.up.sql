-- 0013_uk_short_stays: the United Kingdom as an active market, and short
-- stays (holiday homes, serviced apartments, guest houses, rooms let by the
-- night) as a listable category.
--
-- Written for a database that already holds data (migrations 0001-0012 have
-- run on staging). Nothing existing is rewritten except: markets gain a
-- display order, and the price_period check is widened. Reference rows are
-- upserts and keep an existing row's `active` flag.

-- ---------------------------------------------------------------------------
-- Markets: a display order (Zimbabwe, the launch market, stays first), and
-- the United Kingdom, ACTIVE. The ID is fixed so the application seed data
-- (internal/modules/markets) matches. If a GB row already exists it keeps
-- its ID and is brought up to date.
-- ---------------------------------------------------------------------------
ALTER TABLE markets ADD COLUMN sort_order integer NOT NULL DEFAULT 100;
UPDATE markets SET sort_order = 10 WHERE code = 'ZW';

INSERT INTO markets (id, code, name, currency_code, timezone, status, sort_order)
VALUES ('0195c0de-0000-7000-8000-00000000a005', 'GB', 'United Kingdom', 'GBP', 'Europe/London', 'ACTIVE', 20)
ON CONFLICT (code) DO UPDATE
SET name = EXCLUDED.name, currency_code = EXCLUDED.currency_code, timezone = EXCLUDED.timezone,
    status = 'ACTIVE', sort_order = EXCLUDED.sort_order, updated_at = now();

-- ---------------------------------------------------------------------------
-- Reference data: the short-stay category and its property types, the price
-- periods (so clients do not hard-code "per month" / "per night"), and the
-- United Kingdom's regions, a starter set of cities (each with its region)
-- and a few areas. Codes are unique per set; none of these existed before.
--
-- "Airbnb" is another company's trade mark: no code, type or label is named
-- after it. The word appears only in the category's description, which
-- free-text search matches, so that people who search for it find short stays.
-- ---------------------------------------------------------------------------
ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'regions', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds', 'price_periods'));

INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('property_categories', 'short-stay', 'Short stays', 'Holiday homes, serviced apartments, guest houses and rooms let by the night, including Airbnb-style stays.', 40, NULL, NULL) ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('property_types', 'holiday-home', 'Holiday home', NULL, 80, NULL, 'short-stay') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('property_types', 'serviced-apartment', 'Serviced apartment', NULL, 90, NULL, 'short-stay') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('property_types', 'guest-house', 'Guest house', NULL, 100, NULL, 'short-stay') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('property_types', 'private-room', 'Private room', NULL, 110, NULL, 'short-stay') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('property_types', 'lodge', 'Lodge', NULL, 120, NULL, 'short-stay') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('price_periods', 'month', 'per month', NULL, 10, NULL, NULL) ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('price_periods', 'night', 'per night', NULL, 20, NULL, NULL) ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('regions', 'england', 'England', NULL, 110, (SELECT id FROM markets WHERE code = 'GB'), NULL) ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('regions', 'scotland', 'Scotland', NULL, 120, (SELECT id FROM markets WHERE code = 'GB'), NULL) ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('regions', 'wales', 'Wales', NULL, 130, (SELECT id FROM markets WHERE code = 'GB'), NULL) ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('regions', 'northern-ireland', 'Northern Ireland', NULL, 140, (SELECT id FROM markets WHERE code = 'GB'), NULL) ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('cities', 'london', 'London', NULL, 50, (SELECT id FROM markets WHERE code = 'GB'), 'england') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('cities', 'manchester', 'Manchester', NULL, 60, (SELECT id FROM markets WHERE code = 'GB'), 'england') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('cities', 'birmingham', 'Birmingham', NULL, 70, (SELECT id FROM markets WHERE code = 'GB'), 'england') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('cities', 'leeds', 'Leeds', NULL, 80, (SELECT id FROM markets WHERE code = 'GB'), 'england') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('cities', 'liverpool', 'Liverpool', NULL, 90, (SELECT id FROM markets WHERE code = 'GB'), 'england') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('cities', 'bristol', 'Bristol', NULL, 100, (SELECT id FROM markets WHERE code = 'GB'), 'england') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('cities', 'edinburgh', 'Edinburgh', NULL, 110, (SELECT id FROM markets WHERE code = 'GB'), 'scotland') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('cities', 'glasgow', 'Glasgow', NULL, 120, (SELECT id FROM markets WHERE code = 'GB'), 'scotland') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('cities', 'cardiff', 'Cardiff', NULL, 130, (SELECT id FROM markets WHERE code = 'GB'), 'wales') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('cities', 'belfast', 'Belfast', NULL, 140, (SELECT id FROM markets WHERE code = 'GB'), 'northern-ireland') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('suburbs', 'camden', 'Camden', NULL, 190, (SELECT id FROM markets WHERE code = 'GB'), 'london') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('suburbs', 'greenwich', 'Greenwich', NULL, 200, (SELECT id FROM markets WHERE code = 'GB'), 'london') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('suburbs', 'kensington', 'Kensington', NULL, 210, (SELECT id FROM markets WHERE code = 'GB'), 'london') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('suburbs', 'shoreditch', 'Shoreditch', NULL, 220, (SELECT id FROM markets WHERE code = 'GB'), 'london') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('suburbs', 'wimbledon', 'Wimbledon', NULL, 230, (SELECT id FROM markets WHERE code = 'GB'), 'london') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('suburbs', 'northern-quarter', 'Northern Quarter', NULL, 240, (SELECT id FROM markets WHERE code = 'GB'), 'manchester') ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();

-- ---------------------------------------------------------------------------
-- Listings: a price can be per night, and a short stay says how many guests
-- it sleeps and its shortest stay.
-- ---------------------------------------------------------------------------
ALTER TABLE listings
    ADD COLUMN max_guests integer CONSTRAINT listings_max_guests_check CHECK (max_guests >= 1),
    ADD COLUMN min_nights integer CONSTRAINT listings_min_nights_check CHECK (min_nights >= 1);

-- NULL for a sale price; 'month' for a monthly rent; 'night' for a short stay.
ALTER TABLE listings DROP CONSTRAINT listings_price_period_check;
ALTER TABLE listings ADD CONSTRAINT listings_price_period_check CHECK (price_period IN ('month', 'night'));

-- A short stay is always let (deal 'rent') and priced per night; nothing else
-- is priced per night or has guest and night numbers. Existing rows have no
-- 'night' period and no short-stay category, so they satisfy all three.
ALTER TABLE listings ADD CONSTRAINT listings_short_stay_check
    CHECK (COALESCE(category, '') <> 'short-stay' OR (COALESCE(deal, '') = 'rent' AND COALESCE(price_period, '') = 'night'));
ALTER TABLE listings ADD CONSTRAINT listings_night_price_check
    CHECK (COALESCE(price_period, '') <> 'night' OR COALESCE(category, '') = 'short-stay');
ALTER TABLE listings ADD CONSTRAINT listings_guests_nights_check
    CHECK (COALESCE(category, '') = 'short-stay' OR (max_guests IS NULL AND min_nights IS NULL));

-- The guests filter, on public short stays.
CREATE INDEX listings_public_guests_idx ON listings (max_guests)
    WHERE status = 'PUBLISHED' AND slug IS NOT NULL AND category = 'short-stay';
