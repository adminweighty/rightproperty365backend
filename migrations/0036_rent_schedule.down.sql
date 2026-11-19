-- Reverts 0036_rent_schedule.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every rent due, receipt and allocation, and the organisation audit
-- records of rent actions. The RENT_RECEIVED ledger entries the receipts
-- wrote stay in the ledger (posted entries are never deleted) with their
-- tenancy_id, so the restored CHECK that keeps tenancy_id NULL is added
-- NOT VALID: it holds for new rows only.

DELETE FROM organisation_audit_events WHERE action IN ('RENT_RECEIPT_RECORDED', 'RENT_DUE_WAIVED');
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
    'LEDGER_ENTRY_UNRECONCILED', 'LEDGER_ATTACHMENT_ADDED', 'LEDGER_ATTACHMENT_VIEWED', 'LEDGER_EXPORTED',
    'TENANCY_CREATED', 'TENANCY_ACTIVATED', 'TENANCY_ENDED', 'TENANCY_DELETED'));

DROP INDEX IF EXISTS ledger_entries_tenancy_idx;
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_tenancy_type_check;
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_tenancy_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_tenancy_check CHECK (tenancy_id IS NULL) NOT VALID;

DROP TABLE IF EXISTS rent_allocations;
DROP TABLE IF EXISTS rent_receipts;
DROP TABLE IF EXISTS rent_dues;
