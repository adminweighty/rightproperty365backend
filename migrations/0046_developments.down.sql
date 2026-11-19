-- Reverts 0046. Projects, their phases, unit types, photo rows and listing
-- links are DELETED (uploaded photo objects stay in the object storage,
-- unreferenced), and so are the administrators' audit records about them
-- and the development_amenities reference rows. Business enquiries are kept
-- without their project reference.

DELETE FROM admin_audit_events WHERE target_type = 'DEVELOPMENT';
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW', 'LISTING_PACKAGE', 'ORGANISATION',
                           'LEGAL_DOCUMENT_VERSION', 'TAX_RULE'));

DROP INDEX IF EXISTS business_enquiries_development_idx;
ALTER TABLE business_enquiries DROP CONSTRAINT IF EXISTS business_enquiries_development_snapshot;
ALTER TABLE business_enquiries DROP COLUMN IF EXISTS development_unit_type_name;
ALTER TABLE business_enquiries DROP COLUMN IF EXISTS development_slug;
ALTER TABLE business_enquiries DROP COLUMN IF EXISTS development_market;
ALTER TABLE business_enquiries DROP COLUMN IF EXISTS development_name;
ALTER TABLE business_enquiries DROP COLUMN IF EXISTS development_unit_type_id;
ALTER TABLE business_enquiries DROP COLUMN IF EXISTS development_id;

DROP TABLE IF EXISTS development_listings;
DROP TABLE IF EXISTS development_photos;
DROP TABLE IF EXISTS development_unit_types;
DROP TABLE IF EXISTS development_phases;
DROP TABLE IF EXISTS developments;

DELETE FROM reference_items WHERE set_code = 'development_amenities';
ALTER TABLE reference_items DROP CONSTRAINT IF EXISTS reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'regions', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds', 'price_periods', 'contact_types', 'contact_sources',
        'maintenance_categories', 'maintenance_priorities', 'inspection_checklist_templates'));
