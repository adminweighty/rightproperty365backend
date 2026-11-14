-- Reverts 0015_availability. LOSSY, read before running:
--
--   * Every availability calendar and its audit trail are deleted: all
--     short stays are shown as available again.
--   * Requests to stay are NOT deleted, but they lose their dates and number
--     of guests, and the STAY_REQUEST type has no reference row any more (the
--     requests keep the code).
DELETE FROM reference_items WHERE set_code = 'request_types' AND code = 'STAY_REQUEST';

ALTER TABLE property_requests DROP CONSTRAINT IF EXISTS property_requests_stay_check;
ALTER TABLE property_requests DROP COLUMN IF EXISTS guests, DROP COLUMN IF EXISTS check_out, DROP COLUMN IF EXISTS check_in;

ALTER TABLE listings DROP COLUMN IF EXISTS availability_updated_at;

DROP TABLE IF EXISTS listing_availability_audit;
DROP TABLE IF EXISTS listing_availability;
