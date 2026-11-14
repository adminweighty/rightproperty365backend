-- Reverts 0018_request_work. THIS LOSES DATA: every internal note, the stage
-- of every request and the record of which routing exceptions were resolved.
-- Timeline rows written by an administrator (actor_type ADMIN) are deleted,
-- because the older constraint does not allow them.
--
-- The API release that needs this migration must be rolled back first.
DROP INDEX IF EXISTS property_request_notes_request_idx;
DROP TABLE IF EXISTS property_request_notes;

DELETE FROM request_timeline WHERE actor_type = 'ADMIN';
ALTER TABLE request_timeline
    DROP CONSTRAINT request_timeline_actor_type_check,
    ADD CONSTRAINT request_timeline_actor_type_check
        CHECK (actor_type IN ('CUSTOMER', 'AGENT', 'STAFF', 'SYSTEM'));

DROP INDEX IF EXISTS property_requests_recipient_org_stage_idx;
ALTER TABLE property_requests
    DROP CONSTRAINT IF EXISTS property_requests_exception_resolved_check,
    DROP COLUMN IF EXISTS exception_resolution_note,
    DROP COLUMN IF EXISTS exception_resolved_by,
    DROP COLUMN IF EXISTS exception_resolved_at,
    DROP COLUMN IF EXISTS stage_changed_by,
    DROP COLUMN IF EXISTS stage_changed_at,
    DROP COLUMN IF EXISTS stage;
