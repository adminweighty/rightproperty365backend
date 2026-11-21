DELETE FROM listing_timeline WHERE action IN ('PAUSED', 'RESUMED', 'MARKED_SOLD', 'MARKED_LET', 'CLOSED');
ALTER TABLE listing_timeline DROP CONSTRAINT IF EXISTS listing_timeline_action_check;
ALTER TABLE listing_timeline ADD CONSTRAINT listing_timeline_action_check
    CHECK (action IN ('CREATED', 'SUBMITTED', 'APPROVED', 'CHANGES_REQUESTED', 'REJECTED', 'ARCHIVED', 'RELISTED', 'EXPIRED'));
