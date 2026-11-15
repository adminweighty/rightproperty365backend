-- Reverts 0021_agency_teams.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every invitation, the team audit trail, the members' job titles and
-- the UNASSIGNED appointment timeline rows. A SUSPENDED membership becomes
-- REMOVED (not ACTIVE: rolling back must never give a suspended person their
-- access back); reinstate such members by hand if that is wanted.
ALTER TABLE appointment_timeline DROP CONSTRAINT IF EXISTS appointment_timeline_event_check;
DELETE FROM appointment_timeline WHERE event = 'UNASSIGNED';
ALTER TABLE appointment_timeline
    ADD CONSTRAINT appointment_timeline_event_check
    CHECK (event IN ('REQUESTED', 'CONFIRMED', 'RESCHEDULE_REQUESTED', 'CANCELLED', 'COMPLETED', 'NO_SHOW', 'ASSIGNED'));

DROP TRIGGER IF EXISTS organisation_audit_events_no_update ON organisation_audit_events;
DROP FUNCTION IF EXISTS organisation_audit_events_immutable();
DROP INDEX IF EXISTS organisation_audit_events_organisation_idx;
DROP TABLE IF EXISTS organisation_audit_events;

DROP INDEX IF EXISTS organisation_invitations_organisation_idx;
DROP INDEX IF EXISTS organisation_invitations_one_pending_key;
DROP TABLE IF EXISTS organisation_invitations;

ALTER TABLE memberships DROP COLUMN IF EXISTS job_title;
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_status_check;
UPDATE memberships SET status = 'REMOVED', updated_at = now() WHERE status = 'SUSPENDED';
ALTER TABLE memberships
    ADD CONSTRAINT memberships_status_check CHECK (status IN ('ACTIVE', 'INVITED', 'REMOVED'));
