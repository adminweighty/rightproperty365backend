-- 0028: listing coordinates for map search (API 0.29.0, BR-002; FSD:
-- listing records store "geocoordinates").
--
--   * listings.latitude, listings.longitude: the position the lister set, in
--     decimal degrees with at most six decimals (about 10 cm). Both or
--     neither: a listing without coordinates is simply not on the map.
--   * listings.location_precision: EXACT (the public sees the stored
--     coordinates) or APPROXIMATE (the API rounds the public coordinates to
--     two decimals, about 1 km; the exact ones are only in the manage API).
--   * A pending edit of a published listing keeps the same values in its
--     revision (jsonb Content: "latitude", "longitude", "location_precision");
--     revisions written before this migration simply lack them.
--
-- PostGIS is NOT used (the server runs the official postgres:16 image,
-- without the extension) and no extension is created: a map box is a range
-- on two numeric columns, served by the btree index below.
--
-- Safe on a database with data: two nullable columns and one with a
-- default, NULL / EXACT for every existing listing. The search index learns
-- the locations at the next reindex (mapping version 7).

ALTER TABLE listings ADD COLUMN IF NOT EXISTS latitude numeric(9, 6);
ALTER TABLE listings ADD COLUMN IF NOT EXISTS longitude numeric(9, 6);
ALTER TABLE listings ADD COLUMN IF NOT EXISTS location_precision text NOT NULL DEFAULT 'EXACT';

ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_latitude_check;
ALTER TABLE listings ADD CONSTRAINT listings_latitude_check
    CHECK (latitude IS NULL OR latitude BETWEEN -90 AND 90);

ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_longitude_check;
ALTER TABLE listings ADD CONSTRAINT listings_longitude_check
    CHECK (longitude IS NULL OR longitude BETWEEN -180 AND 180);

ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_coordinates_pair_check;
ALTER TABLE listings ADD CONSTRAINT listings_coordinates_pair_check
    CHECK ((latitude IS NULL) = (longitude IS NULL));

ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_location_precision_check;
ALTER TABLE listings ADD CONSTRAINT listings_location_precision_check
    CHECK (location_precision IN ('EXACT', 'APPROXIMATE'));

CREATE INDEX IF NOT EXISTS listings_coordinates_idx ON listings (latitude, longitude) WHERE latitude IS NOT NULL;
