-- Reverts 0037_maintenance.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every maintenance request, its timeline, photos (the stored files
-- stay in the object storage, unreferenced) and cost rows, the maintenance
-- reference rows, and the organisation audit records of maintenance
-- actions. The EXPENSE ledger entries the costs wrote stay in the ledger
-- (posted entries are never deleted); their property link is dropped with
-- the column.

DELETE FROM organisation_audit_events
    WHERE action IN ('MAINTENANCE_ASSIGNED', 'MAINTENANCE_COST_RECORDED', 'MAINTENANCE_CANCELLED') OR target_type = 'MAINTENANCE_REQUEST';
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION',
                           'VENUE_BOOKING', 'LEDGER_ENTRY', 'LEDGER_BOOK', 'TENANCY'));
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
    'TENANCY_CREATED', 'TENANCY_ACTIVATED', 'TENANCY_ENDED', 'TENANCY_DELETED',
    'RENT_RECEIPT_RECORDED', 'RENT_DUE_WAIVED'));

DELETE FROM reference_items WHERE set_code IN ('maintenance_categories', 'maintenance_priorities');
ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'regions', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds', 'price_periods', 'contact_types', 'contact_sources'));

DROP TABLE IF EXISTS maintenance_costs;
DROP TABLE IF EXISTS maintenance_photos;
DROP TABLE IF EXISTS maintenance_events;
DROP TABLE IF EXISTS maintenance_requests;
DROP SEQUENCE IF EXISTS maintenance_reference_seq;

-- The guard of 0033 (without property_id), then the column.
CREATE OR REPLACE FUNCTION ledger_entries_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'ledger entries are never deleted: post a reversal instead'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.organisation_id IS DISTINCT FROM OLD.organisation_id
       OR NEW.book_user_id IS DISTINCT FROM OLD.book_user_id
       OR NEW.type IS DISTINCT FROM OLD.type
       OR NEW.direction IS DISTINCT FROM OLD.direction
       OR NEW.amount IS DISTINCT FROM OLD.amount
       OR NEW.currency IS DISTINCT FROM OLD.currency
       OR NEW.method IS DISTINCT FROM OLD.method
       OR NEW.category IS DISTINCT FROM OLD.category
       OR NEW.effective_date IS DISTINCT FROM OLD.effective_date
       OR NEW.recorded_at IS DISTINCT FROM OLD.recorded_at
       OR NEW.author_user_id IS DISTINCT FROM OLD.author_user_id
       OR NEW.author_name IS DISTINCT FROM OLD.author_name
       OR NEW.reversal_of IS DISTINCT FROM OLD.reversal_of
       OR NEW.correction_of IS DISTINCT FROM OLD.correction_of
       OR NEW.settles_entry_id IS DISTINCT FROM OLD.settles_entry_id
       OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.branch IS DISTINCT FROM OLD.branch
       OR NEW.description IS DISTINCT FROM OLD.description
       OR NEW.reference IS DISTINCT FROM OLD.reference
       OR NEW.tenancy_id IS DISTINCT FROM OLD.tenancy_id
       OR NEW.gateway_reference IS DISTINCT FROM OLD.gateway_reference
       OR (OLD.reversed_by IS NOT NULL AND NEW.reversed_by IS DISTINCT FROM OLD.reversed_by)
       OR (OLD.corrected_by IS NOT NULL AND NEW.corrected_by IS DISTINCT FROM OLD.corrected_by)
       OR (OLD.transfer_pair IS NOT NULL AND NEW.transfer_pair IS DISTINCT FROM OLD.transfer_pair) THEN
        RAISE EXCEPTION 'posted ledger entries are immutable: post a reversal or a correcting entry instead'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP INDEX IF EXISTS ledger_entries_property_idx;
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_property_type_rule;
ALTER TABLE ledger_entries DROP COLUMN IF EXISTS property_id;
