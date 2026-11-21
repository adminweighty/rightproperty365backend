-- API 0.60.0: upload intents (FR-010). A signed-in client asks to upload one
-- file for a purpose, PUTs it straight to private storage (a presigned URL,
-- or the API itself with local storage), then completes it: the API checks
-- size, SHA-256, real type and malware before the file can be used once.
CREATE TABLE upload_intents (
    id            uuid PRIMARY KEY,
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose       text        NOT NULL CHECK (purpose IN ('LISTING_PHOTO')),
    content_type  text        NOT NULL CHECK (char_length(content_type) BETWEEN 1 AND 100),
    size_bytes    bigint      NOT NULL CHECK (size_bytes > 0),
    sha256        text        NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    object_key    text        NOT NULL CHECK (char_length(object_key) BETWEEN 1 AND 300),
    status        text        NOT NULL CHECK (status IN ('PENDING_UPLOAD', 'READY', 'REJECTED', 'USED', 'EXPIRED')),
    reason        text CHECK (char_length(reason) BETWEEN 1 AND 200),
    created_at    timestamptz NOT NULL,
    upload_by     timestamptz NOT NULL,
    completed_at  timestamptz,
    used_at       timestamptz,
    expires_at    timestamptz NOT NULL
);
CREATE INDEX upload_intents_user_idx ON upload_intents (user_id, created_at DESC);
CREATE INDEX upload_intents_expiry_idx ON upload_intents (expires_at) WHERE status IN ('PENDING_UPLOAD', 'READY', 'REJECTED');
