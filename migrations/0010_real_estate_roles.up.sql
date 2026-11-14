-- 0010_real_estate_roles
--
-- Standard real estate role terminology from the functional specification.
-- Existing role CODES are kept (the web and the mobile app depend on them);
-- labels, descriptions and groups change, and four roles are added:
-- SALES_AGENT, LETTINGS_AGENT, PROPERTY_MANAGER and SOLICITOR (a law firm or
-- practice: organisation kind LAW_FIRM and a public "solicitors" directory).
-- Organisation kinds become a reference set (organisation_kinds) with labels.
--
-- What changes in meaning:
--   * LANDLORD is now an individual property owner, like the property seller
--     (PRIVATE_SELLER): approving it creates no organisation and no profile.
--   * The businesses that used to be created for LANDLORD (organisation kind
--     LANDLORD with a profile in "property-managers") are property managers:
--     every organisation of kind LANDLORD becomes kind PROPERTY_MANAGER, and
--     its owners' LANDLORD account role (and their applications for it)
--     becomes PROPERTY_MANAGER. The kind value LANDLORD stays allowed by the
--     constraint, but nothing creates it any more.
--   * LANDLORD applications that were NOT approved stay LANDLORD and fall
--     under the new, individual-owner requirements: their business details
--     no longer apply and they now need PROOF_OF_OWNERSHIP. One that is
--     already SUBMITTED cannot be approved until it is sent back for changes.
--
-- The document requirements are RESET to the new baseline (they are
-- placeholders awaiting business and legal confirmation).

-- ---------------------------------------------------------------------------
-- Account roles: groups, labels, descriptions, order; three new roles.
-- Existing rows keep their `active` flag.
-- ---------------------------------------------------------------------------
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'CONSUMER', 'Prospective Buyer', NULL, 'Looking for a property to buy.', 10, NULL, NULL, 'PROPERTY_SEEKERS', 'Property Seekers', 1, false)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'TENANT', 'Prospective Tenant', NULL, 'Looking for a property to rent or lease.', 20, NULL, NULL, 'PROPERTY_SEEKERS', 'Property Seekers', 1, false)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'AGENT', 'Estate Agent', NULL, 'Markets properties, manages enquiries, arranges viewings and handles sales or lettings.', 30, NULL, NULL, 'REAL_ESTATE_PROFESSIONALS', 'Real Estate Professionals', 2, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'SALES_AGENT', 'Sales Agent', NULL, 'An estate agent who specialises in properties for sale.', 40, NULL, NULL, 'REAL_ESTATE_PROFESSIONALS', 'Real Estate Professionals', 2, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'LETTINGS_AGENT', 'Lettings Agent', NULL, 'An estate agent who specialises in rentals and leases.', 50, NULL, NULL, 'REAL_ESTATE_PROFESSIONALS', 'Real Estate Professionals', 2, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'PROPERTY_MANAGER', 'Property Manager', NULL, 'Manages rental properties, tenants and viewings on behalf of owners.', 60, NULL, NULL, 'REAL_ESTATE_PROFESSIONALS', 'Real Estate Professionals', 2, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'SOLICITOR', 'Solicitor', NULL, 'A property lawyer or conveyancer who handles transfers, leases and other legal work for buyers, sellers, landlords and tenants.', 70, NULL, NULL, 'REAL_ESTATE_PROFESSIONALS', 'Real Estate Professionals', 2, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'AGENCY_OWNER', 'Agency Principal', NULL, 'Registers a real estate agency or brokerage and is responsible for its account.', 80, NULL, NULL, 'REAL_ESTATE_BUSINESSES', 'Real Estate Businesses', 3, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'AGENCY_ADMIN', 'Agency Administrator', NULL, 'Manages an agency''s agents, listings and enquiries on the principal''s behalf.', 90, NULL, NULL, 'REAL_ESTATE_BUSINESSES', 'Real Estate Businesses', 3, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'AGENCY_STAFF', 'Agency Staff', NULL, 'Works at an agency in a support role.', 100, NULL, NULL, 'REAL_ESTATE_BUSINESSES', 'Real Estate Businesses', 3, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'LANDLORD', 'Landlord', NULL, 'Owns residential or commercial property offered for rent or lease.', 110, NULL, NULL, 'PROPERTY_OWNERS', 'Property Owners', 4, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'PRIVATE_SELLER', 'Property Seller (Vendor)', NULL, 'Owns a property offered for sale. Also called a vendor.', 120, NULL, NULL, 'PROPERTY_OWNERS', 'Property Owners', 4, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'HOST', 'Host', NULL, 'Offers short stays or venues for hire.', 130, NULL, NULL, 'OTHER_PROPERTY_BUSINESSES', 'Other property businesses', 5, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'DEVELOPER', 'Property Developer', NULL, 'Builds and sells new developments.', 140, NULL, NULL, 'OTHER_PROPERTY_BUSINESSES', 'Other property businesses', 5, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('account_roles', 'SERVICE_PROVIDER', 'Property Service Provider', NULL, 'Offers a property-related trade or professional service.', 150, NULL, NULL, 'OTHER_PROPERTY_BUSINESSES', 'Other property businesses', 5, true)
    ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order,
        group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label, group_sort_order = EXCLUDED.group_sort_order,
        requires_verification = EXCLUDED.requires_verification, updated_at = now();

