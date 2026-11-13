-- 0004_session_scope: sessions are either customer sessions (public login) or
-- admin sessions (administrator login, platform-role accounts only).
--
-- Existing sessions were all issued by the public login, so they are
-- backfilled as 'customer'; the default is then dropped so every new session
-- must state its scope. Admin sessions are cookie-only: the application never
-- issues one as a bearer token and the constraint makes that impossible to
-- store. Their shorter lifetime (12 h absolute / 30 min idle by default) is
-- enforced by the application from expires_at and last_seen_at.
ALTER TABLE sessions
    ADD COLUMN scope text NOT NULL DEFAULT 'customer'
    CONSTRAINT sessions_scope_check CHECK (scope IN ('customer', 'admin'));
ALTER TABLE sessions ALTER COLUMN scope DROP DEFAULT;
ALTER TABLE sessions
    ADD CONSTRAINT sessions_admin_cookie_only_check CHECK (scope <> 'admin' OR delivery = 'cookie');
