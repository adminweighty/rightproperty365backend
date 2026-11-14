-- 0018_request_work: working a property request after it arrives.
--
-- Until now a request was only ever stored and read. This adds what the
-- person who receives it needs to work it, and what platform operations need
-- to clear a request nobody could receive:
--
--   * property_requests.stage: the lead stage (the eight stages of the
--     updated functional specification). Every existing request starts at
--     NEW_ENQUIRY. `status` keeps its meaning (RECEIVED or ROUTING_EXCEPTION:
--     whether the request reached anybody) and is not touched.
--   * stage_changed_at / stage_changed_by: who moved it last, and when. The
--     full history is in request_timeline (event_type STAGE_CHANGED, with
--     detail {"from","to","actor_name"}).
--   * property_request_notes: internal notes of the receiving side. They are
--     never part of anything a customer can read.
--   * exception_resolved_*: a platform administrator marks a routing
--     exception as dealt with. The request stays a ROUTING_EXCEPTION (it still
--     has no recipient, see property_requests_routing_check).
--   * request_timeline.actor_type gains ADMIN for that administrator.
--
-- No row is rewritten apart from the column defaults; nothing is deleted.

ALTER TABLE property_requests
    ADD COLUMN stage                     text NOT NULL DEFAULT 'NEW_ENQUIRY'
        CONSTRAINT property_requests_stage_check CHECK (stage IN (
            'NEW_ENQUIRY', 'ASSIGNED', 'CONTACTED', 'VIEWING_REQUESTED',
            'VIEWING_SCHEDULED', 'FOLLOW_UP_REQUIRED', 'CONVERTED', 'CLOSED')),
    ADD COLUMN stage_changed_at          timestamptz,
    ADD COLUMN stage_changed_by          uuid REFERENCES users (id),
    ADD COLUMN exception_resolved_at     timestamptz,
    ADD COLUMN exception_resolved_by     uuid REFERENCES users (id),
    ADD COLUMN exception_resolution_note text
        CONSTRAINT property_requests_exception_note_length
        CHECK (exception_resolution_note IS NULL OR char_length(exception_resolution_note) <= 2000),
    ADD CONSTRAINT property_requests_exception_resolved_check
        CHECK (exception_resolved_at IS NULL OR status = 'ROUTING_EXCEPTION');

CREATE INDEX property_requests_recipient_org_stage_idx ON property_requests (recipient_organisation_id, stage)
    WHERE recipient_organisation_id IS NOT NULL;

ALTER TABLE request_timeline
    DROP CONSTRAINT request_timeline_actor_type_check,
    ADD CONSTRAINT request_timeline_actor_type_check
        CHECK (actor_type IN ('CUSTOMER', 'AGENT', 'STAFF', 'SYSTEM', 'ADMIN'));

CREATE TABLE property_request_notes (
    id             uuid PRIMARY KEY,
    request_id     uuid NOT NULL REFERENCES property_requests (id) ON DELETE CASCADE,
    author_user_id uuid NOT NULL REFERENCES users (id),
    -- The author's name when the note was written, so a note stays readable
    -- after the author leaves the organisation.
    author_name    text NOT NULL,
    body           text NOT NULL CONSTRAINT property_request_notes_body_length
                   CHECK (char_length(body) BETWEEN 1 AND 2000),
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX property_request_notes_request_idx ON property_request_notes (request_id, created_at);
