-- 0042: administrator MFA with an authenticator app (API 0.40.0, FR-001).
--
--   * admin_mfa_totp: at most one authenticator per administrator.
--       - secret_sealed: the TOTP secret (20 random bytes) encrypted with
--         AES-256-GCM under MFA_ENCRYPTION_KEY, bound to the user id
--         (version byte || nonce || ciphertext). Never stored in clear.
--       - confirmed_at: NULL while the setup is pending (the administrator
--         has not yet entered a code from the app); set by confirm. A
--         pending row is replaced by a new setup and useless after 15
--         minutes.
--       - last_used_step: the newest 30-second step whose code was
--         accepted. A code of that step or an earlier one is refused
--         (replay protection); only ever increases.
--   * admin_mfa_recovery_codes: ten one-time codes per administrator, stored
--     as Argon2id hashes (PHC strings); used_at is set when one is used.
--     Regenerating deletes the old set.
--   * sign_in_challenges gains the second-factor step of the administrator
--     sign-in: code_verified_at (the emailed code was accepted and the
--     challenge now waits for the authenticator or a recovery code) and
--     mfa_attempts (wrong second-factor codes; five lock the challenge).
--
-- Account deletion anonymises users rows (never deletes them), so the ON
-- DELETE CASCADE below never fires in practice: the accounts module deletes
-- these rows explicitly (identity.DeleteAdminMFATx).
--
-- Constraint names: every constraint is named explicitly and never takes
-- Postgres's <table>_<column>_check form.
--
-- Safe on a database with data: new tables and new columns with defaults
-- (re-runnable).

CREATE TABLE IF NOT EXISTS admin_mfa_totp (
    user_id        uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    secret_sealed  bytea NOT NULL,
    confirmed_at   timestamptz,
    last_used_step bigint NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    CONSTRAINT admin_mfa_totp_sealed_size CHECK (octet_length(secret_sealed) BETWEEN 29 AND 512),
    CONSTRAINT admin_mfa_totp_step_not_negative CHECK (last_used_step >= 0)
);

CREATE TABLE IF NOT EXISTS admin_mfa_recovery_codes (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  text NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL,
    CONSTRAINT admin_mfa_recovery_codes_hash_size CHECK (char_length(code_hash) BETWEEN 20 AND 400)
);

CREATE INDEX IF NOT EXISTS admin_mfa_recovery_codes_user_idx ON admin_mfa_recovery_codes (user_id);

ALTER TABLE sign_in_challenges ADD COLUMN IF NOT EXISTS code_verified_at timestamptz;
ALTER TABLE sign_in_challenges ADD COLUMN IF NOT EXISTS mfa_attempts integer NOT NULL DEFAULT 0;
ALTER TABLE sign_in_challenges DROP CONSTRAINT IF EXISTS sign_in_challenges_mfa_attempts_min;
ALTER TABLE sign_in_challenges ADD CONSTRAINT sign_in_challenges_mfa_attempts_min CHECK (mfa_attempts >= 0);
-- Only administrator challenges have a second factor.
ALTER TABLE sign_in_challenges DROP CONSTRAINT IF EXISTS sign_in_challenges_mfa_admin_only;
ALTER TABLE sign_in_challenges ADD CONSTRAINT sign_in_challenges_mfa_admin_only CHECK (code_verified_at IS NULL OR scope = 'admin');
