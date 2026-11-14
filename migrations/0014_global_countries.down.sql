-- Reverts 0014_global_countries. LOSSY, read before running:
--
--   * The countries added by 0014 are NOT deleted: organisations, listings,
--     profiles, memberships and reference rows may point at them. They go
--     back to PLANNED, as AE, BW and ZA were; ZW and GB stay ACTIVE. The
--     older API then lists them all as planned markets.
--   * A listing or profile whose city was free text loses it (the columns go
--     away). Such a listing cannot be shown under the old rules, which need a
--     city code: it is NOT deleted (requests and appointments refer to it) but
--     taken off the site (status CLOSED). Such a profile is hidden (SUSPENDED).
--   * Onboarding applications lose their country and free-text city; the
--     market they belong to (market_code) is kept.
UPDATE listings SET status = 'CLOSED', updated_at = now()
WHERE city_name IS NOT NULL AND status = 'PUBLISHED';

UPDATE directory_profiles SET status = 'SUSPENDED', updated_at = now()
WHERE city_name IS NOT NULL AND status = 'ACTIVE';

ALTER TABLE onboarding_applications DROP COLUMN IF EXISTS city_name, DROP COLUMN IF EXISTS country_code;

DROP INDEX IF EXISTS directory_profiles_market_idx;
ALTER TABLE directory_profiles DROP CONSTRAINT IF EXISTS directory_profiles_free_text_place_check;
ALTER TABLE directory_profiles DROP COLUMN IF EXISTS region_name, DROP COLUMN IF EXISTS city_name;

ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_free_text_place_check;
ALTER TABLE listings DROP COLUMN IF EXISTS region_name, DROP COLUMN IF EXISTS city_name;

UPDATE markets SET status = 'PLANNED', updated_at = now() WHERE code NOT IN ('ZW', 'GB');
ALTER TABLE markets DROP COLUMN IF EXISTS enabled;
