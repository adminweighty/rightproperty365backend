-- API 0.55.0: personal data export requests (FSD section 9).
CREATE TABLE data_exports (
    id               uuid PRIMARY KEY,
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    status           text        NOT NULL CHECK (status IN ('REQUESTED', 'BUILDING', 'READY', 'FAILED', 'EXPIRED')),
    -- The archive in the private bucket while READY (deleted, and NULL, once EXPIRED).
    object_key       text,
    size_bytes       bigint      NOT NULL DEFAULT 0,
    attempts         integer     NOT NULL DEFAULT 0,
    lease_until      timestamptz,
    requested_at     timestamptz NOT NULL,
    ready_at         timestamptz,
    expires_at       timestamptz,
    downloads        integer     NOT NULL DEFAULT 0,
    last_download_at timestamptz,
    expired_at       timestamptz,
    CONSTRAINT data_exports_ready CHECK ((status = 'READY') = (object_key IS NOT NULL))
);
CREATE INDEX data_exports_user_idx ON data_exports (user_id, requested_at DESC);
CREATE INDEX data_exports_work_idx ON data_exports (status, requested_at) WHERE status IN ('REQUESTED', 'BUILDING', 'READY');
