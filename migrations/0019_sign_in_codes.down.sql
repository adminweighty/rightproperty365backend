-- Reverts 0019_sign_in_codes. Nothing of value is lost: the table only holds
-- sign-in challenges that expire within ten minutes.
--
-- The API release that needs this migration must be rolled back first: it
-- cannot sign anyone in without the table.
DROP INDEX IF EXISTS sign_in_challenges_user_id_idx;
DROP INDEX IF EXISTS sign_in_challenges_expires_at_idx;
DROP TABLE IF EXISTS sign_in_challenges;
