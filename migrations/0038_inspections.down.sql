-- Reverts 0038_inspections.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every inspection, its checklist, findings and photo rows (the
-- stored files stay in the object storage, unreferenced), the inspection
-- checklist template rows (including administrators' edits), and the
-- organisation audit records of inspection actions.

DELETE FROM organisation_audit_events
    WHERE action IN ('INSPECTION_COMPLETED', 'INSPECTION_CANCELLED') OR target_type = 'INSPECTION';
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION',
                           'VENUE_BOOKING', 'LEDGER_ENTRY', 'LEDGER_BOOK', 'TENANCY', 'MAINTENANCE_REQUEST'));
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
    'RENT_RECEIPT_RECORDED', 'RENT_DUE_WAIVED',
    'MAINTENANCE_ASSIGNED', 'MAINTENANCE_COST_RECORDED', 'MAINTENANCE_CANCELLED'));

DELETE FROM reference_items WHERE set_code = 'inspection_checklist_templates';
ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'regions', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds', 'price_periods', 'contact_types', 'contact_sources',
        'maintenance_categories', 'maintenance_priorities'));

DROP TABLE IF EXISTS inspection_photos;
DROP TRIGGER IF EXISTS inspections_freeze ON inspections;
DROP TABLE IF EXISTS inspections;
DROP FUNCTION IF EXISTS inspections_freeze();
DROP SEQUENCE IF EXISTS inspection_reference_seq;
