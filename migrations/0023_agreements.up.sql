-- 0023_agreements: electronic signing in the style of Adobe Acrobat Sign
-- (FR-032), for generated leases and uploaded PDFs.
--
--   * agreements: one PDF (a lease's signing copy, or an uploaded PDF in the
--     private object storage), DRAFT -> OUT_FOR_SIGNATURE -> COMPLETED |
--     DECLINED | CANCELLED | EXPIRED. The original's and the completed
--     document's SHA-256 are kept (the public verification looks them up).
--   * agreement_recipients: SIGNER, APPROVER or CC with an order (equal
--     orders act in parallel); the live email code is stored only as an
--     HMAC; the adopted signature and initials (typed text and style, or an
--     image's object key).
--   * agreement_fields: the fields placed on the document (PDF points from
--     the page's top-left) and their values once filled.
--   * agreement_links: signing links; only the SHA-256 of a token is stored.
--   * agreement_events: the audit trail (immutable).
--   * agreement_jobs: one row per reminder sent (the worker's idempotency).
--   * user_signatures: a signed-in user's saved signature and initials.
--   * leases.agreement_id gets its foreign key.
--
-- Safe on a database with data: only new objects, each created only if
-- missing; no existing row is rewritten.

CREATE SEQUENCE IF NOT EXISTS agreement_reference_seq;

CREATE TABLE IF NOT EXISTS agreements (
    id                 uuid PRIMARY KEY,
    reference          text NOT NULL CONSTRAINT agreements_reference_key UNIQUE
                       CONSTRAINT agreements_reference_check CHECK (reference ~ '^RPA-[0-9]{4}-[0-9]{6,}$'),
    name               text NOT NULL CONSTRAINT agreements_name_check CHECK (char_length(name) BETWEEN 1 AND 200),
    message            text NOT NULL DEFAULT '',
    status             text NOT NULL DEFAULT 'DRAFT' CONSTRAINT agreements_status_check
                       CHECK (status IN ('DRAFT', 'OUT_FOR_SIGNATURE', 'COMPLETED', 'DECLINED', 'CANCELLED', 'EXPIRED')),
    source             text NOT NULL CONSTRAINT agreements_source_check CHECK (source IN ('UPLOAD', 'LEASE')),
    lease_id           uuid REFERENCES leases (id) ON DELETE SET NULL,
    sender_id          uuid REFERENCES users (id) ON DELETE SET NULL,
    sender_name        text NOT NULL DEFAULT '',
    sender_email       text NOT NULL DEFAULT '',
    organisation_id    uuid REFERENCES organisations (id) ON DELETE SET NULL,
    organisation_name  text NOT NULL DEFAULT '',
    expires_at         timestamptz NOT NULL,
    reminder_frequency text NOT NULL DEFAULT 'NONE' CONSTRAINT agreements_reminder_check
                       CHECK (reminder_frequency IN ('NONE', 'DAILY', 'EVERY_3_DAYS', 'WEEKLY')),
    original_object_key text,
    original_sha256    text CONSTRAINT agreements_original_sha256_check CHECK (original_sha256 IS NULL OR original_sha256 ~ '^[0-9a-f]{64}$'),
    original_size      bigint,
    original_file_name text,
    -- [{width, height}] in PDF points, one per page.
    pages              jsonb NOT NULL DEFAULT '[]'::jsonb,
    final_object_key   text,
    final_sha256       text CONSTRAINT agreements_final_sha256_check CHECK (final_sha256 IS NULL OR final_sha256 ~ '^[0-9a-f]{64}$'),
    final_size         bigint,
    final_file_name    text,
    cancel_reason      text NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    sent_at            timestamptz,
    completed_at       timestamptz,
    declined_at        timestamptz,
    cancelled_at       timestamptz,
    expired_at         timestamptz,
    CONSTRAINT agreements_completed_check CHECK ((status = 'COMPLETED') = (completed_at IS NOT NULL AND final_sha256 IS NOT NULL)),
    CONSTRAINT agreements_sent_check CHECK (status IN ('DRAFT', 'CANCELLED') OR (sent_at IS NOT NULL AND original_sha256 IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS agreements_organisation_idx ON agreements (organisation_id, created_at DESC);
CREATE INDEX IF NOT EXISTS agreements_sender_idx ON agreements (sender_id, created_at DESC);
CREATE INDEX IF NOT EXISTS agreements_status_idx ON agreements (status, expires_at);
CREATE INDEX IF NOT EXISTS agreements_final_sha256_idx ON agreements (final_sha256) WHERE final_sha256 IS NOT NULL;
CREATE INDEX IF NOT EXISTS agreements_original_sha256_idx ON agreements (original_sha256) WHERE original_sha256 IS NOT NULL;
CREATE INDEX IF NOT EXISTS agreements_lease_idx ON agreements (lease_id) WHERE lease_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS agreement_recipients (
    id              uuid PRIMARY KEY,
    agreement_id    uuid NOT NULL REFERENCES agreements (id) ON DELETE CASCADE,
    signing_order   integer NOT NULL CONSTRAINT agreement_recipients_order_check CHECK (signing_order BETWEEN 1 AND 20),
    role            text NOT NULL CONSTRAINT agreement_recipients_role_check CHECK (role IN ('SIGNER', 'APPROVER', 'CC')),
    name            text NOT NULL,
    email           text NOT NULL CONSTRAINT agreement_recipients_email_check CHECK (email = lower(email)),
    label           text NOT NULL DEFAULT '',
    status          text NOT NULL CONSTRAINT agreement_recipients_status_check
                    CHECK (status IN ('WAITING', 'ACTIVE', 'SIGNED', 'APPROVED', 'DECLINED', 'CC')),
    activated_at    timestamptz,
    first_viewed_at timestamptz,
    completed_at    timestamptz,
    declined_at     timestamptz,
    decline_reason  text NOT NULL DEFAULT '',
    -- The live code: HMAC-SHA256 keyed with the raw link token; never the code.
    code_hash       bytea,
    code_link_id    uuid,
    code_expires_at timestamptz,
    code_attempts   integer NOT NULL DEFAULT 0,
    code_sent_at    timestamptz,
    code_send_count integer NOT NULL DEFAULT 0,
    ip              text NOT NULL DEFAULT '',
    user_agent      text NOT NULL DEFAULT '',
    verification    text NOT NULL DEFAULT '' CONSTRAINT agreement_recipients_verification_check
                    CHECK (verification IN ('', 'EMAIL_CODE', 'DEMO_CODE')),
    signature       jsonb,
    initials        jsonb,
    CONSTRAINT agreement_recipients_email_key UNIQUE (agreement_id, email)
);
CREATE INDEX IF NOT EXISTS agreement_recipients_email_idx ON agreement_recipients (email);

CREATE TABLE IF NOT EXISTS agreement_fields (
    id           uuid PRIMARY KEY,
    agreement_id uuid NOT NULL REFERENCES agreements (id) ON DELETE CASCADE,
    recipient_id uuid NOT NULL REFERENCES agreement_recipients (id) ON DELETE CASCADE,
    type         text NOT NULL CONSTRAINT agreement_fields_type_check
                 CHECK (type IN ('SIGNATURE', 'INITIALS', 'DATE_SIGNED', 'NAME', 'TEXT', 'CHECKBOX')),
    page         integer NOT NULL CONSTRAINT agreement_fields_page_check CHECK (page >= 1),
    x            double precision NOT NULL,
    y            double precision NOT NULL,
    w            double precision NOT NULL CONSTRAINT agreement_fields_w_check CHECK (w > 0),
    h            double precision NOT NULL CONSTRAINT agreement_fields_h_check CHECK (h > 0),
    required     boolean NOT NULL DEFAULT true,
    label        text NOT NULL DEFAULT '',
    value        text NOT NULL DEFAULT '',
    position     integer NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS agreement_fields_agreement_idx ON agreement_fields (agreement_id, position);

CREATE TABLE IF NOT EXISTS agreement_links (
    id           uuid PRIMARY KEY,
    agreement_id uuid NOT NULL REFERENCES agreements (id) ON DELETE CASCADE,
    recipient_id uuid NOT NULL REFERENCES agreement_recipients (id) ON DELETE CASCADE,
    -- SHA-256 of the token in the emailed link. The token itself is never stored.
    token_hash   bytea NOT NULL CONSTRAINT agreement_links_token_hash_key UNIQUE,
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS agreement_links_agreement_idx ON agreement_links (agreement_id);

CREATE TABLE IF NOT EXISTS agreement_events (
    id            uuid PRIMARY KEY,
    agreement_id  uuid NOT NULL REFERENCES agreements (id) ON DELETE CASCADE,
    action        text NOT NULL CONSTRAINT agreement_events_action_check CHECK (action IN (
                      'CREATED', 'DOCUMENT_ADDED', 'FIELDS_CHANGED', 'SENT', 'EMAILED', 'VIEWED', 'CODE_SENT',
                      'EMAIL_VERIFIED', 'SIGNED', 'APPROVED', 'DECLINED', 'REMINDED', 'RECIPIENT_REPLACED',
                      'CANCELLED', 'EXPIRED', 'COMPLETED', 'COPY_SENT')),
    actor_type    text NOT NULL CONSTRAINT agreement_events_actor_type_check CHECK (actor_type IN ('USER', 'RECIPIENT', 'SYSTEM')),
    actor_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_name    text NOT NULL DEFAULT '',
    recipient_id  uuid,
    ip            text NOT NULL DEFAULT '',
    user_agent    text NOT NULL DEFAULT '',
    detail        jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS agreement_events_agreement_idx ON agreement_events (agreement_id, created_at, id);

CREATE OR REPLACE FUNCTION agreement_events_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.agreement_id IS DISTINCT FROM OLD.agreement_id
       OR NEW.action IS DISTINCT FROM OLD.action
       OR NEW.actor_type IS DISTINCT FROM OLD.actor_type
       OR NEW.actor_name IS DISTINCT FROM OLD.actor_name
       OR NEW.recipient_id IS DISTINCT FROM OLD.recipient_id
       OR NEW.ip IS DISTINCT FROM OLD.ip
       OR NEW.user_agent IS DISTINCT FROM OLD.user_agent
       OR NEW.detail IS DISTINCT FROM OLD.detail
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (NEW.actor_user_id IS DISTINCT FROM OLD.actor_user_id AND NEW.actor_user_id IS NOT NULL) THEN
        RAISE EXCEPTION 'agreement_events rows are immutable';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS agreement_events_no_update ON agreement_events;
CREATE TRIGGER agreement_events_no_update BEFORE UPDATE ON agreement_events
    FOR EACH ROW EXECUTE FUNCTION agreement_events_immutable();

CREATE TABLE IF NOT EXISTS agreement_jobs (
    key        text PRIMARY KEY CONSTRAINT agreement_jobs_key_check CHECK (char_length(key) BETWEEN 1 AND 200),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS user_signatures (
    user_id    uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    signature  jsonb,
    initials   jsonb,
    updated_at timestamptz NOT NULL DEFAULT now()
);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'leases_agreement_id_fkey') THEN
        ALTER TABLE leases ADD CONSTRAINT leases_agreement_id_fkey
            FOREIGN KEY (agreement_id) REFERENCES agreements (id) ON DELETE SET NULL;
    END IF;
END;
$$;
