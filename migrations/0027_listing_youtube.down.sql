-- Reverts 0027_listing_youtube.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: the YouTube video (id and start time) of every listing. Pending
-- edits keep the values inside their revision jsonb, where the older API
-- ignores them. Reindex the search afterwards to drop has_video.

ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_youtube_start_seconds_check;
ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_youtube_video_id_check;
ALTER TABLE listings DROP COLUMN IF EXISTS youtube_start_seconds;
ALTER TABLE listings DROP COLUMN IF EXISTS youtube_video_id;
