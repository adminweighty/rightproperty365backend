-- Reverts 0007_reference_data. Destroys all reference data, including any
-- rows an administrator added or changed.
--
-- The fixed vocabularies come back as CHECK constraints, added NOT VALID so
-- the revert succeeds even if rows using a newer value exist: such rows are
-- left alone, and only new or updated rows are checked.
ALTER TABLE user_account_roles DROP CONSTRAINT IF EXISTS user_account_roles_no_platform_roles;
ALTER TABLE user_account_roles ADD CONSTRAINT user_account_roles_role_check
    CHECK (role IN ('CONSUMER', 'TENANT', 'PRIVATE_SELLER', 'LANDLORD', 'HOST', 'DEVELOPER',
                    'SERVICE_PROVIDER', 'AGENT', 'AGENCY_STAFF', 'AGENCY_ADMIN', 'AGENCY_OWNER')) NOT VALID;
ALTER TABLE directory_profiles ADD CONSTRAINT directory_profiles_kind_check
    CHECK (kind IN ('agencies', 'agents', 'developers', 'property-managers', 'hosts', 'service-providers')) NOT VALID;
ALTER TABLE business_enquiries ADD CONSTRAINT business_enquiries_contact_method_check
    CHECK (preferred_contact_method IN ('EMAIL', 'PHONE', 'WHATSAPP')) NOT VALID;
ALTER TABLE property_requests ADD CONSTRAINT property_requests_contact_method_check
    CHECK (preferred_contact_method IN ('EMAIL', 'PHONE', 'WHATSAPP')) NOT VALID;
ALTER TABLE property_requests ADD CONSTRAINT property_requests_type_check
    CHECK (request_type IN ('ENQUIRY', 'PROPERTY_INFORMATION', 'VIEWING_REQUEST', 'CONSULTATION_REQUEST')) NOT VALID;

DROP TABLE IF EXISTS reference_items;
