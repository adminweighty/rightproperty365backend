-- Reverts 0004_session_scope. Admin sessions are deleted first: without the
-- scope column they would be indistinguishable from customer sessions and
-- would outlive their intended 12-hour limit.
DELETE FROM sessions WHERE scope = 'admin';
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_admin_cookie_only_check;
ALTER TABLE sessions DROP COLUMN IF EXISTS scope;
