-- 0003_account_roles: the roles a person chooses for themselves at registration.
--
-- An account role is SELF-DECLARED and informational. It is deliberately a
-- separate table from memberships and platform_role_assignments:
--   * it creates no organisation and no membership;
--   * it grants no access to any organisation's data;
--   * no authorisation check reads it.
-- Platform administrator roles (SUPER_ADMIN, ...) are not in the vocabulary and
-- therefore cannot be stored here at all.
--
-- CONSUMER and TENANT are stored ACTIVE; every professional role is stored
-- PENDING_VERIFICATION until an approval workflow exists. REJECTED and REVOKED
-- are reserved for that workflow; nothing sets them yet.
CREATE TABLE user_account_roles (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       text NOT NULL CONSTRAINT user_account_roles_role_check
               CHECK (role IN ('CONSUMER', 'TENANT', 'PRIVATE_SELLER', 'LANDLORD', 'HOST', 'DEVELOPER',
                               'SERVICE_PROVIDER', 'AGENT', 'AGENCY_STAFF', 'AGENCY_ADMIN', 'AGENCY_OWNER')),
    status     text NOT NULL CONSTRAINT user_account_roles_status_check
               CHECK (status IN ('ACTIVE', 'PENDING_VERIFICATION', 'REJECTED', 'REVOKED')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_account_roles_user_role_key UNIQUE (user_id, role)
);
CREATE INDEX user_account_roles_pending_idx ON user_account_roles (role, created_at)
    WHERE status = 'PENDING_VERIFICATION';

-- Accounts created before this migration never chose a role: record them as
-- consumers, which is what everyone implicitly is.
INSERT INTO user_account_roles (user_id, role, status)
SELECT id, 'CONSUMER', 'ACTIVE' FROM users;
