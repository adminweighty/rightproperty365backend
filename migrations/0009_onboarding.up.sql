-- 0009_onboarding
--
-- Onboarding for professional accounts: the application behind a role chosen
-- at sign-up, its private documents, the administrator's decision and an
-- immutable history of every transition.
--
-- What is database-driven here (BR-001): the document types (reference set
-- onboarding_documents), which documents each role needs
-- (onboarding_role_documents) and what approving each role creates
-- (onboarding_role_outcomes).
--
-- The document rows seeded below are PLACEHOLDERS. Which documents each
-- market really requires is a business and legal decision that has not been
-- taken.

-- ---------------------------------------------------------------------------
-- Reference set: onboarding_documents
-- ---------------------------------------------------------------------------
ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents'));

INSERT INTO reference_items (set_code, code, label, description, sort_order) VALUES ('onboarding_documents', 'PROOF_OF_IDENTITY', 'Proof of identity', 'National ID, passport or driver''s licence of the person applying.', 10);
INSERT INTO reference_items (set_code, code, label, description, sort_order) VALUES ('onboarding_documents', 'BUSINESS_REGISTRATION', 'Business registration', 'Certificate of incorporation or business registration.', 20);
INSERT INTO reference_items (set_code, code, label, description, sort_order) VALUES ('onboarding_documents', 'PROOF_OF_ADDRESS', 'Proof of address', 'A utility bill or bank statement for the business address, less than three months old.', 30);
INSERT INTO reference_items (set_code, code, label, description, sort_order) VALUES ('onboarding_documents', 'TAX_CLEARANCE', 'Tax clearance', 'A current tax clearance certificate.', 40);
INSERT INTO reference_items (set_code, code, label, description, sort_order) VALUES ('onboarding_documents', 'ESTATE_AGENTS_COUNCIL_CERTIFICATE', 'Estate Agents Council certificate', 'Current registration or compliance certificate from the Estate Agents Council.', 50);
INSERT INTO reference_items (set_code, code, label, description, sort_order) VALUES ('onboarding_documents', 'PROOF_OF_EMPLOYMENT', 'Proof of employment', 'A letter from the agency confirming that you work there.', 60);
INSERT INTO reference_items (set_code, code, label, description, sort_order) VALUES ('onboarding_documents', 'TRADE_LICENCE', 'Trade licence', 'Licence or certification for your trade, if your trade has one.', 70);
INSERT INTO reference_items (set_code, code, label, description, sort_order) VALUES ('onboarding_documents', 'PROOF_OF_OWNERSHIP', 'Proof of ownership', 'Title deed or other proof that you own the property you intend to sell.', 80);

-- ---------------------------------------------------------------------------
-- What approving each role creates
-- ---------------------------------------------------------------------------
CREATE TABLE onboarding_role_outcomes (
    -- An account role (reference set account_roles). CONSUMER and TENANT have
    -- no row: they need no onboarding.
    role              text PRIMARY KEY
                      CONSTRAINT onboarding_role_outcomes_no_platform_roles
                      CHECK (role NOT IN ('SUPER_ADMIN', 'PLATFORM_ADMIN', 'MARKET_ADMIN', 'FINANCE_ADMIN', 'CONTENT_ADMIN', 'MODERATOR', 'SUPPORT')),
    outcome           text NOT NULL CONSTRAINT onboarding_role_outcomes_outcome_check
                      CHECK (outcome IN ('ORGANISATION', 'AGENCY_MEMBER', 'NONE')),
    -- ORGANISATION: the kind of the organisation created.
    organisation_kind text CONSTRAINT onboarding_role_outcomes_kind_check
                      CHECK (organisation_kind IN ('AGENCY', 'LANDLORD', 'HOST', 'DEVELOPER', 'SERVICE_PROVIDER')),
    -- The public directory profile created (NULL = none).
    directory_kind    text,
    -- The membership created (NULL = none).
    membership_role   text CONSTRAINT onboarding_role_outcomes_membership_check
                      CHECK (membership_role IN ('OWNER', 'MANAGER', 'AGENT', 'STAFF')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT onboarding_role_outcomes_shape CHECK (
        (outcome = 'ORGANISATION' AND organisation_kind IS NOT NULL AND membership_role IS NOT NULL)
        OR (outcome = 'AGENCY_MEMBER' AND organisation_kind IS NULL AND membership_role IS NOT NULL)
        OR (outcome = 'NONE' AND organisation_kind IS NULL AND directory_kind IS NULL AND membership_role IS NULL))
);

INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role) VALUES ('PRIVATE_SELLER', 'NONE', NULL, NULL, NULL);
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role) VALUES ('LANDLORD', 'ORGANISATION', 'LANDLORD', 'property-managers', 'OWNER');
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role) VALUES ('HOST', 'ORGANISATION', 'HOST', 'hosts', 'OWNER');
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role) VALUES ('DEVELOPER', 'ORGANISATION', 'DEVELOPER', 'developers', 'OWNER');
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role) VALUES ('SERVICE_PROVIDER', 'ORGANISATION', 'SERVICE_PROVIDER', 'service-providers', 'OWNER');
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role) VALUES ('AGENT', 'AGENCY_MEMBER', NULL, 'agents', 'AGENT');
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role) VALUES ('AGENCY_STAFF', 'AGENCY_MEMBER', NULL, NULL, 'STAFF');
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role) VALUES ('AGENCY_ADMIN', 'AGENCY_MEMBER', NULL, NULL, 'MANAGER');
INSERT INTO onboarding_role_outcomes (role, outcome, organisation_kind, directory_kind, membership_role) VALUES ('AGENCY_OWNER', 'ORGANISATION', 'AGENCY', 'agencies', 'OWNER');

