-- 0048: replies on property requests (API 0.46.0, FR-063, BR-022, FR-064).
--
--   * property_request_messages: the conversation between the receiving side
--     (the assigned member, the organisation's owners and managers, or the
--     direct seller) and the customer who sent the request. Plain text only.
--     Private notes stay in property_request_notes and never appear here.
--     Messages are never edited or deleted by users; they go with the
--     request (ON DELETE CASCADE). author_user_id is kept when an account
--     is deleted (accounts are anonymised in place).
--   * property_requests gains the conversation summary used by the lists
--     (message_count, last_message_at, last_message_author) and the last
--     time each side opened the request (customer_last_read_at,
--     professional_last_read_at), so "new reply" and "awaiting your reply"
--     need no join.

CREATE TABLE IF NOT EXISTS property_request_messages (
    id             uuid PRIMARY KEY,
    request_id     uuid        NOT NULL REFERENCES property_requests (id) ON DELETE CASCADE,
    author_type    text        NOT NULL CONSTRAINT property_request_messages_author_check
                               CHECK (author_type IN ('PROFESSIONAL', 'CUSTOMER')),
    author_user_id uuid        REFERENCES users (id),
    -- The author's name when the message was written (shown to the
    -- receiving side only; the customer sees "The agent" or "The seller").
    author_name    text        NOT NULL DEFAULT '',
    body           text        NOT NULL CONSTRAINT property_request_messages_body_length
                               CHECK (char_length(body) BETWEEN 1 AND 2000),
    created_at     timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS property_request_messages_request_idx ON property_request_messages (request_id, created_at, id);

ALTER TABLE property_requests
    ADD COLUMN IF NOT EXISTS message_count             integer     NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_message_at           timestamptz,
    ADD COLUMN IF NOT EXISTS last_message_author       text CONSTRAINT property_requests_last_message_author_check
                                                       CHECK (last_message_author IN ('PROFESSIONAL', 'CUSTOMER')),
    ADD COLUMN IF NOT EXISTS customer_last_read_at     timestamptz,
    ADD COLUMN IF NOT EXISTS professional_last_read_at timestamptz;
