-- Reverts 0023_agreements.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every agreement, its recipients, fields, links and audit trail, the
-- reminder records and the saved signatures; leases keep their agreement_id
-- values without the foreign key. The PDF and signature image objects in the
-- object storage (agreements/..., agreementsignatures/...,
-- usersignatures/...) are NOT deleted by this script.
ALTER TABLE leases DROP CONSTRAINT IF EXISTS leases_agreement_id_fkey;

DROP TABLE IF EXISTS user_signatures;
DROP TABLE IF EXISTS agreement_jobs;
DROP TRIGGER IF EXISTS agreement_events_no_update ON agreement_events;
DROP FUNCTION IF EXISTS agreement_events_immutable();
DROP INDEX IF EXISTS agreement_events_agreement_idx;
DROP TABLE IF EXISTS agreement_events;
DROP INDEX IF EXISTS agreement_links_agreement_idx;
DROP TABLE IF EXISTS agreement_links;
DROP INDEX IF EXISTS agreement_fields_agreement_idx;
DROP TABLE IF EXISTS agreement_fields;
DROP INDEX IF EXISTS agreement_recipients_email_idx;
DROP TABLE IF EXISTS agreement_recipients;
DROP INDEX IF EXISTS agreements_lease_idx;
DROP INDEX IF EXISTS agreements_original_sha256_idx;
DROP INDEX IF EXISTS agreements_final_sha256_idx;
DROP INDEX IF EXISTS agreements_status_idx;
DROP INDEX IF EXISTS agreements_sender_idx;
DROP INDEX IF EXISTS agreements_organisation_idx;
DROP TABLE IF EXISTS agreements;
DROP SEQUENCE IF EXISTS agreement_reference_seq;