-- Directory headings (the kind codes, which are URL slugs, are unchanged).
INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id, parent_code, group_code, group_label, group_sort_order, requires_verification) VALUES ('directory_kinds', 'solicitors', 'Solicitors', 'Solicitor', 'Property lawyers and conveyancers for transfers, leases and title work.', 50, NULL, NULL, NULL, NULL, NULL, NULL);
UPDATE reference_items SET label = 'Real Estate Agencies', singular_label = 'Real Estate Agency', sort_order = 10, updated_at = now() WHERE set_code = 'directory_kinds' AND code = 'agencies';
UPDATE reference_items SET label = 'Estate Agents', singular_label = 'Estate Agent', sort_order = 20, updated_at = now() WHERE set_code = 'directory_kinds' AND code = 'agents';
UPDATE reference_items SET label = 'Property Developers', singular_label = 'Property Developer', sort_order = 30, updated_at = now() WHERE set_code = 'directory_kinds' AND code = 'developers';
UPDATE reference_items SET label = 'Property Managers', singular_label = 'Property Manager', sort_order = 40, updated_at = now() WHERE set_code = 'directory_kinds' AND code = 'property-managers';
UPDATE reference_items SET label = 'Solicitors', singular_label = 'Solicitor', sort_order = 50, updated_at = now() WHERE set_code = 'directory_kinds' AND code = 'solicitors';
UPDATE reference_items SET label = 'Hosts and Venues', singular_label = 'Host', sort_order = 60, updated_at = now() WHERE set_code = 'directory_kinds' AND code = 'hosts';
UPDATE reference_items SET label = 'Property Services', singular_label = 'Property Service Provider', sort_order = 70, updated_at = now() WHERE set_code = 'directory_kinds' AND code = 'service-providers';

-- Organisation kinds as reference data, so clients show labels without hard-coding them.
ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds'));
INSERT INTO reference_items (set_code, code, label, sort_order) VALUES ('organisation_kinds', 'AGENCY', 'Real Estate Agency', 10);
INSERT INTO reference_items (set_code, code, label, sort_order) VALUES ('organisation_kinds', 'LAW_FIRM', 'Law Firm', 20);
INSERT INTO reference_items (set_code, code, label, sort_order) VALUES ('organisation_kinds', 'PROPERTY_MANAGER', 'Property Manager', 30);
INSERT INTO reference_items (set_code, code, label, sort_order) VALUES ('organisation_kinds', 'HOST', 'Host', 40);
INSERT INTO reference_items (set_code, code, label, sort_order) VALUES ('organisation_kinds', 'DEVELOPER', 'Property Developer', 50);
INSERT INTO reference_items (set_code, code, label, sort_order) VALUES ('organisation_kinds', 'SERVICE_PROVIDER', 'Property Service Provider', 60);

