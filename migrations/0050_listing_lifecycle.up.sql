-- 0050: the owner's listing lifecycle actions (API 0.48.0, FSD section 3):
-- pause and resume, mark sold, mark let, close. The statuses PAUSED, SOLD,
-- LET and CLOSED exist since 0001; the listing timeline gains their actions.
-- A PAUSED listing keeps its place in the package and its period keeps
-- running; SOLD, LET and CLOSED free the place (see README).

ALTER TABLE listing_timeline DROP CONSTRAINT IF EXISTS listing_timeline_action_check;
ALTER TABLE listing_timeline ADD CONSTRAINT listing_timeline_action_check
    CHECK (action IN ('CREATED', 'SUBMITTED', 'APPROVED', 'CHANGES_REQUESTED', 'REJECTED', 'ARCHIVED', 'RELISTED', 'EXPIRED',
                      'PAUSED', 'RESUMED', 'MARKED_SOLD', 'MARKED_LET', 'CLOSED'));
