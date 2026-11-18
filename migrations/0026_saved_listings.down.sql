-- Reverts 0026_saved_listings.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every customer's saved listings and their snapshots. The
-- LISTING_SAVED / LISTING_UNSAVED events already in the outbox stay.

DROP TABLE IF EXISTS saved_listings;
