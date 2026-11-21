-- API 0.59.0: email bounces and complaints (FR-065). Resend's signed
-- delivery webhooks update the delivery state of each message; a hard bounce
-- or a complaint suppresses the address (stored as its SHA-256 only); a
-- sweep records bounce and complaint spikes for the administrators.
ALTER TABLE email_messages
    ADD COLUMN delivery_state    text CHECK (delivery_state IN ('DELIVERED', 'DELAYED', 'BOUNCED', 'COMPLAINED')),
    ADD COLUMN delivery_state_at timestamptz,
    ADD COLUMN bounce_type       text CHECK (char_length(bounce_type) BETWEEN 1 AND 40);
CREATE INDEX email_messages_provider_message_idx ON email_messages (provider_message_id) WHERE provider_message_id IS NOT NULL;
-- A suppressed message is never sent: FAILED, terminal, last_error 'suppressed'.

-- Every webhook event received once (the webhook id is the provider's
-- svix-id), for idempotency and the spike sweep. No address, only its hash.
CREATE TABLE email_delivery_events (
    webhook_id          text PRIMARY KEY CHECK (char_length(webhook_id) BETWEEN 1 AND 100),
    event_type          text        NOT NULL CHECK (event_type IN ('DELIVERED', 'DELAYED', 'BOUNCED', 'COMPLAINED')),
    provider_message_id text,
    recipient_sha256    text,
    bounce_type         text CHECK (char_length(bounce_type) BETWEEN 1 AND 40),
    hard                boolean     NOT NULL DEFAULT false,
    occurred_at         timestamptz NOT NULL,
    received_at         timestamptz NOT NULL
);
CREATE INDEX email_delivery_events_received_idx ON email_delivery_events (received_at);

-- Addresses that must not be emailed again until an administrator clears them.
CREATE TABLE email_suppressions (
    recipient_sha256    text PRIMARY KEY CHECK (char_length(recipient_sha256) = 64),
    reason              text        NOT NULL CHECK (reason IN ('HARD_BOUNCE', 'COMPLAINT')),
    provider_message_id text,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    cleared_at          timestamptz,
    cleared_by          uuid REFERENCES users (id) ON DELETE SET NULL
);
CREATE INDEX email_suppressions_active_idx ON email_suppressions (created_at DESC) WHERE cleared_at IS NULL;

-- A bounce or complaint spike seen by the sweep (one per kind per hour at most).
CREATE TABLE email_delivery_alerts (
    id           uuid PRIMARY KEY,
    kind         text        NOT NULL CHECK (kind IN ('BOUNCE_SPIKE', 'COMPLAINT_SPIKE')),
    window_start timestamptz NOT NULL,
    window_end   timestamptz NOT NULL,
    events       integer     NOT NULL CHECK (events >= 0),
    accepted     integer     NOT NULL CHECK (accepted >= 0),
    created_at   timestamptz NOT NULL
);
CREATE INDEX email_delivery_alerts_created_idx ON email_delivery_alerts (created_at DESC);

-- Clearing a suppression is audited.
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW', 'LISTING_PACKAGE', 'ORGANISATION',
                           'LEGAL_DOCUMENT_VERSION', 'TAX_RULE', 'DEVELOPMENT', 'INVOICE', 'LISTING', 'MARKET', 'EMAIL_SUPPRESSION'));
