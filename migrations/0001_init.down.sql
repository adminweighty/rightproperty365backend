-- Reverts 0001_init. Destroys all data in these tables.
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS customer_activity;
DROP TABLE IF EXISTS request_timeline;
DROP TABLE IF EXISTS property_requests;
DROP TABLE IF EXISTS publication_cycles;
DROP TABLE IF EXISTS listings;
DROP TYPE IF EXISTS listing_status;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS organisations;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS markets;
