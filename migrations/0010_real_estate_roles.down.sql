-- Reverts 0010_real_estate_roles as far as it can.
--
-- LOSSY: organisations that became PROPERTY_MANAGER go back to LANDLORD and
-- their owners' PROPERTY_MANAGER role goes back to LANDLORD, including any
-- property manager approved AFTER 0010. Account roles SALES_AGENT and
-- LETTINGS_AGENT become AGENT (dropped where the person already holds AGENT),
-- and the specialism on agent profiles is lost. The old labels, groups and
-- document requirements are NOT restored: re-run cmd/seed or edit the rows.
UPDATE onboarding_applications a SET role = 'LANDLORD' WHERE a.role = 'PROPERTY_MANAGER'
  AND NOT EXISTS (SELECT 1 FROM onboarding_applications b WHERE b.user_id = a.user_id AND b.role = 'LANDLORD' AND b.status <> 'REJECTED' AND a.status <> 'REJECTED');
DELETE FROM onboarding_applications WHERE role IN ('PROPERTY_MANAGER', 'SALES_AGENT', 'LETTINGS_AGENT', 'SOLICITOR');
UPDATE user_account_roles r SET role = 'LANDLORD' WHERE r.role = 'PROPERTY_MANAGER'
  AND NOT EXISTS (SELECT 1 FROM user_account_roles x WHERE x.user_id = r.user_id AND x.role = 'LANDLORD');
UPDATE user_account_roles r SET role = 'AGENT' WHERE r.role IN ('SALES_AGENT', 'LETTINGS_AGENT')
  AND NOT EXISTS (SELECT 1 FROM user_account_roles x WHERE x.user_id = r.user_id AND x.role IN ('AGENT', 'SALES_AGENT', 'LETTINGS_AGENT') AND x.id <> r.id);
DELETE FROM user_account_roles WHERE role IN ('PROPERTY_MANAGER', 'SALES_AGENT', 'LETTINGS_AGENT', 'SOLICITOR');
UPDATE organisations SET kind = 'LANDLORD' WHERE kind = 'PROPERTY_MANAGER';
ALTER TABLE organisations DROP CONSTRAINT organisations_kind_check;
ALTER TABLE organisations ADD CONSTRAINT organisations_kind_check
    CHECK (kind IN ('AGENCY', 'LANDLORD', 'HOST', 'DEVELOPER', 'SERVICE_PROVIDER'));

DELETE FROM onboarding_role_documents WHERE role IN ('SALES_AGENT', 'LETTINGS_AGENT', 'PROPERTY_MANAGER', 'SOLICITOR');
DELETE FROM onboarding_role_outcomes WHERE role IN ('SALES_AGENT', 'LETTINGS_AGENT', 'PROPERTY_MANAGER', 'SOLICITOR');
UPDATE onboarding_role_outcomes SET outcome = 'ORGANISATION', organisation_kind = 'LANDLORD', directory_kind = 'property-managers', membership_role = 'OWNER'
WHERE role = 'LANDLORD';
ALTER TABLE onboarding_role_outcomes DROP COLUMN specialism;
ALTER TABLE onboarding_role_outcomes DROP CONSTRAINT onboarding_role_outcomes_kind_check;
ALTER TABLE onboarding_role_outcomes ADD CONSTRAINT onboarding_role_outcomes_kind_check
    CHECK (organisation_kind IN ('AGENCY', 'LANDLORD', 'HOST', 'DEVELOPER', 'SERVICE_PROVIDER'));

ALTER TABLE directory_profiles DROP COLUMN specialism;
DELETE FROM reference_items WHERE set_code = 'account_roles' AND code IN ('SALES_AGENT', 'LETTINGS_AGENT', 'PROPERTY_MANAGER', 'SOLICITOR');
-- Solicitor organisations, their profiles and the SOLICITOR role are KEPT as
-- data (organisations of kind LAW_FIRM would violate the old constraint, so
-- the constraint above is only restored when none exist: remove them first).
DELETE FROM reference_items WHERE set_code = 'directory_kinds' AND code = 'solicitors';
DELETE FROM reference_items WHERE set_code = 'organisation_kinds';
ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents'));
DELETE FROM reference_items WHERE set_code = 'onboarding_documents' AND code = 'PRACTISING_CERTIFICATE';