-- A new document type for solicitors, and clearer wording for proof of ownership.
INSERT INTO reference_items (set_code, code, label, description, sort_order) VALUES ('onboarding_documents', 'PRACTISING_CERTIFICATE', 'Practising certificate', 'Current practising certificate from the Law Society.', 90);
UPDATE reference_items SET description = 'Title deed or other proof that you own the property you intend to sell or let.', updated_at = now()
WHERE set_code = 'onboarding_documents' AND code = 'PROOF_OF_OWNERSHIP';

-- ---------------------------------------------------------------------------
-- Organisation kind PROPERTY_MANAGER; existing LANDLORD organisations move to it.
-- ---------------------------------------------------------------------------
ALTER TABLE organisations DROP CONSTRAINT organisations_kind_check;
ALTER TABLE organisations ADD CONSTRAINT organisations_kind_check
    CHECK (kind IN ('AGENCY', 'LANDLORD', 'HOST', 'DEVELOPER', 'SERVICE_PROVIDER', 'PROPERTY_MANAGER', 'LAW_FIRM'));

-- The owners of those organisations: LANDLORD -> PROPERTY_MANAGER, for the
-- account role and for their applications for it. (Nobody can hold
-- PROPERTY_MANAGER yet, so the unique keys cannot collide.)
UPDATE onboarding_applications a SET role = 'PROPERTY_MANAGER', updated_at = now()
WHERE a.role = 'LANDLORD'
  AND EXISTS (SELECT 1 FROM memberships m JOIN organisations o ON o.id = m.organisation_id
              WHERE m.user_id = a.user_id AND m.role = 'OWNER' AND o.kind = 'LANDLORD');
UPDATE user_account_roles r SET role = 'PROPERTY_MANAGER', updated_at = now()
WHERE r.role = 'LANDLORD'
  AND EXISTS (SELECT 1 FROM memberships m JOIN organisations o ON o.id = m.organisation_id
              WHERE m.user_id = r.user_id AND m.role = 'OWNER' AND o.kind = 'LANDLORD');
UPDATE organisations SET kind = 'PROPERTY_MANAGER', updated_at = now() WHERE kind = 'LANDLORD';

-- ---------------------------------------------------------------------------
-- Agent specialism, recorded on the public agent profile.
-- ---------------------------------------------------------------------------
ALTER TABLE directory_profiles
    ADD COLUMN specialism text
    CONSTRAINT directory_profiles_specialism_check CHECK (specialism IN ('SALES', 'LETTINGS'))
    CONSTRAINT directory_profiles_specialism_agents_only CHECK (specialism IS NULL OR kind = 'agents');

-- ---------------------------------------------------------------------------
-- What approving each role creates
-- ---------------------------------------------------------------------------
ALTER TABLE onboarding_role_outcomes DROP CONSTRAINT onboarding_role_outcomes_kind_check;
ALTER TABLE onboarding_role_outcomes ADD CONSTRAINT onboarding_role_outcomes_kind_check
    CHECK (organisation_kind IN ('AGENCY', 'HOST', 'DEVELOPER', 'SERVICE_PROVIDER', 'PROPERTY_MANAGER', 'LAW_FIRM', 'LANDLORD'));
ALTER TABLE onboarding_role_outcomes
    -- Recorded on the agent profile an approval creates.
    ADD COLUMN specialism text
    CONSTRAINT onboarding_role_outcomes_specialism_check CHECK (specialism IN ('SALES', 'LETTINGS'))
    CONSTRAINT onboarding_role_outcomes_specialism_agents_only CHECK (specialism IS NULL OR directory_kind = 'agents');

INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('AGENT', 'AGENCY_MEMBER', NULL, 'agents', 'AGENT', NULL)
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('SALES_AGENT', 'AGENCY_MEMBER', NULL, 'agents', 'AGENT', 'SALES')
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('LETTINGS_AGENT', 'AGENCY_MEMBER', NULL, 'agents', 'AGENT', 'LETTINGS')
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('PROPERTY_MANAGER', 'ORGANISATION', 'PROPERTY_MANAGER', 'property-managers', 'OWNER', NULL)
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('SOLICITOR', 'ORGANISATION', 'LAW_FIRM', 'solicitors', 'OWNER', NULL)
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('AGENCY_OWNER', 'ORGANISATION', 'AGENCY', 'agencies', 'OWNER', NULL)
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('AGENCY_ADMIN', 'AGENCY_MEMBER', NULL, NULL, 'MANAGER', NULL)
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('AGENCY_STAFF', 'AGENCY_MEMBER', NULL, NULL, 'STAFF', NULL)
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('LANDLORD', 'NONE', NULL, NULL, NULL, NULL)
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('PRIVATE_SELLER', 'NONE', NULL, NULL, NULL, NULL)
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('HOST', 'ORGANISATION', 'HOST', 'hosts', 'OWNER', NULL)
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('DEVELOPER', 'ORGANISATION', 'DEVELOPER', 'developers', 'OWNER', NULL)
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role, specialism) VALUES ('SERVICE_PROVIDER', 'ORGANISATION', 'SERVICE_PROVIDER', 'service-providers', 'OWNER', NULL)
    ON CONFLICT (role) DO UPDATE SET outcome = EXCLUDED.outcome, organisation_kind = EXCLUDED.organisation_kind, directory_kind = EXCLUDED.directory_kind,
        membership_role = EXCLUDED.membership_role, specialism = EXCLUDED.specialism, updated_at = now();

-- ---------------------------------------------------------------------------
-- Documents per role: reset to the new baseline (PLACEHOLDERS).
-- ---------------------------------------------------------------------------
DELETE FROM onboarding_role_documents;
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENT', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENT', 'ESTATE_AGENTS_COUNCIL_CERTIFICATE', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SALES_AGENT', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SALES_AGENT', 'ESTATE_AGENTS_COUNCIL_CERTIFICATE', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('LETTINGS_AGENT', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('LETTINGS_AGENT', 'ESTATE_AGENTS_COUNCIL_CERTIFICATE', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('PROPERTY_MANAGER', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('PROPERTY_MANAGER', 'BUSINESS_REGISTRATION', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('PROPERTY_MANAGER', 'PROOF_OF_ADDRESS', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('PROPERTY_MANAGER', 'TAX_CLEARANCE', false);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SOLICITOR', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SOLICITOR', 'BUSINESS_REGISTRATION', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SOLICITOR', 'PROOF_OF_ADDRESS', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SOLICITOR', 'TAX_CLEARANCE', false);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SOLICITOR', 'PRACTISING_CERTIFICATE', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_OWNER', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_OWNER', 'BUSINESS_REGISTRATION', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_OWNER', 'PROOF_OF_ADDRESS', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_OWNER', 'TAX_CLEARANCE', false);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_OWNER', 'ESTATE_AGENTS_COUNCIL_CERTIFICATE', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_ADMIN', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_ADMIN', 'PROOF_OF_EMPLOYMENT', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_STAFF', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_STAFF', 'PROOF_OF_EMPLOYMENT', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('LANDLORD', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('LANDLORD', 'PROOF_OF_OWNERSHIP', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('PRIVATE_SELLER', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('PRIVATE_SELLER', 'PROOF_OF_OWNERSHIP', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('HOST', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('HOST', 'BUSINESS_REGISTRATION', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('HOST', 'PROOF_OF_ADDRESS', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('HOST', 'TAX_CLEARANCE', false);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('DEVELOPER', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('DEVELOPER', 'BUSINESS_REGISTRATION', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('DEVELOPER', 'PROOF_OF_ADDRESS', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('DEVELOPER', 'TAX_CLEARANCE', false);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SERVICE_PROVIDER', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SERVICE_PROVIDER', 'BUSINESS_REGISTRATION', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SERVICE_PROVIDER', 'PROOF_OF_ADDRESS', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SERVICE_PROVIDER', 'TAX_CLEARANCE', false);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('SERVICE_PROVIDER', 'TRADE_LICENCE', false);