-- ---------------------------------------------------------------------------
-- Which documents each role needs (required) or may attach (optional)
-- ---------------------------------------------------------------------------
CREATE TABLE onboarding_role_documents (
    role          text NOT NULL REFERENCES onboarding_role_outcomes (role) ON DELETE CASCADE,
    -- A code of the reference set onboarding_documents.
    document_code text NOT NULL,
    required      boolean NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (role, document_code)
);

INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('PRIVATE_SELLER', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('PRIVATE_SELLER', 'PROOF_OF_OWNERSHIP', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('LANDLORD', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('LANDLORD', 'BUSINESS_REGISTRATION', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('LANDLORD', 'PROOF_OF_ADDRESS', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('LANDLORD', 'TAX_CLEARANCE', false);
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
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENT', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENT', 'ESTATE_AGENTS_COUNCIL_CERTIFICATE', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_STAFF', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_STAFF', 'PROOF_OF_EMPLOYMENT', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_ADMIN', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_ADMIN', 'PROOF_OF_EMPLOYMENT', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_OWNER', 'PROOF_OF_IDENTITY', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_OWNER', 'BUSINESS_REGISTRATION', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_OWNER', 'PROOF_OF_ADDRESS', true);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_OWNER', 'TAX_CLEARANCE', false);
INSERT INTO onboarding_role_documents (role, document_code, required) VALUES ('AGENCY_OWNER', 'ESTATE_AGENTS_COUNCIL_CERTIFICATE', true);

-- ---------------------------------------------------------------------------
-- Applications
-- ---------------------------------------------------------------------------
CREATE TABLE onboarding_applications (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role                text NOT NULL
                        CONSTRAINT onboarding_applications_no_platform_roles
                        CHECK (role NOT IN ('SUPER_ADMIN', 'PLATFORM_ADMIN', 'MARKET_ADMIN', 'FINANCE_ADMIN', 'CONTENT_ADMIN', 'MODERATOR', 'SUPPORT')),
    status              text NOT NULL DEFAULT 'DRAFT' CONSTRAINT onboarding_applications_status_check
                        CHECK (status IN ('DRAFT', 'SUBMITTED', 'CHANGES_REQUESTED', 'APPROVED', 'REJECTED')),
    -- Details. Which ones apply depends on the role's outcome; the API validates them.
    business_name       text CONSTRAINT onboarding_applications_business_name_length CHECK (char_length(business_name) BETWEEN 2 AND 120),
    tagline             text CONSTRAINT onboarding_applications_tagline_length CHECK (char_length(tagline) <= 140),
    description         text CONSTRAINT onboarding_applications_description_length CHECK (char_length(description) <= 2000),
    city_code           text,
    areas               text[] NOT NULL DEFAULT '{}',
    services            text[] NOT NULL DEFAULT '{}',
    trade_code          text,
    -- PRIVATE: for the verification team only. Never copied to a public profile.
    registration_number text,
    -- PRIVATE: E.164. For the verification team only. Never copied to a public profile.
    contact_phone       text CONSTRAINT onboarding_applications_phone_format CHECK (contact_phone ~ '^\+[1-9][0-9]{7,14}$'),
    agency_slug         text,
    agency_name         text,
    job_title           text,
    bio                 text CONSTRAINT onboarding_applications_bio_length CHECK (char_length(bio) <= 1000),
    -- The market the application belongs to (from its city or its agency);
    -- scopes what a MARKET_ADMIN may review.
    market_code         text,
    -- The last decision (reason for a rejection or a request for changes).
    decision_reason     text,
    decided_at          timestamptz,
    submitted_at        timestamptz,
    -- The public profile an approval created.
    profile_kind        text,
    profile_slug        text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
-- One current application per user and role. A REJECTED one is final and
-- stays as history; the person may start a new one.
CREATE UNIQUE INDEX onboarding_applications_current_key ON onboarding_applications (user_id, role) WHERE status <> 'REJECTED';
CREATE INDEX onboarding_applications_user_idx ON onboarding_applications (user_id, role, created_at DESC);
CREATE INDEX onboarding_applications_queue_idx ON onboarding_applications (status, submitted_at, created_at);

-- ---------------------------------------------------------------------------
-- Documents. The file itself is in private object storage under object_key,
-- a random key with no user input. Nothing here is ever public.
-- ---------------------------------------------------------------------------
CREATE TABLE onboarding_documents (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid NOT NULL REFERENCES onboarding_applications (id) ON DELETE CASCADE,
    document_code  text NOT NULL,
    -- Sanitised display name; never part of the storage key.
    file_name      text NOT NULL CONSTRAINT onboarding_documents_file_name_length CHECK (char_length(file_name) BETWEEN 1 AND 100),
    -- Decided by inspecting the content, not by the client's header.
    content_type   text NOT NULL CONSTRAINT onboarding_documents_content_type_check
                   CHECK (content_type IN ('application/pdf', 'image/jpeg', 'image/png')),
    size_bytes     bigint NOT NULL CONSTRAINT onboarding_documents_size_check CHECK (size_bytes BETWEEN 1 AND 10485760),
    sha256         text NOT NULL CONSTRAINT onboarding_documents_sha256_format CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    object_key     text NOT NULL CONSTRAINT onboarding_documents_object_key_key UNIQUE,
    uploaded_at    timestamptz NOT NULL DEFAULT now(),
    -- One document per type; uploading the same type again replaces it.
    CONSTRAINT onboarding_documents_type_key UNIQUE (application_id, document_code)
);

-- ---------------------------------------------------------------------------
-- History: one row per transition, never changed afterwards.
-- ---------------------------------------------------------------------------
CREATE TABLE onboarding_history (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid NOT NULL REFERENCES onboarding_applications (id) ON DELETE CASCADE,
    from_status    text NOT NULL,
    to_status      text NOT NULL,
    actor_user_id  uuid REFERENCES users (id) ON DELETE SET NULL,
    reason         text,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX onboarding_history_application_idx ON onboarding_history (application_id, created_at);

-- Rows are immutable. (They disappear only with their application, when the
-- account is deleted; losing the actor to an account deletion is the one
-- column change allowed.)
CREATE FUNCTION onboarding_history_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.application_id IS DISTINCT FROM OLD.application_id
       OR NEW.from_status IS DISTINCT FROM OLD.from_status
       OR NEW.to_status IS DISTINCT FROM OLD.to_status
       OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (NEW.actor_user_id IS DISTINCT FROM OLD.actor_user_id AND NEW.actor_user_id IS NOT NULL) THEN
        RAISE EXCEPTION 'onboarding_history rows are immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER onboarding_history_no_update BEFORE UPDATE ON onboarding_history
    FOR EACH ROW EXECUTE FUNCTION onboarding_history_immutable();

-- ---------------------------------------------------------------------------
-- Backfill: every role that is already waiting for verification gets a DRAFT
-- application. Roles that are already ACTIVE (granted before onboarding
-- existed) are left alone and have no application.
-- ---------------------------------------------------------------------------
INSERT INTO onboarding_applications (user_id, role, status, created_at, updated_at)
SELECT r.user_id, r.role, 'DRAFT', now(), now()
FROM user_account_roles r
JOIN onboarding_role_outcomes o ON o.role = r.role
WHERE r.status = 'PENDING_VERIFICATION';

-- Only published AND verified profiles are public.
DROP INDEX IF EXISTS directory_profiles_public_idx;
CREATE INDEX directory_profiles_public_idx ON directory_profiles (kind, sponsored DESC, name) WHERE status = 'ACTIVE' AND verified;
