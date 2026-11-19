-- 0046: the development and investment showcase (API 0.44.0, BR-019, FR-048).
--
--   * developments: a new-development project of a developer. It belongs to
--     the developer's directory profile (profile_id, kind 'developers') and
--     to that profile's organisation (organisation_id), whose OWNER, MANAGER
--     and AGENT edit it. Its address is a market, a city (a reference code or,
--     in a country without reference cities, free text) and a suburb, with
--     optional coordinates (the 0028 rules: both or neither, EXACT or
--     APPROXIMATE). There is NO street-address, phone or email column: the
--     project is reached only through a business enquiry.
--     status is the construction status (PLANNED .. SOLD_OUT);
--     moderation_status is the review (DRAFT -> PENDING_REVIEW -> PUBLISHED,
--     or REJECTED). Only a PUBLISHED project of a public profile is public.
--     The slug is unique per market. highlights are the developer's own
--     investment statements ([{label, value}]), shown as such, never advice.
--     amenities are development_amenities codes (validated by the API).
--   * development_phases and development_unit_types: the phases of a project
--     and the unit types of each phase (bedrooms, bathrooms, a size range, a
--     price from / to with its currency or "price on request", units in all
--     and available). A unit type carries its development_id so links and
--     enquiries can name it with the project (composite keys).
--   * development_photos: as listing_photos after 0020 (a bundled sample
--     image has a url; an upload has variants, object keys of the private
--     storage). Only approved photos are public; approval is the project's.
--   * development_listings: a listing of the same organisation shown as an
--     available unit of a project (and optionally of one of its unit types).
--     A listing is in at most one project. Deleting the unit type keeps the
--     link without a unit type (ON DELETE SET NULL (unit_type_id), Postgres 15+).
--   * business_enquiries gains the project (and unit type) an enquiry is
--     about, with snapshots of their names, so the developer's inbox quotes
--     them even after a rename or a deletion.
--   * reference_items gains the admin-editable set development_amenities;
--     admin_audit_events may name a DEVELOPMENT (approve / reject).
--
-- Constraint names never take the form <table>_<column>_check (the name
-- Postgres gives an unnamed column check), so that an ALTER of a later
-- migration can never collide with one.
--
-- Safe on a database with data: new tables, new nullable columns, a wider
-- check. Re-runnable (IF NOT EXISTS / DROP ... IF EXISTS / ON CONFLICT).
-- Not in Elasticsearch: the public list is a Postgres query (no reindex).

-- ---------------------------------------------------------------------------
-- Reference data: development amenities (admin-editable).
-- ---------------------------------------------------------------------------
ALTER TABLE reference_items DROP CONSTRAINT IF EXISTS reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'regions', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds', 'price_periods', 'contact_types', 'contact_sources',
        'maintenance_categories', 'maintenance_priorities', 'inspection_checklist_templates',
        'development_amenities'));

INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'gated-community', 'Gated community', NULL, 10, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'security-24h', '24-hour security', NULL, 20, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'clubhouse', 'Clubhouse', NULL, 30, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'swimming-pool', 'Swimming pool', NULL, 40, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'gym', 'Gym', NULL, 50, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'childrens-play-area', 'Children''s play area', NULL, 60, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'borehole-water', 'Borehole water', NULL, 70, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'solar-backup', 'Solar back-up power', NULL, 80, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'fibre-internet', 'Fibre internet', NULL, 90, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'paved-roads', 'Paved roads', NULL, 100, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'landscaped-gardens', 'Landscaped gardens', NULL, 110, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'walking-trails', 'Walking trails', NULL, 120, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'visitor-parking', 'Visitor parking', NULL, 130, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('development_amenities', 'retail-centre', 'Retail centre', NULL, 140, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Projects
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS developments (
    id                    uuid PRIMARY KEY,
    organisation_id       uuid NOT NULL REFERENCES organisations (id) ON DELETE CASCADE,
    profile_id            uuid NOT NULL REFERENCES directory_profiles (id) ON DELETE CASCADE,
    market_code           text NOT NULL REFERENCES markets (code),
    slug                  text NOT NULL CONSTRAINT developments_slug_format
                          CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(slug) <= 80),
    name                  text NOT NULL CONSTRAINT developments_name_length CHECK (char_length(name) BETWEEN 1 AND 120),
    summary               text NOT NULL DEFAULT '' CONSTRAINT developments_summary_length CHECK (char_length(summary) <= 200),
    description           text NOT NULL DEFAULT '' CONSTRAINT developments_description_length CHECK (char_length(description) <= 5000),
    status                text NOT NULL DEFAULT 'PLANNED' CONSTRAINT developments_status_allowed
                          CHECK (status IN ('PLANNED', 'UNDER_CONSTRUCTION', 'SELLING', 'COMPLETED', 'SOLD_OUT')),
    moderation_status     text NOT NULL DEFAULT 'DRAFT' CONSTRAINT developments_moderation_allowed
                          CHECK (moderation_status IN ('DRAFT', 'PENDING_REVIEW', 'PUBLISHED', 'REJECTED')),
    -- "2027-Q3" or "2027-06-30" (validated as a date by the API).
    completion_estimate   text CONSTRAINT developments_completion_format
                          CHECK (completion_estimate ~ '^[0-9]{4}-(Q[1-4]|[0-9]{2}-[0-9]{2})$'),
    city_code             text,
    suburb_code           text,
    city_name             text CONSTRAINT developments_city_name_length CHECK (char_length(city_name) BETWEEN 1 AND 80),
    latitude              numeric(9, 6) CONSTRAINT developments_latitude_range CHECK (latitude BETWEEN -90 AND 90),
    longitude             numeric(9, 6) CONSTRAINT developments_longitude_range CHECK (longitude BETWEEN -180 AND 180),
    location_precision    text NOT NULL DEFAULT 'EXACT' CONSTRAINT developments_precision_allowed
                          CHECK (location_precision IN ('EXACT', 'APPROXIMATE')),
    amenities             text[] NOT NULL DEFAULT '{}',
    highlights            jsonb NOT NULL DEFAULT '[]'::jsonb,
    youtube_video_id      text CONSTRAINT developments_youtube_format CHECK (youtube_video_id ~ '^[A-Za-z0-9_-]{11}$'),
    youtube_start_seconds integer,
    review_reason         text CONSTRAINT developments_review_reason_length CHECK (char_length(review_reason) BETWEEN 1 AND 1000),
    submitted_at          timestamptz,
    submitted_by          uuid REFERENCES users (id) ON DELETE SET NULL,
    reviewed_at           timestamptz,
    reviewed_by           uuid REFERENCES users (id) ON DELETE SET NULL,
    published_at          timestamptz,
    created_by            uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at            timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL,
    CONSTRAINT developments_market_slug_unique UNIQUE (market_code, slug),
    CONSTRAINT developments_coordinates_pair CHECK ((latitude IS NULL) = (longitude IS NULL)),
    CONSTRAINT developments_youtube_start_valid
        CHECK (youtube_start_seconds IS NULL OR (youtube_start_seconds >= 0 AND youtube_video_id IS NOT NULL)),
    CONSTRAINT developments_city_one_kind CHECK (city_code IS NULL OR city_name IS NULL),
    CONSTRAINT developments_published_when_public CHECK (moderation_status <> 'PUBLISHED' OR published_at IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS developments_organisation_idx ON developments (organisation_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS developments_profile_idx ON developments (profile_id);
CREATE INDEX IF NOT EXISTS developments_public_idx ON developments (published_at DESC, id) WHERE moderation_status = 'PUBLISHED';
CREATE INDEX IF NOT EXISTS developments_review_idx ON developments (submitted_at, id) WHERE moderation_status = 'PENDING_REVIEW';

CREATE TABLE IF NOT EXISTS development_phases (
    id                  uuid PRIMARY KEY,
    development_id      uuid NOT NULL REFERENCES developments (id) ON DELETE CASCADE,
    name                text NOT NULL CONSTRAINT development_phases_name_length CHECK (char_length(name) BETWEEN 1 AND 80),
    status              text NOT NULL DEFAULT 'PLANNED' CONSTRAINT development_phases_status_allowed
                        CHECK (status IN ('PLANNED', 'UNDER_CONSTRUCTION', 'SELLING', 'COMPLETED', 'SOLD_OUT')),
    completion_estimate text CONSTRAINT development_phases_completion_format
                        CHECK (completion_estimate ~ '^[0-9]{4}-(Q[1-4]|[0-9]{2}-[0-9]{2})$'),
    position            integer NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL,
    CONSTRAINT development_phases_development_id_unique UNIQUE (development_id, id)
);

CREATE TABLE IF NOT EXISTS development_unit_types (
    id               uuid PRIMARY KEY,
    development_id   uuid NOT NULL,
    phase_id         uuid NOT NULL,
    name             text NOT NULL CONSTRAINT development_unit_types_name_length CHECK (char_length(name) BETWEEN 1 AND 80),
    bedrooms         integer CONSTRAINT development_unit_types_bedrooms_range CHECK (bedrooms BETWEEN 0 AND 50),
    bathrooms        integer CONSTRAINT development_unit_types_bathrooms_range CHECK (bathrooms BETWEEN 0 AND 50),
    size_min_m2      integer CONSTRAINT development_unit_types_size_min_range CHECK (size_min_m2 BETWEEN 1 AND 100000),
    size_max_m2      integer CONSTRAINT development_unit_types_size_max_range CHECK (size_max_m2 BETWEEN 1 AND 100000),
    price_on_request boolean NOT NULL DEFAULT false,
    price_from       numeric(20, 4) CONSTRAINT development_unit_types_price_from_range CHECK (price_from >= 0),
    price_to         numeric(20, 4) CONSTRAINT development_unit_types_price_to_range CHECK (price_to >= 0),
    currency_code    text CONSTRAINT development_unit_types_currency_format CHECK (currency_code ~ '^[A-Z]{3}$'),
    units_total      integer CONSTRAINT development_unit_types_units_total_range CHECK (units_total >= 0),
    units_available  integer CONSTRAINT development_unit_types_units_available_range CHECK (units_available >= 0),
    position         integer NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL,
    CONSTRAINT development_unit_types_phase_fk FOREIGN KEY (development_id, phase_id)
        REFERENCES development_phases (development_id, id) ON DELETE CASCADE,
    CONSTRAINT development_unit_types_development_id_unique UNIQUE (development_id, id),
    CONSTRAINT development_unit_types_size_order CHECK (size_min_m2 IS NULL OR size_max_m2 IS NULL OR size_min_m2 <= size_max_m2),
    CONSTRAINT development_unit_types_price_shape CHECK (
        (price_on_request AND price_from IS NULL AND price_to IS NULL AND currency_code IS NULL)
        OR (NOT price_on_request AND price_from IS NOT NULL AND currency_code IS NOT NULL
            AND (price_to IS NULL OR price_to >= price_from))),
    CONSTRAINT development_unit_types_units_order
        CHECK (units_total IS NULL OR units_available IS NULL OR units_available <= units_total)
);
CREATE INDEX IF NOT EXISTS development_unit_types_phase_idx ON development_unit_types (phase_id);

CREATE TABLE IF NOT EXISTS development_photos (
    id             uuid PRIMARY KEY,
    development_id uuid NOT NULL REFERENCES developments (id) ON DELETE CASCADE,
    position       integer NOT NULL DEFAULT 0,
    url            text,
    alt            text NOT NULL DEFAULT '' CONSTRAINT development_photos_alt_length CHECK (char_length(alt) <= 250),
    width          integer NOT NULL CONSTRAINT development_photos_width_positive CHECK (width > 0),
    height         integer NOT NULL CONSTRAINT development_photos_height_positive CHECK (height > 0),
    checksum       text CONSTRAINT development_photos_checksum_format CHECK (checksum ~ '^[0-9a-f]{64}$'),
    variants       jsonb,
    approved_at    timestamptz,
    uploaded_by    uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at     timestamptz NOT NULL,
    CONSTRAINT development_photos_one_source CHECK ((url IS NULL) <> (variants IS NULL)),
    CONSTRAINT development_photos_upload_checksum CHECK (variants IS NULL OR checksum IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS development_photos_development_idx ON development_photos (development_id, position);

CREATE TABLE IF NOT EXISTS development_listings (
    listing_id     uuid PRIMARY KEY REFERENCES listings (id) ON DELETE CASCADE,
    development_id uuid NOT NULL REFERENCES developments (id) ON DELETE CASCADE,
    unit_type_id   uuid,
    created_at     timestamptz NOT NULL,
    CONSTRAINT development_listings_unit_type_fk FOREIGN KEY (development_id, unit_type_id)
        REFERENCES development_unit_types (development_id, id) ON DELETE SET NULL (unit_type_id)
);
CREATE INDEX IF NOT EXISTS development_listings_development_idx ON development_listings (development_id);

-- ---------------------------------------------------------------------------
-- Business enquiries about a project.
-- ---------------------------------------------------------------------------
ALTER TABLE business_enquiries ADD COLUMN IF NOT EXISTS development_id uuid REFERENCES developments (id) ON DELETE SET NULL;
ALTER TABLE business_enquiries ADD COLUMN IF NOT EXISTS development_unit_type_id uuid REFERENCES development_unit_types (id) ON DELETE SET NULL;
ALTER TABLE business_enquiries ADD COLUMN IF NOT EXISTS development_name text;
ALTER TABLE business_enquiries ADD COLUMN IF NOT EXISTS development_market text;
ALTER TABLE business_enquiries ADD COLUMN IF NOT EXISTS development_slug text;
ALTER TABLE business_enquiries ADD COLUMN IF NOT EXISTS development_unit_type_name text;
ALTER TABLE business_enquiries DROP CONSTRAINT IF EXISTS business_enquiries_development_snapshot;
ALTER TABLE business_enquiries ADD CONSTRAINT business_enquiries_development_snapshot
    CHECK ((development_name IS NULL) = (development_slug IS NULL) AND (development_name IS NULL) = (development_market IS NULL)
           AND (development_unit_type_name IS NULL OR development_name IS NOT NULL));
CREATE INDEX IF NOT EXISTS business_enquiries_development_idx ON business_enquiries (development_id, created_at DESC)
    WHERE development_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- The administrators' audit may name a project.
-- ---------------------------------------------------------------------------
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW', 'LISTING_PACKAGE', 'ORGANISATION',
                           'LEGAL_DOCUMENT_VERSION', 'TAX_RULE', 'DEVELOPMENT'));
