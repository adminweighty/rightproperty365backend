-- Reverts 0016_logos_and_trust. LOSSY, read before running:
--
--   * Every profile loses its logo. The image objects stay in the object
--     storage (under the "logos/" prefix) and are no longer referenced by
--     anything: remove them there if they must go.
--   * The time each profile was verified and the review mark of each listing
--     are deleted.
--   * LOGO documents already attached to onboarding applications are NOT
--     deleted (they are private documents like the others), but the type has
--     no reference row and no per-role row any more.
--   * The API release that needs this migration must be rolled back first:
--     it reads these columns.
DELETE FROM onboarding_role_documents WHERE document_code = 'LOGO';
DELETE FROM reference_items WHERE set_code = 'onboarding_documents' AND code = 'LOGO';

ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_review_check;
ALTER TABLE listings DROP COLUMN IF EXISTS reviewed_by, DROP COLUMN IF EXISTS reviewed_at;

DROP INDEX IF EXISTS directory_profiles_logo_key_idx;
ALTER TABLE directory_profiles DROP CONSTRAINT IF EXISTS directory_profiles_logo_complete;
ALTER TABLE directory_profiles
    DROP COLUMN IF EXISTS logo_updated_at, DROP COLUMN IF EXISTS logo_size_bytes, DROP COLUMN IF EXISTS logo_checksum,
    DROP COLUMN IF EXISTS logo_height, DROP COLUMN IF EXISTS logo_width, DROP COLUMN IF EXISTS logo_content_type,
    DROP COLUMN IF EXISTS logo_object_key, DROP COLUMN IF EXISTS verified_at;
