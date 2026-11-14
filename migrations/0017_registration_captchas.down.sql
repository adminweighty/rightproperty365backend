-- Reverts 0017_registration_captchas. Nothing of value is lost: the table
-- only holds registration questions that expire within ten minutes.
--
-- The API release that needs this migration must be rolled back first: it
-- cannot ask a question or accept a registration without the table.
DROP INDEX IF EXISTS registration_captchas_expires_at_idx;
DROP TABLE IF EXISTS registration_captchas;
