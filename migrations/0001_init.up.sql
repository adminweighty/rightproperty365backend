-- 0001_init: foundation schema for the property-request flow.
--
-- Conventions
--   * UUID primary keys, timestamptz everywhere (values are stored in UTC).
--   * Money is an exact numeric amount plus an ISO 4217 currency code. Never float.
--   * Small, evolving vocabularies use text + CHECK; the listing lifecycle is an enum.
--   * This migration models only what POST /v1/property-requests and the outbox
--     worker need. Identity, listings content, media and billing get their own
--     migrations when those modules are built.

-- ---------------------------------------------------------------------------
-- Markets
-- ---------------------------------------------------------------------------
CREATE TABLE markets (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code          text NOT NULL CONSTRAINT markets_code_key UNIQUE
                  CONSTRAINT markets_code_format CHECK (code ~ '^[A-Z]{2}$'),
    name          text NOT NULL,
    currency_code text NOT NULL CONSTRAINT markets_currency_format CHECK (currency_code ~ '^[A-Z]{3}$'),
    timezone      text NOT NULL,
    status        text NOT NULL DEFAULT 'PLANNED'
                  CONSTRAINT markets_status_check CHECK (status IN ('ACTIVE', 'PLANNED', 'SUSPENDED')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- IDs are fixed so application seed data (internal/modules/markets) matches.
INSERT INTO markets (id, code, name, currency_code, timezone, status) VALUES
    ('0195c0de-0000-7000-8000-00000000a001', 'ZW', 'Zimbabwe',             'USD', 'Africa/Harare',       'ACTIVE'),
    ('0195c0de-0000-7000-8000-00000000a002', 'BW', 'Botswana',             'BWP', 'Africa/Gaborone',     'PLANNED'),
    ('0195c0de-0000-7000-8000-00000000a003', 'ZA', 'South Africa',         'ZAR', 'Africa/Johannesburg', 'PLANNED'),
    ('0195c0de-0000-7000-8000-00000000a004', 'AE', 'United Arab Emirates', 'AED', 'Asia/Dubai',          'PLANNED');

-- ---------------------------------------------------------------------------
-- Users, organisations, memberships (minimal: enough to own listings and
-- receive requests; credentials/sessions belong to the future identity module)
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email      text NOT NULL,
    phone      text,
    full_name  text NOT NULL,
    status     text NOT NULL DEFAULT 'ACTIVE'
               CONSTRAINT users_status_check CHECK (status IN ('ACTIVE', 'SUSPENDED', 'DELETED')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));

CREATE TABLE organisations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    market_id     uuid NOT NULL REFERENCES markets (id),
    name          text NOT NULL,
    -- Shared inbox that receives requests when a listing has no eligible agent.
    enquiry_email text,
    status        text NOT NULL DEFAULT 'ACTIVE'
                  CONSTRAINT organisations_status_check CHECK (status IN ('ACTIVE', 'SUSPENDED', 'CLOSED')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX organisations_market_idx ON organisations (market_id);

CREATE TABLE memberships (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id uuid NOT NULL REFERENCES organisations (id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role            text NOT NULL
                    CONSTRAINT memberships_role_check CHECK (role IN ('OWNER', 'ADMIN', 'AGENT')),
    status          text NOT NULL DEFAULT 'ACTIVE'
                    CONSTRAINT memberships_status_check CHECK (status IN ('ACTIVE', 'INVITED', 'REMOVED')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT memberships_org_user_key UNIQUE (organisation_id, user_id)
);
CREATE INDEX memberships_user_idx ON memberships (user_id);

-- ---------------------------------------------------------------------------
-- Listings and publication cycles
-- ---------------------------------------------------------------------------
CREATE TYPE listing_status AS ENUM (
    'DRAFT', 'PENDING_REVIEW', 'PUBLISHED', 'EXPIRED', 'PAUSED', 'SOLD', 'LET', 'CLOSED'
);

CREATE TABLE listings (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    market_id       uuid NOT NULL REFERENCES markets (id),
    title           text NOT NULL,
    status          listing_status NOT NULL DEFAULT 'DRAFT',
    price_amount    numeric(20, 4) NOT NULL CONSTRAINT listings_price_non_negative CHECK (price_amount >= 0),
    currency_code   text NOT NULL CONSTRAINT listings_currency_format CHECK (currency_code ~ '^[A-Z]{3}$'),
    -- Ownership: an organisation listing (optionally with an assigned agent)
    -- or a direct-seller listing.
    organisation_id uuid REFERENCES organisations (id),
    agent_user_id   uuid REFERENCES users (id),
    seller_user_id  uuid REFERENCES users (id),
    -- Denormalised from the current publication cycle for cheap availability checks.
    published_at    timestamptz,
    expires_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT listings_owner_check CHECK (organisation_id IS NOT NULL OR seller_user_id IS NOT NULL),
    CONSTRAINT listings_agent_requires_org CHECK (agent_user_id IS NULL OR organisation_id IS NOT NULL)
);
CREATE INDEX listings_market_status_idx ON listings (market_id, status);
CREATE INDEX listings_status_expires_idx ON listings (status, expires_at);
CREATE INDEX listings_organisation_idx ON listings (organisation_id) WHERE organisation_id IS NOT NULL;
CREATE INDEX listings_agent_idx ON listings (agent_user_id) WHERE agent_user_id IS NOT NULL;
CREATE INDEX listings_seller_idx ON listings (seller_user_id) WHERE seller_user_id IS NOT NULL;

CREATE TABLE publication_cycles (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id   uuid NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
    -- 0 for the first publication, incremented on every relist.
    relist_no    integer NOT NULL DEFAULT 0 CONSTRAINT publication_cycles_relist_non_negative CHECK (relist_no >= 0),
    published_at timestamptz NOT NULL,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT publication_cycles_listing_relist_key UNIQUE (listing_id, relist_no),
    CONSTRAINT publication_cycles_window_check CHECK (expires_at > published_at)
);

-- ---------------------------------------------------------------------------
-- Property requests (the mandatory MVP enquiry flow)
-- ---------------------------------------------------------------------------
CREATE TABLE property_requests (
    id                        uuid PRIMARY KEY,
    reference                 text NOT NULL CONSTRAINT property_requests_reference_key UNIQUE,
    market_id                 uuid NOT NULL REFERENCES markets (id),
    listing_id                uuid NOT NULL REFERENCES listings (id),
    request_type              text NOT NULL CONSTRAINT property_requests_type_check
                              CHECK (request_type IN ('ENQUIRY', 'PROPERTY_INFORMATION', 'VIEWING_REQUEST', 'CONSULTATION_REQUEST')),
    -- Internal status. Customers are only ever shown RECEIVED.
    status                    text NOT NULL CONSTRAINT property_requests_status_check
                              CHECK (status IN ('RECEIVED', 'ROUTING_EXCEPTION')),
    message                   text NOT NULL CONSTRAINT property_requests_message_length
                              CHECK (char_length(message) BETWEEN 1 AND 2000),
    preferred_contact_method  text NOT NULL CONSTRAINT property_requests_contact_method_check
                              CHECK (preferred_contact_method IN ('EMAIL', 'PHONE', 'WHATSAPP')),
    preferred_slot_at         timestamptz,
    -- The unique constraint is what makes concurrent retries safe.
    idempotency_key           text NOT NULL CONSTRAINT property_requests_idempotency_key_key UNIQUE,
    request_hash              text NOT NULL,
    -- NULL until authenticated customers exist; guests are identified by contact details.
    customer_user_id          uuid REFERENCES users (id),
    guest_name                text,
    guest_email               text,
    guest_phone               text,
    consent_privacy_version   text NOT NULL,
    consent_accepted_at       timestamptz NOT NULL,
    recipient_type            text CONSTRAINT property_requests_recipient_type_check
                              CHECK (recipient_type IN ('AGENT', 'ORGANISATION', 'SELLER')),
    recipient_user_id         uuid REFERENCES users (id),
    recipient_organisation_id uuid REFERENCES organisations (id),
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT property_requests_customer_check
        CHECK (customer_user_id IS NOT NULL OR (guest_name IS NOT NULL AND guest_email IS NOT NULL)),
    CONSTRAINT property_requests_routing_check
        CHECK ((status = 'ROUTING_EXCEPTION') = (recipient_type IS NULL))
);
CREATE INDEX property_requests_listing_idx ON property_requests (listing_id, created_at DESC);
CREATE INDEX property_requests_market_created_idx ON property_requests (market_id, created_at DESC);
CREATE INDEX property_requests_recipient_org_idx ON property_requests (recipient_organisation_id, created_at DESC)
    WHERE recipient_organisation_id IS NOT NULL;
CREATE INDEX property_requests_recipient_user_idx ON property_requests (recipient_user_id, created_at DESC)
    WHERE recipient_user_id IS NOT NULL;
CREATE INDEX property_requests_routing_exception_idx ON property_requests (created_at)
    WHERE status = 'ROUTING_EXCEPTION';

CREATE TABLE request_timeline (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id    uuid NOT NULL REFERENCES property_requests (id) ON DELETE CASCADE,
    event_type    text NOT NULL,
    actor_type    text NOT NULL CONSTRAINT request_timeline_actor_type_check
                  CHECK (actor_type IN ('CUSTOMER', 'AGENT', 'STAFF', 'SYSTEM')),
    actor_user_id uuid REFERENCES users (id),
    detail        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX request_timeline_request_idx ON request_timeline (request_id, created_at);

CREATE TABLE customer_activity (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid REFERENCES users (id),
    guest_email   text,
    activity_type text NOT NULL,
    market_id     uuid REFERENCES markets (id),
    listing_id    uuid REFERENCES listings (id),
    request_id    uuid REFERENCES property_requests (id) ON DELETE CASCADE,
    metadata      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT customer_activity_subject_check CHECK (user_id IS NOT NULL OR guest_email IS NOT NULL)
);
CREATE INDEX customer_activity_user_idx ON customer_activity (user_id, created_at DESC) WHERE user_id IS NOT NULL;
CREATE INDEX customer_activity_guest_email_idx ON customer_activity (lower(guest_email), created_at DESC)
    WHERE guest_email IS NOT NULL;
CREATE INDEX customer_activity_request_idx ON customer_activity (request_id) WHERE request_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Transactional outbox (consumed by cmd/worker, at-least-once)
-- ---------------------------------------------------------------------------
CREATE TABLE outbox_events (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type     text NOT NULL,
    aggregate_type text NOT NULL,
    aggregate_id   uuid NOT NULL,
    payload        jsonb NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- When the event is next due (creation time, then pushed out by retry backoff).
    available_at   timestamptz NOT NULL DEFAULT now(),
    attempts       integer NOT NULL DEFAULT 0,
    last_error     text,
    -- Lease held by a worker; expires on its own if the worker dies.
    locked_until   timestamptz,
    locked_by      text,
    processed_at   timestamptz,
    dead_letter_at timestamptz
);
CREATE INDEX outbox_events_pending_idx ON outbox_events (available_at, created_at)
    WHERE processed_at IS NULL AND dead_letter_at IS NULL;
CREATE INDEX outbox_events_dead_letter_idx ON outbox_events (dead_letter_at)
    WHERE dead_letter_at IS NOT NULL;
CREATE INDEX outbox_events_aggregate_idx ON outbox_events (aggregate_type, aggregate_id);
