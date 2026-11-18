-- Reverts 0032_venue_hourly.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every listing's hourly settings and fees, every quote, every
-- hourly booking with its history, the organisation audit records of host
-- decisions on them, and the hourly_bookable flag. The property types added
-- by 0032 are deactivated, not deleted (listings may use them). Venue
-- booking outbox events already written stay. btree_gist stays installed.

DELETE FROM organisation_audit_events WHERE action LIKE 'VENUE_BOOKING_%' OR target_type = 'VENUE_BOOKING';
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_action_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_action_check CHECK (action IN (
    'INVITATION_CREATED', 'INVITATION_CANCELLED', 'INVITATION_ACCEPTED',
    'JOIN_REQUEST_APPROVED', 'JOIN_REQUEST_DECLINED', 'JOIN_REQUEST_CHANGES_REQUESTED',
    'MEMBER_ROLE_CHANGED', 'MEMBER_JOB_TITLE_CHANGED', 'MEMBER_SUSPENDED',
    'MEMBER_REACTIVATED', 'MEMBER_REMOVED', 'REQUEST_ASSIGNED', 'REQUEST_UNASSIGNED',
    'CONTACT_OWNER_CHANGED', 'CONTACTS_MERGED', 'CONTACTS_IMPORTED', 'CONTACTS_EXPORTED',
    'APPLICATION_VIEWED', 'APPLICATION_FILE_VIEWED'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION'));

DROP TABLE IF EXISTS venue_booking_events;
ALTER TABLE IF EXISTS venue_quotes DROP CONSTRAINT IF EXISTS venue_quotes_used_by_fkey;
DROP TABLE IF EXISTS venue_bookings;
DROP TABLE IF EXISTS venue_quotes;
DROP TABLE IF EXISTS venue_fees;
DROP TABLE IF EXISTS venue_settings;

UPDATE reference_items SET active = false, updated_at = now()
WHERE set_code = 'property_types' AND code IN ('showroom', 'event-space', 'hall', 'conference-room', 'studio');
ALTER TABLE reference_items DROP COLUMN IF EXISTS hourly_bookable;
