-- 0027: YouTube property tours (API 0.28.0, BR-013, FR-011).
--
--   * listings.youtube_video_id: the id of the YouTube video linked to the
--     listing (11 characters of A-Z, a-z, 0-9, "_" and "-"), parsed by the
--     API from a watch or share link of an allowlisted YouTube host. Only the
--     reference is stored: never the link as typed, never anything fetched
--     from YouTube.
--   * listings.youtube_start_seconds: where the video starts (from the t= or
--     start= of the link), >= 0; only with a video.
--   * A pending edit of a published listing keeps the same two values in its
--     revision (jsonb Content: "youtube_video_id", "youtube_start_seconds");
--     revisions written before this migration simply lack them (no video).
--
-- Safe on a database with data: two nullable columns, NULL for every
-- existing listing. The search index learns has_video at the next reindex.

ALTER TABLE listings ADD COLUMN IF NOT EXISTS youtube_video_id text;
ALTER TABLE listings ADD COLUMN IF NOT EXISTS youtube_start_seconds integer;

ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_youtube_video_id_check;
ALTER TABLE listings ADD CONSTRAINT listings_youtube_video_id_check
    CHECK (youtube_video_id IS NULL OR youtube_video_id ~ '^[A-Za-z0-9_-]{11}$');

ALTER TABLE listings DROP CONSTRAINT IF EXISTS listings_youtube_start_seconds_check;
ALTER TABLE listings ADD CONSTRAINT listings_youtube_start_seconds_check
    CHECK (youtube_start_seconds IS NULL OR (youtube_start_seconds >= 0 AND youtube_video_id IS NOT NULL));
