-- Reverts 0033_agent_ledger.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every organisation's and direct owner's books (entries, notes,
-- attachment records, categories) and the organisation audit records of
-- ledger actions. Attachment files stay in the object storage (their keys
-- are gone with the rows). Dropping the table does not fire the guard
-- trigger (it refuses row deletes, not DROP).

DELETE FROM organisation_audit_events WHERE action LIKE 'LEDGER_%' OR target_type IN ('LEDGER_ENTRY', 'LEDGER_BOOK');
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_action_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_action_check CHECK (action IN (
    'INVITATION_CREATED', 'INVITATION_CANCELLED', 'INVITATION_ACCEPTED',
    'JOIN_REQUEST_APPROVED', 'JOIN_REQUEST_DECLINED', 'JOIN_REQUEST_CHANGES_REQUESTED',
    'MEMBER_ROLE_CHANGED', 'MEMBER_JOB_TITLE_CHANGED', 'MEMBER_SUSPENDED',
    'MEMBER_REACTIVATED', 'MEMBER_REMOVED', 'REQUEST_ASSIGNED', 'REQUEST_UNASSIGNED',
    'CONTACT_OWNER_CHANGED', 'CONTACTS_MERGED', 'CONTACTS_IMPORTED', 'CONTACTS_EXPORTED',
    'APPLICATION_VIEWED', 'APPLICATION_FILE_VIEWED',
    'VENUE_BOOKING_CONFIRMED', 'VENUE_BOOKING_DECLINED', 'VENUE_BOOKING_CANCELLED',
    'VENUE_BOOKING_TIME_PROPOSED', 'VENUE_BOOKING_COMPLETED', 'VENUE_BOOKING_NO_SHOW'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION',
                           'VENUE_BOOKING'));

DROP TABLE IF EXISTS ledger_attachments;
DROP TABLE IF EXISTS ledger_entry_notes;
DROP TABLE IF EXISTS ledger_entries;
DROP FUNCTION IF EXISTS ledger_entries_guard();
DROP TABLE IF EXISTS ledger_categories;
