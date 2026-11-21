-- API 0.52.0: saved searches and their alerts (FR-042).
CREATE TABLE saved_searches (
    id                uuid PRIMARY KEY,
    user_id           uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name              text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    -- The search's canonical query string, as GET /v1/listings takes it.
    query             text        NOT NULL CHECK (char_length(query) <= 2000),
    frequency         text        NOT NULL CHECK (frequency IN ('INSTANT', 'DAILY', 'WEEKLY', 'OFF')),
    -- The capability in the alert emails' unsubscribe link: it can only
    -- switch this search's alerts off.
    unsubscribe_token text        NOT NULL UNIQUE,
    -- Listings published after the watermark are new.
    last_checked_at   timestamptz NOT NULL,
    next_check_at     timestamptz,
    last_alert_at     timestamptz,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    CONSTRAINT saved_searches_next_check CHECK ((frequency = 'OFF') = (next_check_at IS NULL))
);
CREATE INDEX saved_searches_user_idx ON saved_searches (user_id, created_at DESC);
CREATE INDEX saved_searches_due_idx ON saved_searches (next_check_at) WHERE frequency <> 'OFF';
