-- Reverts 0030_agent_crm.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every contact book (contacts, their emails, phones, tags, notes,
-- reminders, activity and links), the organisation audit records of contact
-- reassignments, merges, imports and exports, and the contact_types and
-- contact_sources reference rows. CONTACT_REMINDER_DUE events already in the
-- outbox stay.

DROP TABLE IF EXISTS contact_links;
DROP TABLE IF EXISTS contact_events;
DROP TABLE IF EXISTS contact_reminders;
DROP TABLE IF EXISTS contact_notes;
DROP TABLE IF EXISTS contact_tags;
DROP TABLE IF EXISTS contact_phones;
DROP TABLE IF EXISTS contact_emails;
DROP TABLE IF EXISTS contacts;

DELETE FROM organisation_audit_events
WHERE action IN ('CONTACT_OWNER_CHANGED', 'CONTACTS_MERGED', 'CONTACTS_IMPORTED', 'CONTACTS_EXPORTED')
   OR target_type IN ('CONTACT', 'CONTACT_BOOK');
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_action_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_action_check CHECK (action IN (
    'INVITATION_CREATED', 'INVITATION_CANCELLED', 'INVITATION_ACCEPTED',
    'JOIN_REQUEST_APPROVED', 'JOIN_REQUEST_DECLINED', 'JOIN_REQUEST_CHANGES_REQUESTED',
    'MEMBER_ROLE_CHANGED', 'MEMBER_JOB_TITLE_CHANGED', 'MEMBER_SUSPENDED',
    'MEMBER_REACTIVATED', 'MEMBER_REMOVED', 'REQUEST_ASSIGNED', 'REQUEST_UNASSIGNED'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST'));

DELETE FROM reference_items WHERE set_code IN ('contact_types', 'contact_sources');
ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'regions', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds', 'price_periods'));
