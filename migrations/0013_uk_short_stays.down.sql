-- Reverts 0013_uk_short_stays. LOSSY, read before running:
--
--   * Short-stay listings cannot exist under the old rules (their price is
--     per night). They are NOT deleted (requests and appointments refer to
--     them): each is taken off the site (status CLOSED) and loses its
--     price period, guest and night numbers. Its category and property type
--     keep their codes, which no longer have reference rows.
--   * The United Kingdom market is NOT deleted (organisations, listings and
--     profiles may belong to it): it goes back to PLANNED. Its regions,
--     cities and areas are removed, so UK listings and profiles keep city
--     and suburb codes without reference rows.
--   * The price_periods reference set is removed.
DROP INDEX IF EXISTS listings_public_guests_idx;

UPDATE listings
SET status = 'CLOSED', price_period = NULL, max_guests = NULL, min_nights = NULL, updated_at = now()
WHERE category = 'short-stay' OR price_period = 'night' OR max_guests IS NOT NULL OR min_nights IS NOT NULL;

ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_guests_nights_check;
ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_night_price_check;
ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_short_stay_check;
ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_price_period_check;
ALTER TABLE listings ADD CONSTRAINT listings_price_period_check CHECK (price_period IN ('month'));
ALTER TABLE listings DROP COLUMN IF EXISTS min_nights, DROP COLUMN IF EXISTS max_guests;

DELETE FROM reference_items WHERE set_code = 'price_periods';
DELETE FROM reference_items WHERE set_code = 'property_types' AND parent_code = 'short-stay';
DELETE FROM reference_items WHERE set_code = 'property_categories' AND code = 'short-stay';
DELETE FROM reference_items
WHERE set_code IN ('regions', 'cities', 'suburbs') AND market_id = (SELECT id FROM markets WHERE code = 'GB');

ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'regions', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds'));

UPDATE markets SET status = 'PLANNED', updated_at = now() WHERE code = 'GB';
ALTER TABLE markets DROP COLUMN IF EXISTS sort_order;
