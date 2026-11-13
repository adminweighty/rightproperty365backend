-- 0002_identity: credentials, email verification, password reset, sessions and
-- the role model (organisation kind, membership role + market, platform roles).
--
-- 0001 already has users, organisations, memberships and
-- property_requests.customer_user_id; this migration alters those tables
-- rather than duplicating them. Vocabularies stay text + CHECK (see the
-- conventions in 0001).
--
-- Secrets: only password HASHES (Argon2id, PHC string) and SHA-256 digests of
-- session / verification / reset tokens are stored. Raw tokens never are.

-- ---------------------------------------------------------------------------
-- Users: credentials and verification
-- ---------------------------------------------------------------------------
ALTER TABLE users
    -- NULL for accounts that cannot sign in with a password (e.g. rows created
    -- by operations for direct sellers before they register).
    ADD COLUMN password_hash     text,
    ADD COLUMN email_verified_at timestamptz,
    ADD COLUMN marketing_opt_in  boolean NOT NULL DEFAULT false;

-- Consent given at registration (and any later re-acceptance). Append-only.
CREATE TABLE user_consents (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    consent_type text NOT NULL CONSTRAINT user_consents_type_check CHECK (consent_type IN ('PRIVACY_NOTICE')),
    version      text NOT NULL,
    accepted_at  timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX user_consents_user_idx ON user_consents (user_id, accepted_at DESC);

-- ---------------------------------------------------------------------------
-- Organisations: kind
-- ---------------------------------------------------------------------------
-- Existing organisations are backfilled as AGENCY (the only kind 0001 could
-- express); the default is then dropped so new rows must state their kind.
ALTER TABLE organisations
    ADD COLUMN kind text NOT NULL DEFAULT 'AGENCY'
    CONSTRAINT organisations_kind_check
    CHECK (kind IN ('AGENCY', 'LANDLORD', 'HOST', 'DEVELOPER', 'SERVICE_PROVIDER'));
ALTER TABLE organisations ALTER COLUMN kind DROP DEFAULT;

-- ---------------------------------------------------------------------------
-- Memberships: role vocabulary OWNER/MANAGER/AGENT/STAFF, scoped to a market
-- ---------------------------------------------------------------------------
ALTER TABLE memberships DROP CONSTRAINT memberships_role_check;
-- 0001's ADMIN becomes MANAGER.
UPDATE memberships SET role = 'MANAGER' WHERE role = 'ADMIN';
ALTER TABLE memberships
    ADD CONSTRAINT memberships_role_check CHECK (role IN ('OWNER', 'MANAGER', 'AGENT', 'STAFF'));

ALTER TABLE memberships ADD COLUMN market_id uuid REFERENCES markets (id);
UPDATE memberships m SET market_id = o.market_id FROM organisations o WHERE o.id = m.organisation_id;
ALTER TABLE memberships ALTER COLUMN market_id SET NOT NULL;
CREATE INDEX memberships_market_idx ON memberships (market_id);

-- ---------------------------------------------------------------------------
-- Platform roles (held by the user, not by an organisation)
-- ---------------------------------------------------------------------------
CREATE TABLE platform_role_assignments (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       text NOT NULL CONSTRAINT platform_role_assignments_role_check
               CHECK (role IN ('SUPER_ADMIN', 'PLATFORM_ADMIN', 'MARKET_ADMIN', 'FINANCE_ADMIN',
                               'CONTENT_ADMIN', 'MODERATOR', 'SUPPORT')),
    -- Set for MARKET_ADMIN and only for MARKET_ADMIN.
    market_id  uuid REFERENCES markets (id),
    granted_by uuid REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT platform_role_assignments_market_scope_check
        CHECK ((role = 'MARKET_ADMIN') = (market_id IS NOT NULL))
);
CREATE UNIQUE INDEX platform_role_assignments_global_key
    ON platform_role_assignments (user_id, role) WHERE market_id IS NULL;
CREATE UNIQUE INDEX platform_role_assignments_market_key
    ON platform_role_assignments (user_id, role, market_id) WHERE market_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Sessions (opaque tokens; 30-day absolute and 14-day idle expiry are
-- enforced by the application from expires_at and last_seen_at)
-- ---------------------------------------------------------------------------
CREATE TABLE sessions (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- SHA-256 of the token.
    token_hash   bytea NOT NULL CONSTRAINT sessions_token_hash_key UNIQUE
                 CONSTRAINT sessions_token_hash_length CHECK (octet_length(token_hash) = 32),
    delivery     text NOT NULL CONSTRAINT sessions_delivery_check CHECK (delivery IN ('cookie', 'bearer')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz
);
CREATE INDEX sessions_user_active_idx ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- ---------------------------------------------------------------------------
-- One-time tokens (single use; stored as SHA-256)
-- ---------------------------------------------------------------------------
CREATE TABLE email_verification_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL CONSTRAINT email_verification_tokens_token_hash_key UNIQUE
               CONSTRAINT email_verification_tokens_token_hash_length CHECK (octet_length(token_hash) = 32),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX email_verification_tokens_user_idx ON email_verification_tokens (user_id) WHERE used_at IS NULL;

CREATE TABLE password_reset_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL CONSTRAINT password_reset_tokens_token_hash_key UNIQUE
               CONSTRAINT password_reset_tokens_token_hash_length CHECK (octet_length(token_hash) = 32),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX password_reset_tokens_user_idx ON password_reset_tokens (user_id) WHERE used_at IS NULL;

-- ---------------------------------------------------------------------------
-- Property requests: customer_user_id already exists (0001). Index it so a
-- signed-in customer's own requests can be listed.
-- ---------------------------------------------------------------------------
CREATE INDEX property_requests_customer_idx ON property_requests (customer_user_id, created_at DESC)
    WHERE customer_user_id IS NOT NULL;
