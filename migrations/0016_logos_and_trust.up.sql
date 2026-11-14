-- 0016_logos_and_trust: logos for directory profiles, the time a profile was
-- verified, and a review mark on listings.
--
-- Written for a database that already holds data (migrations 0001-0015 have
-- run or will have run on staging). Only nullable columns are added. One
-- UPDATE backfills verified_at on profiles that are already verified; no
-- other existing row is rewritten. The reference row and the per-role rows
-- are upserts: running them twice changes nothing, and an existing row keeps
-- its `active` flag.
--
-- BEHAVIOUR THAT CHANGES WITH THE RELEASE THIS MIGRATION BELONGS TO (the
-- rule is in the API's queries, not in this file): a listing is public only
-- while its advertiser is verified -
--   * an organisation's listing: the organisation is ACTIVE and has a
--     profile of its own (not an agent's) that is ACTIVE and verified;
--   * a direct owner's listing: the owner's account is ACTIVE and their
--     PRIVATE_SELLER or LANDLORD role is ACTIVE.
-- Listings that do not meet it stop being shown; nothing is deleted.

-- ---------------------------------------------------------------------------
-- Directory profiles: when the profile was verified, and its logo.
--
-- The logo is the ONE public file of a profile. The image itself is in the
-- object storage, under a random key that never leaves the API; it is served
-- only by GET /v1/directory/{kind}/{slug}/logo and only while the profile is
-- public. PNG or JPEG, at most 2 MB, 128 to 4096 pixels each way.
-- ---------------------------------------------------------------------------
ALTER TABLE directory_profiles
    -- When the profile was verified; NULL when it is not, or when the time
    -- was never recorded.
    ADD COLUMN verified_at       timestamptz,
    ADD COLUMN logo_object_key   text CONSTRAINT directory_profiles_logo_key_check
                                 CHECK (logo_object_key ~ '^[a-z0-9]+(/[a-z0-9]+)*$' AND char_length(logo_object_key) <= 200),
    ADD COLUMN logo_content_type text CONSTRAINT directory_profiles_logo_type_check
                                 CHECK (logo_content_type IN ('image/png', 'image/jpeg')),
    ADD COLUMN logo_width        integer CONSTRAINT directory_profiles_logo_width_check CHECK (logo_width BETWEEN 128 AND 4096),
    ADD COLUMN logo_height       integer CONSTRAINT directory_profiles_logo_height_check CHECK (logo_height BETWEEN 128 AND 4096),
    -- SHA-256 of the image, in hexadecimal: the ETag of the public route and
    -- the source of the version in the logo's URL.
    ADD COLUMN logo_checksum     text CONSTRAINT directory_profiles_logo_checksum_check CHECK (logo_checksum ~ '^[0-9a-f]{64}$'),
    ADD COLUMN logo_size_bytes   bigint CONSTRAINT directory_profiles_logo_size_check CHECK (logo_size_bytes BETWEEN 1 AND 2097152),
    -- When the logo was last set or removed.
    ADD COLUMN logo_updated_at   timestamptz,
    -- A logo is all of its columns or none of them.
    ADD CONSTRAINT directory_profiles_logo_complete CHECK (
        (logo_object_key IS NULL AND logo_content_type IS NULL AND logo_width IS NULL AND logo_height IS NULL
         AND logo_checksum IS NULL AND logo_size_bytes IS NULL)
        OR (logo_object_key IS NOT NULL AND logo_content_type IS NOT NULL AND logo_width IS NOT NULL AND logo_height IS NOT NULL
            AND logo_checksum IS NOT NULL AND logo_size_bytes IS NOT NULL));
-- Two profiles never share one stored object.
CREATE UNIQUE INDEX directory_profiles_logo_key_idx ON directory_profiles (logo_object_key) WHERE logo_object_key IS NOT NULL;

-- Profiles that are verified today were verified at an unrecorded time:
-- their creation time is the best answer there is.
UPDATE directory_profiles SET verified_at = created_at WHERE verified AND verified_at IS NULL;

-- ---------------------------------------------------------------------------
-- Listings: a mark that a platform administrator or moderator reviewed the
-- listing itself. NULL on every existing listing: there is no review
-- workflow yet, and only the sample-data seed sets it.
-- ---------------------------------------------------------------------------
ALTER TABLE listings
    ADD COLUMN reviewed_at timestamptz,
    ADD COLUMN reviewed_by uuid REFERENCES users (id),
    ADD CONSTRAINT listings_review_check CHECK ((reviewed_at IS NULL) = (reviewed_by IS NULL));

-- ---------------------------------------------------------------------------
-- Onboarding: the optional LOGO document. Unlike every other document it
-- becomes PUBLIC when the application is approved.
-- ---------------------------------------------------------------------------
INSERT INTO reference_items (set_code, code, label, description, sort_order) VALUES ('onboarding_documents', 'LOGO', 'Logo or profile photo', 'Optional. Your business logo, or your own photo if you are an agent. Unlike your other documents it is shown PUBLICLY, on your profile and on your listings, once your application is approved. PNG or JPEG, at most 2 MB, between 128x128 and 4096x4096 pixels.', 100)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, updated_at = now();

-- Every role whose approval creates a public profile may attach it.
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENT', 'LOGO', false)
    ON CONFLICT (role, document_code) DO UPDATE SET required = EXCLUDED.required, updated_at = now();
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SALES_AGENT', 'LOGO', false)
    ON CONFLICT (role, document_code) DO UPDATE SET required = EXCLUDED.required, updated_at = now();
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('LETTINGS_AGENT', 'LOGO', false)
    ON CONFLICT (role, document_code) DO UPDATE SET required = EXCLUDED.required, updated_at = now();
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('PROPERTY_MANAGER', 'LOGO', false)
    ON CONFLICT (role, document_code) DO UPDATE SET required = EXCLUDED.required, updated_at = now();
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SOLICITOR', 'LOGO', false)
    ON CONFLICT (role, document_code) DO UPDATE SET required = EXCLUDED.required, updated_at = now();
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_OWNER', 'LOGO', false)
    ON CONFLICT (role, document_code) DO UPDATE SET required = EXCLUDED.required, updated_at = now();
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('HOST', 'LOGO', false)
    ON CONFLICT (role, document_code) DO UPDATE SET required = EXCLUDED.required, updated_at = now();
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('DEVELOPER', 'LOGO', false)
    ON CONFLICT (role, document_code) DO UPDATE SET required = EXCLUDED.required, updated_at = now();
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SERVICE_PROVIDER', 'LOGO', false)
    ON CONFLICT (role, document_code) DO UPDATE SET required = EXCLUDED.required, updated_at = now();
