-- 0017_registration_captchas: the questions of the registration form.
--
-- GET /v1/auth/captcha asks a small arithmetic question; POST
-- /v1/auth/register must answer one. A row is one question that was asked.
-- Only what is needed to judge an answer is kept: the opaque id given to the
-- client, a hash of the answer (never the answer or the question), when the
-- question expires (ten minutes after it was asked) and when it was used. A
-- question can be presented once, right or wrong: used_at is set by the first
-- registration attempt that presents it.
--
-- Rows are short-lived. The API deletes expired rows whenever it asks a new
-- question, so the table stays about as small as the number of questions
-- asked in the last ten minutes. No personal data is stored and nothing
-- refers to this table.
--
-- Adds one new table and one index; no existing table or row is touched.
CREATE TABLE registration_captchas (
    -- Opaque random id (43 URL-safe characters) handed to the client.
    id          text PRIMARY KEY,
    -- SHA-256 of the id and the answer.
    answer_hash bytea NOT NULL,
    expires_at  timestamptz NOT NULL,
    -- Set by the first registration attempt that presents the captcha.
    used_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- For the clean-up of expired rows.
CREATE INDEX registration_captchas_expires_at_idx ON registration_captchas (expires_at);
