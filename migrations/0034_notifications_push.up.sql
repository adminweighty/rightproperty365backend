-- 0034: in-app notifications, push and device registration (API 0.35.0,
-- FR-044, FR-045, FR-062, FR-064, FR-065, FR-003).
--
--   * user_devices: a user's push tokens (FCM registration tokens; FCM
--     carries APNs). A token is unique: registering it again updates the
--     row (and moves it to the new user). session_id is the session that
--     registered it: its logout removes the device. Removed on account
--     deletion (explicitly: the users row is anonymised, so the cascade
--     never fires) and when FCM reports the token invalid.
--   * notifications: the durable in-app record, created by the worker from
--     outbox events: ONE per (source_event_id, user_id), enforced by the
--     unique key, so a redelivered event never creates a second one. Safe
--     text only (a reference and a place). read_at is the recipient's own.
--   * notification_deliveries: one row per notification per channel target
--     (PUSH per device, EMAIL once): PENDING, ATTEMPTED, PROVIDER_ACCEPTED,
--     FAILED (retry at next_attempt_at) or DEAD. There is deliberately no
--     "delivered" status: a provider accepting a push is not a person
--     seeing it. target is the device id (text) or 'email', so a removed
--     device (device_id SET NULL) never collides with another.
--   * notification_preferences: per user and type, email and push (in-app
--     is always on; essential types are never stored as off: the API
--     refuses it), with changed_at and source; notification_preference_
--     changes keeps every change (FR-003: timestamped, source-attributed).
--
-- Constraint names: inline CHECKs get Postgres's <table>_<column>_check;
-- the table-level constraints below are named differently on purpose.
--
-- Safe on a database with data: new tables only.

CREATE TABLE IF NOT EXISTS user_devices (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    session_id   uuid,
    token        text NOT NULL CHECK (char_length(token) BETWEEN 1 AND 4096),
    platform     text NOT NULL CHECK (platform IN ('IOS', 'ANDROID', 'WEB')),
    app_version  text NOT NULL CHECK (char_length(app_version) BETWEEN 1 AND 40),
    locale       text CHECK (char_length(locale) <= 35),
    push_consent boolean NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_devices_token_key UNIQUE (token)
);
CREATE INDEX IF NOT EXISTS user_devices_user_idx ON user_devices (user_id);
CREATE INDEX IF NOT EXISTS user_devices_session_idx ON user_devices (session_id) WHERE session_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS notifications (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    source_event_id uuid NOT NULL,
    type            text NOT NULL CHECK (type ~ '^[A-Z_]{1,40}$'),
    title           text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 120),
    body            text NOT NULL CHECK (char_length(body) <= 200),
    link            text NOT NULL CHECK (link LIKE '/%' AND link NOT LIKE '//%' AND char_length(link) <= 300),
    reference       text CHECK (char_length(reference) <= 60),
    created_at      timestamptz NOT NULL DEFAULT now(),
    read_at         timestamptz,
    CONSTRAINT notifications_one_per_event_recipient UNIQUE (source_event_id, user_id)
);
CREATE INDEX IF NOT EXISTS notifications_user_created_idx ON notifications (user_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS notifications_user_unread_idx ON notifications (user_id) WHERE read_at IS NULL;
CREATE INDEX IF NOT EXISTS notifications_created_idx ON notifications (created_at);

CREATE TABLE IF NOT EXISTS notification_deliveries (
    id                  uuid PRIMARY KEY,
    notification_id     uuid NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    user_id             uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel             text NOT NULL CHECK (channel IN ('PUSH', 'EMAIL')),
    device_id           uuid REFERENCES user_devices (id) ON DELETE SET NULL,
    target              text NOT NULL CHECK (char_length(target) BETWEEN 1 AND 40),
    status              text NOT NULL CHECK (status IN ('PENDING', 'ATTEMPTED', 'PROVIDER_ACCEPTED', 'FAILED', 'DEAD')),
    attempts            integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error          text CHECK (char_length(last_error) <= 500),
    provider_message_id text CHECK (char_length(provider_message_id) <= 300),
    next_attempt_at     timestamptz,
    last_attempt_at     timestamptz,
    batch_id            uuid,
    summary             boolean NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notification_deliveries_one_per_target UNIQUE (notification_id, channel, target)
);
CREATE INDEX IF NOT EXISTS notification_deliveries_due_idx ON notification_deliveries (next_attempt_at, created_at)
    WHERE channel = 'PUSH' AND status IN ('PENDING', 'ATTEMPTED', 'FAILED');
CREATE INDEX IF NOT EXISTS notification_deliveries_device_idx ON notification_deliveries (device_id, last_attempt_at)
    WHERE device_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS notification_deliveries_status_idx ON notification_deliveries (channel, status);

CREATE TABLE IF NOT EXISTS notification_preferences (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type       text NOT NULL CHECK (type ~ '^[A-Z_]{1,40}$'),
    email      boolean NOT NULL,
    push       boolean NOT NULL,
    changed_at timestamptz NOT NULL,
    source     text NOT NULL CHECK (source IN ('web', 'app', 'api')),
    PRIMARY KEY (user_id, type)
);

CREATE TABLE IF NOT EXISTS notification_preference_changes (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type       text NOT NULL CHECK (type ~ '^[A-Z_]{1,40}$'),
    email      boolean NOT NULL,
    push       boolean NOT NULL,
    changed_at timestamptz NOT NULL,
    source     text NOT NULL CHECK (source IN ('web', 'app', 'api'))
);
CREATE INDEX IF NOT EXISTS notification_preference_changes_user_idx ON notification_preference_changes (user_id, changed_at);
