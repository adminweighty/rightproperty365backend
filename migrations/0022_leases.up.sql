-- 0022_leases: lease templates and leases (BR-011,
-- FR-030, FR-031). Signing (FR-032) is 0023_agreements.
--
--   * lease_templates: the template library kept by content administrators.
--     A template is one VERSION of a family (family_id, version): DRAFT ->
--     APPROVED (with its legal review) -> RETIRED. At most one DRAFT per
--     family; approving a version retires the family's earlier APPROVED one.
--     sections and optional_clauses are JSON arrays; the {{variable}}
--     placeholders are checked by the API against a fixed list.
--   * lease_template_events: the template audit trail (immutable).
--   * leases: a lease drafted from a template by someone who manages
--     listings. input_snapshot is everything they entered (frozen once the
--     lease is sent). The human reference RPL-<year>-<6 digits> comes from
--     lease_reference_seq.
--   * lease_documents: the stored PDFs (private object storage; the key is
--     never shown to anyone). One row per kind: DRAFT (replaced on every
--     save, only the newest is kept), SIGNING (the frozen copy the parties
--     sign) and FINAL (the signed PDF).
--     Each row keeps the input snapshot it was rendered from, the template
--     version and the document's SHA-256. FINAL is the signed PDF of the
--     lease's agreement (signing is the agreements module, migration 0023).
--   * lease_events: the lease audit trail (immutable).
--
-- Safe on a database with data: only new objects, each created only if missing.

CREATE SEQUENCE IF NOT EXISTS lease_reference_seq;

