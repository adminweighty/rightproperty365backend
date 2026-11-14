-- Reverts 0008_listings_directory. Destroys all public listing content
-- (titles stay; slugs, descriptions, features and photos go) and all public
-- profile content.
DROP INDEX IF EXISTS directory_profiles_public_idx;
ALTER TABLE directory_profiles
    DROP COLUMN IF EXISTS offerings,
    DROP COLUMN IF EXISTS services,
    DROP COLUMN IF EXISTS areas,
    DROP COLUMN IF EXISTS since_year,
    DROP COLUMN IF EXISTS sponsored,
    DROP COLUMN IF EXISTS verified,
    DROP COLUMN IF EXISTS trade_code,
    DROP COLUMN IF EXISTS city_code,
    DROP COLUMN IF EXISTS about,
    DROP COLUMN IF EXISTS tagline,
    DROP COLUMN IF EXISTS role_title;

DROP TABLE IF EXISTS listing_photos;
DROP TABLE IF EXISTS listing_features;

DROP INDEX IF EXISTS listings_public_filter_idx;
DROP INDEX IF EXISTS listings_public_idx;
DROP INDEX IF EXISTS listings_reference_key;
DROP INDEX IF EXISTS listings_slug_key;
ALTER TABLE listings
    DROP COLUMN IF EXISTS private_seller_name,
    DROP COLUMN IF EXISTS featured,
    DROP COLUMN IF EXISTS description,
    DROP COLUMN IF EXISTS summary,
    DROP COLUMN IF EXISTS land_area_m2,
    DROP COLUMN IF EXISTS floor_area_m2,
    DROP COLUMN IF EXISTS bathrooms,
    DROP COLUMN IF EXISTS bedrooms,
    DROP COLUMN IF EXISTS suburb_code,
    DROP COLUMN IF EXISTS city_code,
    DROP COLUMN IF EXISTS price_period,
    DROP COLUMN IF EXISTS property_type,
    DROP COLUMN IF EXISTS category,
    DROP COLUMN IF EXISTS deal,
    DROP COLUMN IF EXISTS reference,
    DROP COLUMN IF EXISTS slug;
