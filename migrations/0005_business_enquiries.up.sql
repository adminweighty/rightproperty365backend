-- 0005_business_enquiries: directory profiles and the enquiries sent to them.
--
-- A directory profile (agency, agent, developer, property manager, host or
-- service provider) can be contacted ONLY through an enquiry. That is why
-- directory_profiles has no phone, email or address columns at all: there is
-- nothing contact-like to leak. The customer's contact details live on the
-- enquiry and are read only by the recipient's inbox.

-- ---------------------------------------------------------------------------
-- Directory profiles
-- ---------------------------------------------------------------------------
CREATE TABLE directory_profiles (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    market_id       uuid NOT NULL REFERENCES markets (id),
    -- Public URL segment pair, e.g. service-providers / sample-plumbers.
    kind            text NOT NULL CONSTRAINT directory_profiles_kind_check
                    CHECK (kind IN ('agencies', 'agents', 'developers', 'property-managers', 'hosts', 'service-providers')),
    slug            text NOT NULL CONSTRAINT directory_profiles_slug_format
                    CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(slug) <= 80),
    name            text NOT NULL,
    -- The organisation that owns the profile and receives its enquiries. For
    -- an agent profile this is the agent's agency.
    organisation_id uuid NOT NULL REFERENCES organisations (id),
    -- Agent profiles only: the person the profile is about.
    agent_user_id   uuid REFERENCES users (id),
    -- Only ACTIVE profiles accept enquiries.
    status          text NOT NULL DEFAULT 'PENDING_VERIFICATION'
                    CONSTRAINT directory_profiles_status_check
                    CHECK (status IN ('ACTIVE', 'PENDING_VERIFICATION', 'SUSPENDED')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT directory_profiles_kind_slug_key UNIQUE (kind, slug),
    CONSTRAINT directory_profiles_agent_check CHECK ((kind = 'agents') = (agent_user_id IS NOT NULL))
);
CREATE INDEX directory_profiles_organisation_idx ON directory_profiles (organisation_id);
CREATE INDEX directory_profiles_agent_idx ON directory_profiles (agent_user_id) WHERE agent_user_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Business enquiries
-- ---------------------------------------------------------------------------
CREATE TABLE business_enquiries (
    id                        uuid PRIMARY KEY,
    reference                 text NOT NULL CONSTRAINT business_enquiries_reference_key UNIQUE,
    market_id                 uuid NOT NULL REFERENCES markets (id),
    profile_id                uuid NOT NULL REFERENCES directory_profiles (id),
    -- Internal status. Customers are only ever shown RECEIVED.
    status                    text NOT NULL CONSTRAINT business_enquiries_status_check
                              CHECK (status IN ('RECEIVED', 'ROUTING_EXCEPTION')),
    -- One of the recipient's listed services, when the customer picked one.
    service                   text CONSTRAINT business_enquiries_service_length
                              CHECK (char_length(service) BETWEEN 1 AND 120),
    message                   text NOT NULL CONSTRAINT business_enquiries_message_length
                              CHECK (char_length(message) BETWEEN 1 AND 2000),
    preferred_contact_method  text NOT NULL CONSTRAINT business_enquiries_contact_method_check
                              CHECK (preferred_contact_method IN ('EMAIL', 'PHONE', 'WHATSAPP')),
    -- The unique constraint is what makes concurrent retries safe. The key
    -- space is separate from property_requests.
    idempotency_key           text NOT NULL CONSTRAINT business_enquiries_idempotency_key_key UNIQUE,
    request_hash              text NOT NULL,
    customer_user_id          uuid REFERENCES users (id),
    guest_name                text,
    guest_email               text,
    guest_phone               text,
    consent_privacy_version   text NOT NULL,
    consent_accepted_at       timestamptz NOT NULL,
    -- Routing: always an organisation; for an agent profile also the agent.
    recipient_organisation_id uuid REFERENCES organisations (id),
    recipient_user_id         uuid REFERENCES users (id),
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT business_enquiries_customer_check
        CHECK (customer_user_id IS NOT NULL OR (guest_name IS NOT NULL AND guest_email IS NOT NULL)),
    CONSTRAINT business_enquiries_routing_check
        CHECK ((status = 'ROUTING_EXCEPTION') = (recipient_organisation_id IS NULL)),
    CONSTRAINT business_enquiries_recipient_user_check
        CHECK (recipient_user_id IS NULL OR recipient_organisation_id IS NOT NULL)
);
CREATE INDEX business_enquiries_profile_idx ON business_enquiries (profile_id, created_at DESC);
CREATE INDEX business_enquiries_recipient_org_idx ON business_enquiries (recipient_organisation_id, created_at DESC)
    WHERE recipient_organisation_id IS NOT NULL;
CREATE INDEX business_enquiries_recipient_user_idx ON business_enquiries (recipient_user_id, created_at DESC)
    WHERE recipient_user_id IS NOT NULL;
CREATE INDEX business_enquiries_customer_idx ON business_enquiries (customer_user_id, created_at DESC)
    WHERE customer_user_id IS NOT NULL;
CREATE INDEX business_enquiries_routing_exception_idx ON business_enquiries (created_at)
    WHERE status = 'ROUTING_EXCEPTION';

CREATE TABLE business_enquiry_timeline (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    enquiry_id    uuid NOT NULL REFERENCES business_enquiries (id) ON DELETE CASCADE,
    event_type    text NOT NULL,
    actor_type    text NOT NULL CONSTRAINT business_enquiry_timeline_actor_type_check
                  CHECK (actor_type IN ('CUSTOMER', 'AGENT', 'STAFF', 'SYSTEM')),
    actor_user_id uuid REFERENCES users (id),
    detail        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX business_enquiry_timeline_enquiry_idx ON business_enquiry_timeline (enquiry_id, created_at);

-- Customer activity gains a link to the enquiry it records (the existing
-- request_id link is for property requests).
ALTER TABLE customer_activity
    ADD COLUMN business_enquiry_id uuid REFERENCES business_enquiries (id) ON DELETE CASCADE;
CREATE INDEX customer_activity_business_enquiry_idx ON customer_activity (business_enquiry_id)
    WHERE business_enquiry_id IS NOT NULL;
