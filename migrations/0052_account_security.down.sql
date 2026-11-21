DROP TABLE IF EXISTS email_change_tokens;
ALTER TABLE sessions DROP COLUMN IF EXISTS client;