-- ---------------------------------------------------------------------------
-- Templates
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS lease_templates (
    id               uuid PRIMARY KEY,
    family_id        uuid NOT NULL,
    version          integer NOT NULL CONSTRAINT lease_templates_version_check CHECK (version >= 1),
    market_code      text NOT NULL CONSTRAINT lease_templates_market_code_check CHECK (market_code ~ '^[A-Z]{2}$'),
    name             text NOT NULL CONSTRAINT lease_templates_name_check CHECK (char_length(name) BETWEEN 1 AND 120),
    lease_kind       text NOT NULL CONSTRAINT lease_templates_lease_kind_check CHECK (lease_kind IN ('RESIDENTIAL', 'COMMERCIAL')),
    description      text NOT NULL DEFAULT '',
    sections         jsonb NOT NULL CONSTRAINT lease_templates_sections_check CHECK (jsonb_typeof(sections) = 'array'),
    optional_clauses jsonb NOT NULL DEFAULT '[]'::jsonb
                     CONSTRAINT lease_templates_optional_clauses_check CHECK (jsonb_typeof(optional_clauses) = 'array'),
    status           text NOT NULL DEFAULT 'DRAFT' CONSTRAINT lease_templates_status_check
                     CHECK (status IN ('DRAFT', 'APPROVED', 'RETIRED')),
    -- {reviewer_name, organisation, reviewed_on, note?}: required once approved.
    legal_review     jsonb,
    created_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by_name  text NOT NULL DEFAULT '',
    approved_at      timestamptz,
    approved_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    approved_by_name text NOT NULL DEFAULT '',
    retired_at       timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT lease_templates_family_version_key UNIQUE (family_id, version),
    CONSTRAINT lease_templates_review_check CHECK (status = 'DRAFT' OR (legal_review IS NOT NULL AND approved_at IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS lease_templates_one_draft_key ON lease_templates (family_id) WHERE status = 'DRAFT';
CREATE UNIQUE INDEX IF NOT EXISTS lease_templates_one_approved_key ON lease_templates (family_id) WHERE status = 'APPROVED';
CREATE INDEX IF NOT EXISTS lease_templates_market_status_idx ON lease_templates (market_code, status);

CREATE TABLE IF NOT EXISTS lease_template_events (
    id            uuid PRIMARY KEY,
    template_id   uuid NOT NULL REFERENCES lease_templates (id) ON DELETE CASCADE,
    action        text NOT NULL CONSTRAINT lease_template_events_action_check
                  CHECK (action IN ('CREATED', 'EDITED', 'NEW_VERSION', 'APPROVED', 'RETIRED')),
    actor_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_name    text NOT NULL DEFAULT '',
    detail        jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS lease_template_events_template_idx ON lease_template_events (template_id, created_at);

-- ---------------------------------------------------------------------------
-- Leases
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS leases (
    id                uuid PRIMARY KEY,
    reference         text NOT NULL CONSTRAINT leases_reference_key UNIQUE
                      CONSTRAINT leases_reference_check CHECK (reference ~ '^RPL-[0-9]{4}-[0-9]{6,}$'),
    status            text NOT NULL DEFAULT 'DRAFT' CONSTRAINT leases_status_check
                      CHECK (status IN ('DRAFT', 'SENT_FOR_SIGNING', 'SIGNED', 'DECLINED', 'CANCELLED', 'EXPIRED')),
    -- A template that leases use is never deleted (RESTRICT).
    template_id       uuid NOT NULL REFERENCES lease_templates (id) ON DELETE RESTRICT,
    template_version  integer NOT NULL,
    template_name     text NOT NULL,
    market_code       text NOT NULL CONSTRAINT leases_market_code_check CHECK (market_code ~ '^[A-Z]{2}$'),
    lease_kind        text NOT NULL CONSTRAINT leases_lease_kind_check CHECK (lease_kind IN ('RESIDENTIAL', 'COMMERCIAL')),
    organisation_id   uuid REFERENCES organisations (id) ON DELETE SET NULL,
    -- The organisation's name when the lease was drafted.
    organisation_name text NOT NULL DEFAULT '',
    listing_id        uuid REFERENCES listings (id) ON DELETE SET NULL,
    listing_reference text NOT NULL DEFAULT '',
    listing_title     text NOT NULL DEFAULT '',
    created_by        uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by_name   text NOT NULL DEFAULT '',
    input_snapshot    jsonb NOT NULL,
    -- The agreement the lease was sent for signing with (0023 adds the foreign key).
    agreement_id      uuid,
    sent_at           timestamptz,
    sent_by           uuid REFERENCES users (id) ON DELETE SET NULL,
    completed_at      timestamptz,
    declined_at       timestamptz,
    cancelled_at      timestamptz,
    cancelled_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    cancel_reason     text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT leases_sent_check CHECK (status IN ('DRAFT', 'CANCELLED') OR sent_at IS NOT NULL),
    CONSTRAINT leases_signed_check CHECK ((status = 'SIGNED') = (completed_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS leases_organisation_idx ON leases (organisation_id, created_at DESC);
CREATE INDEX IF NOT EXISTS leases_created_by_idx ON leases (created_by, created_at DESC);
CREATE INDEX IF NOT EXISTS leases_status_idx ON leases (status, created_at DESC);

CREATE TABLE IF NOT EXISTS lease_documents (
    id               uuid PRIMARY KEY,
    lease_id         uuid NOT NULL REFERENCES leases (id) ON DELETE CASCADE,
    kind             text NOT NULL CONSTRAINT lease_documents_kind_check CHECK (kind IN ('DRAFT', 'SIGNING', 'FINAL')),
    template_id      uuid NOT NULL REFERENCES lease_templates (id) ON DELETE RESTRICT,
    template_version integer NOT NULL,
    input_snapshot   jsonb NOT NULL,
    object_key       text NOT NULL,
    sha256           text NOT NULL CONSTRAINT lease_documents_sha256_check CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes       bigint NOT NULL CONSTRAINT lease_documents_size_check CHECK (size_bytes > 0),
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT lease_documents_lease_kind_key UNIQUE (lease_id, kind)
);

CREATE TABLE IF NOT EXISTS lease_events (
    id            uuid PRIMARY KEY,
    lease_id      uuid NOT NULL REFERENCES leases (id) ON DELETE CASCADE,
    action        text NOT NULL CONSTRAINT lease_events_action_check CHECK (action IN (
                      'CREATED', 'EDITED', 'PDF_GENERATED', 'SENT', 'SIGNED', 'DECLINED', 'CANCELLED', 'EXPIRED')),
    actor_type    text NOT NULL CONSTRAINT lease_events_actor_type_check CHECK (actor_type IN ('USER', 'SYSTEM')),
    actor_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_name    text NOT NULL DEFAULT '',
    detail        jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS lease_events_lease_idx ON lease_events (lease_id, created_at, id);

-- ---------------------------------------------------------------------------
-- Immutable audit rows: an UPDATE may only clear the actor
-- (account deletion); nothing else may change.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION lease_events_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.lease_id IS DISTINCT FROM OLD.lease_id
       OR NEW.action IS DISTINCT FROM OLD.action
       OR NEW.actor_type IS DISTINCT FROM OLD.actor_type
       OR NEW.actor_name IS DISTINCT FROM OLD.actor_name
       OR NEW.detail IS DISTINCT FROM OLD.detail
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (NEW.actor_user_id IS DISTINCT FROM OLD.actor_user_id AND NEW.actor_user_id IS NOT NULL) THEN
        RAISE EXCEPTION 'lease_events rows are immutable';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS lease_events_no_update ON lease_events;
CREATE TRIGGER lease_events_no_update BEFORE UPDATE ON lease_events
    FOR EACH ROW EXECUTE FUNCTION lease_events_immutable();

CREATE OR REPLACE FUNCTION lease_template_events_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.template_id IS DISTINCT FROM OLD.template_id
       OR NEW.action IS DISTINCT FROM OLD.action
       OR NEW.actor_name IS DISTINCT FROM OLD.actor_name
       OR NEW.detail IS DISTINCT FROM OLD.detail
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (NEW.actor_user_id IS DISTINCT FROM OLD.actor_user_id AND NEW.actor_user_id IS NOT NULL) THEN
        RAISE EXCEPTION 'lease_template_events rows are immutable';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS lease_template_events_no_update ON lease_template_events;
CREATE TRIGGER lease_template_events_no_update BEFORE UPDATE ON lease_template_events
    FOR EACH ROW EXECUTE FUNCTION lease_template_events_immutable();

