-- Reverts 0028_listing_coordinates.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: the coordinates and location precision of every listing. Pending
-- edits keep the values inside their revision jsonb, where the older API
-- ignores them. Reindex the search afterwards to drop the locations.

DROP INDEX IF EXISTS listings_coordinates_idx;
ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_location_precision_check;
ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_coordinates_pair_check;
ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_longitude_check;
ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_latitude_check;
ALTER TABLE listings DROP COLUMN IF EXISTS location_precision;
ALTER TABLE listings DROP COLUMN IF EXISTS longitude;
ALTER TABLE listings DROP COLUMN IF EXISTS latitude;
