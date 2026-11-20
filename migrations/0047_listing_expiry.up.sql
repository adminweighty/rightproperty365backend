-- 0047: the listing expiry job and its reminders (API 0.45.0, FR-006, FR-044,
-- FR-053, FSD section 4).
--
--   * listing_expiry_notices: one row per listing, publication cycle and
--     notice the expiry sweep (cmd/worker, hourly) has dealt with: the
--     reminders 14, 7 and 1 days before the end of the publication period
--     and the expiry itself. relist_no is publication_cycles.relist_no of the
--     listing's current cycle (0 for the first publication; 0 too for a
--     listing published without a cycle row, such as the sample data). The
--     primary key makes every reminder and every expiry happen exactly once
--     per cycle, across reruns and several workers. skipped is true for a
--     reminder that was recorded but not sent because a later one was
--     already due (a worker that was down sends only the most recent one).
--   * listing_timeline accepts the SYSTEM actor and the EXPIRED action: the
--     sweep records the expiry of a listing in its history.
--
-- The sweep stores status 'EXPIRED' (the enum value exists since 0001); the
-- read rule stays: PUBLISHED with expires_at <= now reads EXPIRED everywhere,
-- so a delayed worker never keeps an expired listing public.
--
-- No new listings index: listings_status_expires_idx (status, expires_at) of
-- 0001 already serves "status = 'PUBLISHED' AND expires_at <= $1".
--
-- Nothing is backfilled: the first run expires the listings already past
-- their period (and sends their listing-expired email).

CREATE TABLE IF NOT EXISTS listing_expiry_notices (
    listing_id uuid        NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
    relist_no  integer     NOT NULL CONSTRAINT listing_expiry_notices_relist_non_negative CHECK (relist_no >= 0),
    kind       text        NOT NULL CONSTRAINT listing_expiry_notices_kind_check CHECK (kind IN ('DAYS_14', 'DAYS_7', 'DAYS_1', 'EXPIRED')),
    skipped    boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (listing_id, relist_no, kind),
    CONSTRAINT listing_expiry_notices_skipped_reminders_only CHECK (NOT skipped OR kind <> 'EXPIRED')
);

ALTER TABLE listing_timeline DROP CONSTRAINT IF EXISTS listing_timeline_actor_kind_check;
ALTER TABLE listing_timeline ADD CONSTRAINT listing_timeline_actor_kind_check
    CHECK (actor_kind IN ('ADVERTISER', 'REVIEWER', 'SYSTEM'));
ALTER TABLE listing_timeline DROP CONSTRAINT IF EXISTS listing_timeline_action_check;
ALTER TABLE listing_timeline ADD CONSTRAINT listing_timeline_action_check
    CHECK (action IN ('CREATED', 'SUBMITTED', 'APPROVED', 'CHANGES_REQUESTED', 'REJECTED', 'ARCHIVED', 'RELISTED', 'EXPIRED'));
