-- Reverts 0009_onboarding. Destroys every onboarding application, its
-- history and its document records. The files in object storage are NOT
-- removed by this and become orphans: delete the "onboarding/" prefix by hand.
-- Organisations, memberships and profiles created by approvals are kept.
DROP INDEX IF EXISTS directory_profiles_public_idx;
CREATE INDEX directory_profiles_public_idx ON directory_profiles (kind, sponsored DESC, name) WHERE status = 'ACTIVE';

DROP TRIGGER IF EXISTS onboarding_history_no_update ON onboarding_history;
DROP TABLE IF EXISTS onboarding_history;
DROP FUNCTION IF EXISTS onboarding_history_immutable();
DROP TABLE IF EXISTS onboarding_documents;
DROP TABLE IF EXISTS onboarding_applications;
DROP TABLE IF EXISTS onboarding_role_documents;
DROP TABLE IF EXISTS onboarding_role_outcomes;

DELETE FROM reference_items WHERE set_code = 'onboarding_documents';
ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'cities', 'suburbs', 'listing_features'));
