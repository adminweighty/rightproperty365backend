-- 0020_listing_management: advertisers manage their own listings, with up to
-- 20 uploaded photos each, and platform reviewers approve them before they
-- are public.
--
-- Lifecycle (listings.status, the listing_status enum of 0001):
--   DRAFT -> PENDING_REVIEW (submit) -> PUBLISHED (approve)
--                                    -> CHANGES_REQUESTED (edit, submit again)
--                                    -> REJECTED (final)
--   any of DRAFT, PENDING_REVIEW, CHANGES_REQUESTED, PUBLISHED, EXPIRED,
--   PAUSED -> ARCHIVED (the advertiser withdraws it) -> relist.
-- DRAFT and PENDING_REVIEW already existed; this adds CHANGES_REQUESTED,
-- REJECTED and ARCHIVED. ALTER TYPE ... ADD VALUE runs inside the migration's
-- transaction (PostgreSQL 12 or later); the new values are therefore NOT
-- used anywhere in this file (a value added in a transaction cannot be used
-- before it commits).
--
-- A PUBLISHED listing is edited through a revision (listings.revision): the
-- public keeps seeing the approved content until a reviewer approves the
-- changes. Uploaded photos wait for the same approval
-- (listing_photos.approved_at); only approved photos are public.
--
-- Adds three enum values, columns to listings and listing_photos, one table
-- (listing_timeline) and two indexes. Existing rows keep their meaning: every
-- existing photo is approved (it was public), and no existing listing has a
-- revision.

ALTER TYPE listing_status ADD VALUE IF NOT EXISTS 'CHANGES_REQUESTED';
ALTER TYPE listing_status ADD VALUE IF NOT EXISTS 'REJECTED';
ALTER TYPE listing_status ADD VALUE IF NOT EXISTS 'ARCHIVED';

-- ---------------------------------------------------------------------------
-- Listings: who created it, the last submission, the reviewer's reason and
-- the pending changes of a published listing. A draft may have no price yet.
-- ---------------------------------------------------------------------------
ALTER TABLE listings
    ALTER COLUMN price_amount DROP NOT NULL,
    ADD COLUMN created_by      uuid REFERENCES users (id),
    ADD COLUMN submitted_at    timestamptz,
    ADD COLUMN submitted_by    uuid REFERENCES users (id),
    -- The reason of the last request for changes or rejection of the listing.
    ADD COLUMN review_reason   text CONSTRAINT listings_review_reason_length CHECK (char_length(review_reason) BETWEEN 1 AND 1000),
    -- The pending changes of a PUBLISHED listing (listings.Revision as JSON:
    -- status, content, photo_alts, reason, submitted_at, updated_at) and its
    -- status, kept as a column for the review queue.
    ADD COLUMN revision        jsonb,
    ADD COLUMN revision_status text CONSTRAINT listings_revision_status_values
                               CHECK (revision_status IN ('EDITING', 'PENDING_REVIEW', 'CHANGES_REQUESTED', 'REJECTED')),
    ADD CONSTRAINT listings_revision_has_status CHECK ((revision IS NULL) = (revision_status IS NULL));

-- The review queue: listings and revisions waiting for review, oldest first.
CREATE INDEX listings_review_queue_idx ON listings (submitted_at, id)
    WHERE status = 'PENDING_REVIEW' OR revision_status = 'PENDING_REVIEW';

-- ---------------------------------------------------------------------------
-- Photos. A bundled sample image keeps its web path in url; an upload has no
-- url and three stored sizes in variants ([{size, key, width, height,
-- bytes}], keys of the private object storage, never returned by the API).
-- width and height are the large size's. Order is position; at most 20
-- photos per listing is enforced by the API (atomically, under a lock on the
-- listing row).
-- ---------------------------------------------------------------------------
ALTER TABLE listing_photos
    ALTER COLUMN url DROP NOT NULL,
    ADD COLUMN checksum    text CONSTRAINT listing_photos_checksum_format CHECK (checksum ~ '^[0-9a-f]{64}$'),
    ADD COLUMN variants    jsonb,
    ADD COLUMN approved_at timestamptz,
    ADD COLUMN uploaded_by uuid REFERENCES users (id),
    ADD CONSTRAINT listing_photos_one_source CHECK ((url IS NULL) <> (variants IS NULL)),
    ADD CONSTRAINT listing_photos_upload_checksum CHECK (variants IS NULL OR checksum IS NOT NULL),
    ADD CONSTRAINT listing_photos_alt_length CHECK (char_length(alt) <= 250);

-- Every photo that exists was public: it is approved.
UPDATE listing_photos SET approved_at = created_at WHERE approved_at IS NULL;

-- ---------------------------------------------------------------------------
-- The listing's history: created, submitted, approved, changes requested,
-- rejected, archived, relisted. Immutable rows, like appointment_timeline.
-- ---------------------------------------------------------------------------
CREATE TABLE listing_timeline (
    id            uuid PRIMARY KEY,
    listing_id    uuid NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
    actor_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_kind    text NOT NULL CONSTRAINT listing_timeline_actor_kind_check CHECK (actor_kind IN ('ADVERTISER', 'REVIEWER')),
    action        text NOT NULL CONSTRAINT listing_timeline_action_check
                  CHECK (action IN ('CREATED', 'SUBMITTED', 'APPROVED', 'CHANGES_REQUESTED', 'REJECTED', 'ARCHIVED', 'RELISTED')),
    from_status   text,
    to_status     text,
    review_kind   text CONSTRAINT listing_timeline_review_kind_check CHECK (review_kind IN ('NEW', 'EDIT')),
    reason        text CONSTRAINT listing_timeline_reason_length CHECK (char_length(reason) BETWEEN 1 AND 1000),
    created_at    timestamptz NOT NULL
);
CREATE INDEX listing_timeline_listing_idx ON listing_timeline (listing_id, created_at);

CREATE FUNCTION listing_timeline_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.listing_id IS DISTINCT FROM OLD.listing_id
       OR NEW.actor_kind IS DISTINCT FROM OLD.actor_kind
       OR NEW.action IS DISTINCT FROM OLD.action
       OR NEW.from_status IS DISTINCT FROM OLD.from_status
       OR NEW.to_status IS DISTINCT FROM OLD.to_status
       OR NEW.review_kind IS DISTINCT FROM OLD.review_kind
       OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (NEW.actor_user_id IS DISTINCT FROM OLD.actor_user_id AND NEW.actor_user_id IS NOT NULL) THEN
        RAISE EXCEPTION 'listing_timeline rows are immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER listing_timeline_no_update BEFORE UPDATE ON listing_timeline
    FOR EACH ROW EXECUTE FUNCTION listing_timeline_immutable();
