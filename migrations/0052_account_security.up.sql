-- 0052: account security (API 0.50.0, Handover section 14).
--
--   * sessions.client: a short label of the browser and system recorded at
--     sign-in ("Chrome on Windows"), shown in the list of signed-in devices.
--     No IP address is stored.
--   * email_change_tokens: a pending change of email address, confirmed by a
--     single-use link sent to the NEW address (24 hours). A new request
--     replaces the previous one.

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS client text
    CONSTRAINT sessions_client_length CHECK (client IS NULL OR char_length(client) <= 120);

CREATE TABLE IF NOT EXISTS email_change_tokens (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    new_email  text NOT NULL CONSTRAINT email_change_tokens_email_length CHECK (char_length(new_email) BETWEEN 3 AND 254),
    token_hash bytea NOT NULL CONSTRAINT email_change_tokens_hash_key UNIQUE
               CONSTRAINT email_change_tokens_hash_length CHECK (octet_length(token_hash) = 32),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    used_at    timestamptz
);
CREATE INDEX IF NOT EXISTS email_change_tokens_user_idx ON email_change_tokens (user_id) WHERE used_at IS NULL;
