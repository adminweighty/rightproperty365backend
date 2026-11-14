-- 0006_email_messages: one row per logical email, recording what happened to it.
--
-- Statuses
--   ATTEMPTED          a send is in progress, or failed in a way that will be retried
--   PROVIDER_ACCEPTED  the email provider accepted the message and returned an ID.
--                      This is NOT "delivered": nothing here knows whether the
--                      message reached an inbox.
--   FAILED             the last attempt failed; `terminal` says whether it will be retried
--
-- notification_key identifies the logical notification, for example
-- "property-request-received/<outbox event id>/customer". It is also sent to
-- the provider as the idempotency key, which is what guarantees one email per
-- notification even if a worker crashes between the send and this record.
--
-- Deliberately NOT stored: the recipient address (only its SHA-256), the
-- subject, the body and any link. Verification and reset tokens therefore
-- never reach this table.
CREATE TABLE email_messages (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    notification_key    text NOT NULL CONSTRAINT email_messages_notification_key_key UNIQUE
                        CONSTRAINT email_messages_notification_key_length CHECK (char_length(notification_key) BETWEEN 1 AND 256),
    -- Template name, e.g. verify-email.
    kind                text NOT NULL,
    recipient_user_id   uuid REFERENCES users (id) ON DELETE SET NULL,
    recipient_sha256    text NOT NULL,
    status              text NOT NULL CONSTRAINT email_messages_status_check
                        CHECK (status IN ('ATTEMPTED', 'PROVIDER_ACCEPTED', 'FAILED')),
    terminal            boolean NOT NULL DEFAULT false,
    attempts            integer NOT NULL DEFAULT 1 CONSTRAINT email_messages_attempts_positive CHECK (attempts >= 1),
    provider            text NOT NULL,
    provider_message_id text,
    -- Short, safe reason (HTTP status and the provider's error name). Never a header or a body.
    last_error          text,
    accepted_at         timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT email_messages_accepted_check
        CHECK ((status = 'PROVIDER_ACCEPTED') = (provider_message_id IS NOT NULL AND accepted_at IS NOT NULL))
);
CREATE INDEX email_messages_status_idx ON email_messages (status, updated_at);
CREATE INDEX email_messages_recipient_user_idx ON email_messages (recipient_user_id, created_at DESC)
    WHERE recipient_user_id IS NOT NULL;
