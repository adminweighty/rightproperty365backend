-- Reverts 0002_identity. Destroys all credentials, sessions, tokens, consent
-- records and platform role assignments.
--
-- LOSSY: the 0001 membership vocabulary has no MANAGER or STAFF, so MANAGER
-- becomes ADMIN and STAFF becomes AGENT.
DROP INDEX IF EXISTS property_requests_customer_idx;

DROP TABLE IF EXISTS password_reset_tokens;
DROP TABLE IF EXISTS email_verification_tokens;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS platform_role_assignments;

DROP INDEX IF EXISTS memberships_market_idx;
ALTER TABLE memberships DROP COLUMN IF EXISTS market_id;
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_role_check;
UPDATE memberships SET role = 'ADMIN' WHERE role = 'MANAGER';
UPDATE memberships SET role = 'AGENT' WHERE role = 'STAFF';
ALTER TABLE memberships
    ADD CONSTRAINT memberships_role_check CHECK (role IN ('OWNER', 'ADMIN', 'AGENT'));

ALTER TABLE organisations DROP COLUMN IF EXISTS kind;

DROP TABLE IF EXISTS user_consents;

ALTER TABLE users
    DROP COLUMN IF EXISTS marketing_opt_in,
    DROP COLUMN IF EXISTS email_verified_at,
    DROP COLUMN IF EXISTS password_hash;
