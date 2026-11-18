-- Reverts 0031_questionnaires_applications.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every questionnaire (form) and its versions, every listing's
-- application intake, every application with its answers, files (the
-- database rows; the stored objects stay in the object storage), timeline
-- and internal notes, the CRM links to applications, and the organisation
-- audit records of application views. Application outbox events already
-- written stay.

DELETE FROM contact_links WHERE kind = 'APPLICATION';
ALTER TABLE contact_links DROP CONSTRAINT IF EXISTS contact_links_kind_check;
ALTER TABLE contact_links ADD CONSTRAINT contact_links_kind_check CHECK (kind IN (
    'LISTING', 'PROPERTY_REQUEST', 'BUSINESS_ENQUIRY', 'APPOINTMENT'));

DELETE FROM organisation_audit_events
WHERE action IN ('APPLICATION_VIEWED', 'APPLICATION_FILE_VIEWED') OR target_type = 'APPLICATION';
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_action_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_action_check CHECK (action IN (
    'INVITATION_CREATED', 'INVITATION_CANCELLED', 'INVITATION_ACCEPTED',
    'JOIN_REQUEST_APPROVED', 'JOIN_REQUEST_DECLINED', 'JOIN_REQUEST_CHANGES_REQUESTED',
    'MEMBER_ROLE_CHANGED', 'MEMBER_JOB_TITLE_CHANGED', 'MEMBER_SUSPENDED',
    'MEMBER_REACTIVATED', 'MEMBER_REMOVED', 'REQUEST_ASSIGNED', 'REQUEST_UNASSIGNED',
    'CONTACT_OWNER_CHANGED', 'CONTACTS_MERGED', 'CONTACTS_IMPORTED', 'CONTACTS_EXPORTED'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK'));

DROP TABLE IF EXISTS application_notes;
DROP TABLE IF EXISTS application_events;
DROP TABLE IF EXISTS application_file_purges;
DROP TABLE IF EXISTS application_files;
DROP TABLE IF EXISTS applications;
DROP SEQUENCE IF EXISTS application_reference_seq;
DROP TABLE IF EXISTS listing_application_intake;
DROP TRIGGER IF EXISTS forms_frozen_after_submission ON forms;
DROP TABLE IF EXISTS forms;
DROP FUNCTION IF EXISTS forms_frozen_after_submission();
