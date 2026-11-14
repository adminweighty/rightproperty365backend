-- 0012_locations: the location hierarchy above the city, as reference data.
--
--   country (a market) > region > city > suburb
--
-- * A new reference set, `regions` (provinces, states), per market. Zimbabwe
--   gets its ten provinces; the planned markets get none yet.
-- * A city's parent_code is now its region (a suburb's parent_code stays its
--   city). A listing and a directory profile store their city; their region
--   is the region of that city, so the hierarchy has one source and nothing
--   is stored twice.
-- * Indexes for the new search filters (country, floor and land area).
--
-- Search on `country`, `region` and the advanced filters needs no other
-- schema change: listings already carry market_id, bedrooms, bathrooms,
-- floor_area_m2, land_area_m2, published_at and their photos.
--
-- NOT RUN: there is no PostgreSQL on the machine this was written on.

ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'regions', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds'));

-- Zimbabwe's ten provinces. Codes are the slugs of the names.
INSERT INTO reference_items (set_code, code, label, sort_order, market_id) VALUES ('regions', 'harare', 'Harare', 10, (SELECT id FROM markets WHERE code = 'ZW'));
INSERT INTO reference_items (set_code, code, label, sort_order, market_id) VALUES ('regions', 'bulawayo', 'Bulawayo', 20, (SELECT id FROM markets WHERE code = 'ZW'));
INSERT INTO reference_items (set_code, code, label, sort_order, market_id) VALUES ('regions', 'manicaland', 'Manicaland', 30, (SELECT id FROM markets WHERE code = 'ZW'));
INSERT INTO reference_items (set_code, code, label, sort_order, market_id) VALUES ('regions', 'mashonaland-central', 'Mashonaland Central', 40, (SELECT id FROM markets WHERE code = 'ZW'));
INSERT INTO reference_items (set_code, code, label, sort_order, market_id) VALUES ('regions', 'mashonaland-east', 'Mashonaland East', 50, (SELECT id FROM markets WHERE code = 'ZW'));
INSERT INTO reference_items (set_code, code, label, sort_order, market_id) VALUES ('regions', 'mashonaland-west', 'Mashonaland West', 60, (SELECT id FROM markets WHERE code = 'ZW'));
INSERT INTO reference_items (set_code, code, label, sort_order, market_id) VALUES ('regions', 'masvingo', 'Masvingo', 70, (SELECT id FROM markets WHERE code = 'ZW'));
INSERT INTO reference_items (set_code, code, label, sort_order, market_id) VALUES ('regions', 'matabeleland-north', 'Matabeleland North', 80, (SELECT id FROM markets WHERE code = 'ZW'));
INSERT INTO reference_items (set_code, code, label, sort_order, market_id) VALUES ('regions', 'matabeleland-south', 'Matabeleland South', 90, (SELECT id FROM markets WHERE code = 'ZW'));
INSERT INTO reference_items (set_code, code, label, sort_order, market_id) VALUES ('regions', 'midlands', 'Midlands', 100, (SELECT id FROM markets WHERE code = 'ZW'));

-- Each city belongs to a region.
UPDATE reference_items SET parent_code = 'harare', updated_at = now() WHERE set_code = 'cities' AND code = 'harare';
UPDATE reference_items SET parent_code = 'bulawayo', updated_at = now() WHERE set_code = 'cities' AND code = 'bulawayo';
UPDATE reference_items SET parent_code = 'manicaland', updated_at = now() WHERE set_code = 'cities' AND code = 'mutare';
UPDATE reference_items SET parent_code = 'matabeleland-north', updated_at = now() WHERE set_code = 'cities' AND code = 'victoria-falls';

-- A region is found from a city often (the region filter and facet).
CREATE INDEX reference_items_city_region_idx ON reference_items (parent_code, code) WHERE set_code = 'cities';

-- The country filter and the area filters and sort, on public listings.
CREATE INDEX listings_public_market_idx ON listings (market_id, published_at DESC) WHERE status = 'PUBLISHED' AND slug IS NOT NULL;
CREATE INDEX listings_public_floor_area_idx ON listings (floor_area_m2) WHERE status = 'PUBLISHED' AND slug IS NOT NULL;
