-- 0045: per-market legal documents, acceptances and tax rules (API 0.43.0, BR-001).
--
--   * legal_document_versions: one row per version of a legal document, per
--     kind (TERMS, PRIVACY, COOKIES, REFUND, EULA, AGENT_TERMS), market
--     (GLOBAL, the fallback, or an ISO 3166-1 alpha-2 code) and language
--     (en). Status DRAFT -> PUBLISHED -> RETIRED. A PUBLISHED or RETIRED row
--     never changes (legal_document_versions_frozen): only PUBLISHED ->
--     RETIRED, and the unlinking of an account that is deleted, are allowed;
--     only DRAFT rows can be deleted. A change is always a new version.
--     created_by / published_by: account deletion anonymises users rows, so
--     ON DELETE SET NULL never fires; the accounts module unlinks them
--     (legal.ForgetUserTx) and replaces the names.
--   * user_consents (0002) records every acceptance: consent_type now allows
--     the legal kinds (PRIVACY stays PRIVACY_NOTICE), plus market_code (the
--     document's market) and context (REGISTRATION | REACCEPTANCE). The old
--     check user_consents_type_check is replaced by user_consents_type_allowed.
--     Acceptances are KEPT on account deletion, as the privacy-notice records
--     always were (the users row they name is anonymised).
--   * tax_rules: per market, tax name, rate (percent), what it applies to,
--     inclusive or exclusive, effective_from / effective_to (exclusive),
--     registration number label, status ACTIVE | RETIRED, is_sample. No user
--     columns (the administrators' audit trail names who changed what).
--   * admin_audit_events targets gain LEGAL_DOCUMENT_VERSION and TAX_RULE.
--
-- No rows are inserted: production gets no legal document and no tax rule
-- (nothing live, nothing presented as final, no unconfirmed rate). The
-- development and staging seed (cmd/seed, devseed.SeedPostgresLegalTax)
-- publishes the GLOBAL placeholders, adds DRAFT copies for ZW and GB and the
-- SAMPLE tax rules ZW VAT 15% / GB VAT 20%.
--
-- Constraint names: every constraint is named explicitly and never takes
-- Postgres's <table>_<column>_check form. Safe on a database with data and
-- re-runnable (IF NOT EXISTS, DROP ... IF EXISTS).

CREATE TABLE IF NOT EXISTS legal_document_versions (
    id                    uuid PRIMARY KEY,
    kind                  text NOT NULL,
    market_code           text NOT NULL,
    lang                  text NOT NULL,
    version_label         text NOT NULL,
    effective_from        timestamptz NOT NULL,
    status                text NOT NULL,
    body_md               text NOT NULL,
    summary_of_changes    text NOT NULL DEFAULT '',
    requires_reacceptance boolean NOT NULL DEFAULT false,
    draft_notice          boolean NOT NULL DEFAULT true,
    created_by            uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by_name       text NOT NULL,
    published_by          uuid REFERENCES users (id) ON DELETE SET NULL,
    published_by_name     text,
    published_at          timestamptz,
    retired_at            timestamptz,
    created_at            timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL,
    CONSTRAINT legal_document_versions_kind_allowed
        CHECK (kind IN ('TERMS', 'PRIVACY', 'COOKIES', 'REFUND', 'EULA', 'AGENT_TERMS')),
    CONSTRAINT legal_document_versions_market_format CHECK (market_code = 'GLOBAL' OR market_code ~ '^[A-Z]{2}$'),
    CONSTRAINT legal_document_versions_lang_format CHECK (lang ~ '^[a-z]{2}$'),
    CONSTRAINT legal_document_versions_label_format
        CHECK (char_length(version_label) BETWEEN 1 AND 32 AND version_label ~ '^[ -~]+$'),
    CONSTRAINT legal_document_versions_status_allowed CHECK (status IN ('DRAFT', 'PUBLISHED', 'RETIRED')),
    CONSTRAINT legal_document_versions_body_length CHECK (char_length(body_md) BETWEEN 1 AND 200000),
    CONSTRAINT legal_document_versions_summary_length CHECK (char_length(summary_of_changes) <= 2000),
    CONSTRAINT legal_document_versions_creator_name_length CHECK (char_length(created_by_name) BETWEEN 1 AND 200),
    CONSTRAINT legal_document_versions_published_when_done
        CHECK (status = 'DRAFT' OR (published_at IS NOT NULL AND published_by_name IS NOT NULL)),
    CONSTRAINT legal_document_versions_retired_when_done CHECK ((status = 'RETIRED') = (retired_at IS NOT NULL)),
    CONSTRAINT legal_document_versions_label_unique UNIQUE (kind, market_code, lang, version_label)
);

CREATE INDEX IF NOT EXISTS legal_document_versions_resolve_idx
    ON legal_document_versions (kind, market_code, lang, effective_from DESC) WHERE status = 'PUBLISHED';
CREATE INDEX IF NOT EXISTS legal_document_versions_created_by_idx ON legal_document_versions (created_by) WHERE created_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS legal_document_versions_published_by_idx ON legal_document_versions (published_by) WHERE published_by IS NOT NULL;

CREATE OR REPLACE FUNCTION legal_document_versions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status <> 'DRAFT' THEN
            RAISE EXCEPTION 'legal_document_versions: only a DRAFT can be deleted'
                USING ERRCODE = 'check_violation', CONSTRAINT = 'legal_document_versions_frozen';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.status = 'DRAFT' THEN
        RETURN NEW;
    END IF;
    -- PUBLISHED / RETIRED: the content never changes.
    IF NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.market_code IS DISTINCT FROM OLD.market_code
       OR NEW.lang IS DISTINCT FROM OLD.lang
       OR NEW.version_label IS DISTINCT FROM OLD.version_label
       OR NEW.effective_from IS DISTINCT FROM OLD.effective_from
       OR NEW.body_md IS DISTINCT FROM OLD.body_md
       OR NEW.summary_of_changes IS DISTINCT FROM OLD.summary_of_changes
       OR NEW.requires_reacceptance IS DISTINCT FROM OLD.requires_reacceptance
       OR NEW.draft_notice IS DISTINCT FROM OLD.draft_notice
       OR NEW.published_at IS DISTINCT FROM OLD.published_at
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (NEW.status IS DISTINCT FROM OLD.status AND NOT (OLD.status = 'PUBLISHED' AND NEW.status = 'RETIRED'))
       OR (NEW.retired_at IS DISTINCT FROM OLD.retired_at AND OLD.status <> 'PUBLISHED')
       -- Account deletion may only unlink: an ID becomes NULL, never another.
       OR (NEW.created_by IS DISTINCT FROM OLD.created_by AND NEW.created_by IS NOT NULL)
       OR (NEW.published_by IS DISTINCT FROM OLD.published_by AND NEW.published_by IS NOT NULL) THEN
        RAISE EXCEPTION 'legal_document_versions: a published or retired version is immutable: add a new version'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'legal_document_versions_frozen';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS legal_document_versions_frozen ON legal_document_versions;
CREATE TRIGGER legal_document_versions_frozen
    BEFORE UPDATE OR DELETE ON legal_document_versions
    FOR EACH ROW EXECUTE FUNCTION legal_document_versions_guard();

-- Acceptances: user_consents.
ALTER TABLE user_consents DROP CONSTRAINT IF EXISTS user_consents_type_check;
ALTER TABLE user_consents DROP CONSTRAINT IF EXISTS user_consents_type_allowed;
ALTER TABLE user_consents ADD CONSTRAINT user_consents_type_allowed
    CHECK (consent_type IN ('PRIVACY_NOTICE', 'TERMS', 'COOKIES', 'REFUND', 'EULA', 'AGENT_TERMS'));
ALTER TABLE user_consents ADD COLUMN IF NOT EXISTS market_code text;
ALTER TABLE user_consents ADD COLUMN IF NOT EXISTS context text;
ALTER TABLE user_consents DROP CONSTRAINT IF EXISTS user_consents_market_format;
ALTER TABLE user_consents ADD CONSTRAINT user_consents_market_format
    CHECK (market_code IS NULL OR market_code = 'GLOBAL' OR market_code ~ '^[A-Z]{2}$');
ALTER TABLE user_consents DROP CONSTRAINT IF EXISTS user_consents_context_allowed;
ALTER TABLE user_consents ADD CONSTRAINT user_consents_context_allowed
    CHECK (context IS NULL OR context IN ('REGISTRATION', 'REACCEPTANCE'));
CREATE INDEX IF NOT EXISTS user_consents_user_type_idx ON user_consents (user_id, consent_type, version);

-- Tax rules.
CREATE TABLE IF NOT EXISTS tax_rules (
    id                        uuid PRIMARY KEY,
    market_code               text NOT NULL REFERENCES markets (code),
    tax_name                  text NOT NULL,
    rate                      numeric(7, 4) NOT NULL,
    applies_to                text NOT NULL,
    inclusive                 boolean NOT NULL DEFAULT false,
    effective_from            timestamptz NOT NULL,
    effective_to              timestamptz,
    registration_number_label text,
    status                    text NOT NULL,
    is_sample                 boolean NOT NULL DEFAULT false,
    notes                     text NOT NULL DEFAULT '',
    created_at                timestamptz NOT NULL,
    updated_at                timestamptz NOT NULL,
    CONSTRAINT tax_rules_name_length CHECK (char_length(tax_name) BETWEEN 1 AND 40),
    CONSTRAINT tax_rules_rate_range CHECK (rate >= 0 AND rate <= 100),
    CONSTRAINT tax_rules_applies_to_allowed CHECK (applies_to IN ('SUBSCRIPTIONS', 'VENUE_BOOKINGS', 'LISTING_FEES')),
    CONSTRAINT tax_rules_period_order CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT tax_rules_registration_label_length
        CHECK (registration_number_label IS NULL OR char_length(registration_number_label) BETWEEN 1 AND 80),
    CONSTRAINT tax_rules_status_allowed CHECK (status IN ('ACTIVE', 'RETIRED')),
    CONSTRAINT tax_rules_notes_length CHECK (char_length(notes) <= 1000)
);
CREATE INDEX IF NOT EXISTS tax_rules_lookup_idx ON tax_rules (market_code, applies_to, effective_from DESC) WHERE status = 'ACTIVE';

-- Audit targets.
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW', 'LISTING_PACKAGE', 'ORGANISATION',
                           'LEGAL_DOCUMENT_VERSION', 'TAX_RULE'));
