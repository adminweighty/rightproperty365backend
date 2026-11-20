-- Reverts 0047. The listings the sweep expired are set back to PUBLISHED
-- (the read rule makes them read EXPIRED again, since their expires_at has
-- passed); listings stored EXPIRED otherwise (the sample data) stay as they
-- are. The sweep's timeline entries are DELETED, then the notices table goes.

UPDATE listings l SET status = 'PUBLISHED'
WHERE l.status = 'EXPIRED'
  AND EXISTS (SELECT 1 FROM listing_expiry_notices n WHERE n.listing_id = l.id AND n.kind = 'EXPIRED');

DELETE FROM listing_timeline WHERE actor_kind = 'SYSTEM' OR action = 'EXPIRED';
ALTER TABLE listing_timeline DROP CONSTRAINT IF EXISTS listing_timeline_action_check;
ALTER TABLE listing_timeline ADD CONSTRAINT listing_timeline_action_check
    CHECK (action IN ('CREATED', 'SUBMITTED', 'APPROVED', 'CHANGES_REQUESTED', 'REJECTED', 'ARCHIVED', 'RELISTED'));
ALTER TABLE listing_timeline DROP CONSTRAINT IF EXISTS listing_timeline_actor_kind_check;
ALTER TABLE listing_timeline ADD CONSTRAINT listing_timeline_actor_kind_check
    CHECK (actor_kind IN ('ADVERTISER', 'REVIEWER'));

DROP TABLE IF EXISTS listing_expiry_notices;
