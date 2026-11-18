-- Reverts 0029_listing_analytics.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every raw analytics event, the late-event records and the daily
-- rollups (all listing figures). The LISTING_RELISTED events already in the
-- outbox stay.

DROP TABLE IF EXISTS analytics_rollup_runs;
DROP TABLE IF EXISTS analytics_listing_daily;
DROP TABLE IF EXISTS analytics_late_events;
DROP TABLE IF EXISTS analytics_events;
