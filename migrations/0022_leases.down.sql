-- Reverts 0022_leases.
--
-- The API release that needs this migration must be rolled back first.
-- Needs 0023 to be reverted first. LOSES: every lease template and its audit
-- trail, every lease and its audit trail, and the record of the stored PDFs.
-- The PDF objects in the object storage (keys leases/...) are NOT deleted by this script: export
-- what must be kept, then remove them from the bucket by hand.
DROP TRIGGER IF EXISTS lease_template_events_no_update ON lease_template_events;
DROP FUNCTION IF EXISTS lease_template_events_immutable();
DROP TRIGGER IF EXISTS lease_events_no_update ON lease_events;
DROP FUNCTION IF EXISTS lease_events_immutable();

DROP INDEX IF EXISTS lease_events_lease_idx;
DROP TABLE IF EXISTS lease_events;
DROP TABLE IF EXISTS lease_documents;
DROP INDEX IF EXISTS leases_status_idx;
DROP INDEX IF EXISTS leases_created_by_idx;
DROP INDEX IF EXISTS leases_organisation_idx;
DROP TABLE IF EXISTS leases;

DROP INDEX IF EXISTS lease_template_events_template_idx;
DROP TABLE IF EXISTS lease_template_events;
DROP INDEX IF EXISTS lease_templates_market_status_idx;
DROP INDEX IF EXISTS lease_templates_one_approved_key;
DROP INDEX IF EXISTS lease_templates_one_draft_key;
DROP TABLE IF EXISTS lease_templates;

DROP SEQUENCE IF EXISTS lease_reference_seq;
