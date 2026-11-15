-- 0019_sign_in_codes: the one-time code step of signing in.
--
-- POST /v1/auth/login and POST /v1/admin/auth/login no longer start a session
-- when the password is right: they start a challenge here and email a
-- six-digit code. POST .../login/verify with that code starts the session.
--
-- A row is one challenge. Nothing secret is stored in a usable form:
--   - id is the SHA-256 of the opaque challenge id handed to the client;
--   - code_hash is an HMAC-SHA256 of the code keyed with that raw id, so the
--     code cannot be tested from this table alone.
-- A challenge belongs to one account and one scope (customer or admin), is
-- valid for ten minutes from the last code sent, can be used once, and is dead
-- after five wrong codes. delivery remembers how the customer asked for the
-- session (cookie or bearer); administrator challenges are always cookie.
--
-- Rows are short-lived. The API deletes expired rows whenever it starts a new
-- challenge. Nothing refers to this table.
--
-- Adds one new table and two indexes; no existing table or row is touched.
CREATE TABLE sign_in_challenges (
    -- SHA-256 of the challenge id given to the client.
    id           bytea PRIMARY KEY
                 CONSTRAINT sign_in_challenges_id_length CHECK (octet_length(id) = 32),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    scope        text NOT NULL CONSTRAINT sign_in_challenges_scope_check CHECK (scope IN ('customer', 'admin')),
    delivery     text NOT NULL CONSTRAINT sign_in_challenges_delivery_check CHECK (delivery IN ('cookie', 'bearer')),
    -- HMAC-SHA256 of the current code, keyed with the raw challenge id.
    code_hash    bytea NOT NULL,
    expires_at   timestamptz NOT NULL,
    -- Wrong codes so far; five kill the challenge.
    attempts     integer NOT NULL DEFAULT 0 CONSTRAINT sign_in_challenges_attempts_check CHECK (attempts >= 0),
    -- Codes sent again after the first; at most five.
    resend_count integer NOT NULL DEFAULT 0 CONSTRAINT sign_in_challenges_resend_count_check CHECK (resend_count >= 0),
    last_sent_at timestamptz NOT NULL,
    -- Set when the right code was presented (the session was started).
    consumed_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT sign_in_challenges_admin_cookie_check CHECK (scope <> 'admin' OR delivery = 'cookie')
);

-- For the clean-up of expired rows.
CREATE INDEX sign_in_challenges_expires_at_idx ON sign_in_challenges (expires_at);
-- For deleting an account's rows with the account.
CREATE INDEX sign_in_challenges_user_id_idx ON sign_in_challenges (user_id);
