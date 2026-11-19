-- Reverts 0035_properties_tenancies.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every property, unit, tenancy and tenancy party, and the
-- organisation audit records of tenancy actions.

DELETE FROM organisation_audit_events WHERE action LIKE 'TENANCY_%' OR target_type = 'TENANCY';
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_action_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_action_check CHECK (action IN (
    'INVITATION_CREATED', 'INVITATION_CANCELLED', 'INVITATION_ACCEPTED',
    'JOIN_REQUEST_APPROVED', 'JOIN_REQUEST_DECLINED', 'JOIN_REQUEST_CHANGES_REQUESTED',
    'MEMBER_ROLE_CHANGED', 'MEMBER_JOB_TITLE_CHANGED', 'MEMBER_SUSPENDED',
    'MEMBER_REACTIVATED', 'MEMBER_REMOVED', 'REQUEST_ASSIGNED', 'REQUEST_UNASSIGNED',
    'CONTACT_OWNER_CHANGED', 'CONTACTS_MERGED', 'CONTACTS_IMPORTED', 'CONTACTS_EXPORTED',
    'APPLICATION_VIEWED', 'APPLICATION_FILE_VIEWED',
    'VENUE_BOOKING_CONFIRMED', 'VENUE_BOOKING_DECLINED', 'VENUE_BOOKING_CANCELLED',
    'VENUE_BOOKING_TIME_PROPOSED', 'VENUE_BOOKING_COMPLETED', 'VENUE_BOOKING_NO_SHOW',
    'LEDGER_ENTRY_CREATED', 'LEDGER_ENTRY_REVERSED', 'LEDGER_ENTRY_CORRECTED', 'LEDGER_ENTRY_RECONCILED',
    'LEDGER_ENTRY_UNRECONCILED', 'LEDGER_ATTACHMENT_ADDED', 'LEDGER_ATTACHMENT_VIEWED', 'LEDGER_EXPORTED'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION',
                           'VENUE_BOOKING', 'LEDGER_ENTRY', 'LEDGER_BOOK'));

ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_tenancy_ref_fkey;

DROP TABLE IF EXISTS tenancy_parties;
DROP TABLE IF EXISTS tenancies;
DROP TABLE IF EXISTS property_units;
DROP TABLE IF EXISTS properties;
