-- Reverts 0042: drops the administrators' authenticators and recovery codes
-- and the second-factor columns of sign_in_challenges.

ALTER TABLE sign_in_challenges DROP CONSTRAINT IF EXISTS sign_in_challenges_mfa_admin_only;
ALTER TABLE sign_in_challenges DROP CONSTRAINT IF EXISTS sign_in_challenges_mfa_attempts_min;
ALTER TABLE sign_in_challenges DROP COLUMN IF EXISTS mfa_attempts;
ALTER TABLE sign_in_challenges DROP COLUMN IF EXISTS code_verified_at;
DROP TABLE IF EXISTS admin_mfa_recovery_codes;
DROP TABLE IF EXISTS admin_mfa_totp;
