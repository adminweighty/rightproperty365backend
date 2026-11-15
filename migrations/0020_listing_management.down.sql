-- Reverts 0020_listing_management.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every uploaded photo row (their stored files stay in the object
-- storage and must be removed by hand: keys under listingphotos/), the
-- listings' pending changes, review reasons and timelines. Listings in the
-- statuses this migration added go back to DRAFT. A draft without a price
-- gets 0, because price_amount becomes NOT NULL again.
--
-- PostgreSQL cannot remove a value from an enum: CHANGES_REQUESTED, REJECTED
-- and ARCHIVED stay in listing_status, unused. Running 0020 again is safe
-- (ADD VALUE IF NOT EXISTS).
DROP TRIGGER IF EXISTS listing_timeline_no_update ON listing_timeline;
DROP FUNCTION IF EXISTS listing_timeline_immutable();
DROP INDEX IF EXISTS listing_timeline_listing_idx;
DROP TABLE IF EXISTS listing_timeline;

DELETE FROM listing_photos WHERE url IS NULL;
ALTER TABLE listing_photos
    DROP CONSTRAINT IF EXISTS listing_photos_alt_length,
    DROP CONSTRAINT IF EXISTS listing_photos_upload_checksum,
    DROP CONSTRAINT IF EXISTS listing_photos_one_source,
    DROP COLUMN IF EXISTS uploaded_by,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS variants,
    DROP COLUMN IF EXISTS checksum,
    ALTER COLUMN url SET NOT NULL;

DROP INDEX IF EXISTS listings_review_queue_idx;
UPDATE listings SET status = 'DRAFT' WHERE status::text IN ('CHANGES_REQUESTED', 'REJECTED', 'ARCHIVED');
UPDATE listings SET price_amount = 0 WHERE price_amount IS NULL;
ALTER TABLE listings
    DROP CONSTRAINT IF EXISTS listings_revision_has_status,
    DROP COLUMN IF EXISTS revision_status,
    DROP COLUMN IF EXISTS revision,
    DROP COLUMN IF EXISTS review_reason,
    DROP COLUMN IF EXISTS submitted_by,
    DROP COLUMN IF EXISTS submitted_at,
    DROP COLUMN IF EXISTS created_by,
    ALTER COLUMN price_amount SET NOT NULL;
