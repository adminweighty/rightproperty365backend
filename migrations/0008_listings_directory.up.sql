-- 0008_listings_directory: the public content of listings and directory profiles.
--
-- 0001 created `listings` with only what request routing needed, and 0005
-- created `directory_profiles` with only what enquiry routing needed. This
-- migration adds what the public pages show.
--
-- Codes (deal, category, property_type, city_code, suburb_code, feature_code,
-- trade_code) are reference_items codes. They are validated by the API and
-- the seed, not by foreign keys, because reference rows can be retired while
-- old listings still carry their code.
--
-- Neither table has, or gets, a phone, email or street-address column.

-- ---------------------------------------------------------------------------
-- Listings
-- ---------------------------------------------------------------------------
ALTER TABLE listings
    -- Public address and human reference. NULL only for rows that predate
    -- this migration; a listing without a slug is never served.
    ADD COLUMN slug                text CONSTRAINT listings_slug_format CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(slug) <= 200),
    ADD COLUMN reference           text,
    ADD COLUMN deal                text,
    ADD COLUMN category            text,
    ADD COLUMN property_type       text,
    -- NULL for a sale price; 'month' for a monthly rent.
    ADD COLUMN price_period        text CONSTRAINT listings_price_period_check CHECK (price_period IN ('month')),
    ADD COLUMN city_code           text,
    ADD COLUMN suburb_code         text,
    ADD COLUMN bedrooms            integer CONSTRAINT listings_bedrooms_check CHECK (bedrooms >= 0),
    ADD COLUMN bathrooms           integer CONSTRAINT listings_bathrooms_check CHECK (bathrooms >= 0),
    -- Square metres.
    ADD COLUMN floor_area_m2       integer CONSTRAINT listings_floor_area_check CHECK (floor_area_m2 >= 0),
    ADD COLUMN land_area_m2        integer CONSTRAINT listings_land_area_check CHECK (land_area_m2 >= 0),
    ADD COLUMN summary             text,
    ADD COLUMN description         text,
    ADD COLUMN featured            boolean NOT NULL DEFAULT false,
    -- Display name of a direct seller (listings without an organisation).
    ADD COLUMN private_seller_name text;
CREATE UNIQUE INDEX listings_slug_key ON listings (slug) WHERE slug IS NOT NULL;
CREATE UNIQUE INDEX listings_reference_key ON listings (reference) WHERE reference IS NOT NULL;
-- The public search: PUBLISHED and unexpired, newest first, with the common filters.
CREATE INDEX listings_public_idx ON listings (published_at DESC, id) WHERE status = 'PUBLISHED' AND slug IS NOT NULL;
CREATE INDEX listings_public_filter_idx ON listings (deal, category, city_code, suburb_code) WHERE status = 'PUBLISHED' AND slug IS NOT NULL;

CREATE TABLE listing_features (
    listing_id   uuid NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
    feature_code text NOT NULL,
    position     integer NOT NULL DEFAULT 0,
    PRIMARY KEY (listing_id, feature_code)
);
CREATE INDEX listing_features_code_idx ON listing_features (feature_code);

-- Photos. `url` is a path served by the web app today; object storage and
-- uploads are not built.
CREATE TABLE listing_photos (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id uuid NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
    position   integer NOT NULL DEFAULT 0,
    url        text NOT NULL,
    alt        text NOT NULL DEFAULT '',
    width      integer NOT NULL CONSTRAINT listing_photos_width_check CHECK (width > 0),
    height     integer NOT NULL CONSTRAINT listing_photos_height_check CHECK (height > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT listing_photos_position_key UNIQUE (listing_id, position)
);

-- ---------------------------------------------------------------------------
-- Directory profiles: public content
-- ---------------------------------------------------------------------------
ALTER TABLE directory_profiles
    -- Job title for a person, speciality for a business.
    ADD COLUMN role_title text,
    ADD COLUMN tagline    text,
    ADD COLUMN about      text,
    ADD COLUMN city_code  text,
    -- Service providers: their trade.
    ADD COLUMN trade_code text,
    ADD COLUMN verified   boolean NOT NULL DEFAULT false,
    -- Paid placement; always labelled as sponsored wherever it is shown.
    ADD COLUMN sponsored  boolean NOT NULL DEFAULT false,
    ADD COLUMN since_year integer CONSTRAINT directory_profiles_since_check CHECK (since_year BETWEEN 1800 AND 2200),
    -- Free-text area names and service names, in display order.
    ADD COLUMN areas      text[] NOT NULL DEFAULT '{}',
    ADD COLUMN services   text[] NOT NULL DEFAULT '{}',
    -- [{"name": "...", "description": "..."}]
    ADD COLUMN offerings  jsonb NOT NULL DEFAULT '[]'::jsonb;
CREATE INDEX directory_profiles_public_idx ON directory_profiles (kind, sponsored DESC, name) WHERE status = 'ACTIVE';
