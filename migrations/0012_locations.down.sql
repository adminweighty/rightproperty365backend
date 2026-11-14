-- Reverts 0012_locations: removes the regions, the link from each city to
-- its region, and the indexes. Listings and profiles are untouched (they
-- never stored a region).
DROP INDEX IF EXISTS listings_public_floor_area_idx;
DROP INDEX IF EXISTS listings_public_market_idx;
DROP INDEX IF EXISTS reference_items_city_region_idx;

UPDATE reference_items SET parent_code = NULL, updated_at = now() WHERE set_code = 'cities';
DELETE FROM reference_items WHERE set_code = 'regions';

ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds'));
