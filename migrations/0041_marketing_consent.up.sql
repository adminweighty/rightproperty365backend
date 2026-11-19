-- 0041: marketing consent records (API 0.37.1, BR-017, FR-003).
--
--   * marketing_consent_events: one row per change of users.marketing_opt_in,
--     append-only: never updated (marketing_consent_events_no_update), and
--     deleted only by account deletion (the accounts module deletes them
--     explicitly; users rows are anonymised, never deleted, so the cascade
--     below never fires in practice).
--       - opt_in: the new value; changed_at: when (UTC).
--       - source: web | app | api, from the X-Client header.
--       - context: REGISTRATION | PREFERENCES | MIGRATION | ADMIN |
--         UNSUBSCRIBE (ADMIN and UNSUBSCRIBE are reserved; nothing writes
--         them in 0.37.1).
--       - consent_text_version: the opt-in wording agreed to; set on opt-in
--         only (marketing_consent_events_version_on_opt_in), except MIGRATION
--         records, which have none.
--     No IP address or user agent is stored.
--   * marketing_consent_texts: the opt-in wording per version (version,
--     text, status DRAFT | APPROVED, created_at). Rows are immutable
--     (marketing_consent_texts_immutable refuses UPDATE and DELETE): a change
--     of wording is always a new version. The current version is named by
--     identity.CurrentMarketingConsentVersion. marketing-2026-01 is inserted
--     here (production needs it too) as DRAFT: it awaits the owner's legal
--     wording; approving it means inserting a new version with status
--     APPROVED and pointing the constant at it.
--   * Backfill: every account already opted in gets one MIGRATION record,
--     dated at the account's created_at, source api, no text version.
--
-- Constraint names: every constraint is named explicitly and never takes
-- Postgres's <table>_<column>_check form.
--
-- Safe on a database with data: new tables, triggers and inserts that skip
-- rows already present (re-runnable).

CREATE TABLE IF NOT EXISTS marketing_consent_texts (
    version    text PRIMARY KEY,
    text       text NOT NULL,
    status     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT marketing_consent_texts_version_length CHECK (char_length(version) BETWEEN 1 AND 40),
    CONSTRAINT marketing_consent_texts_text_length CHECK (char_length(text) BETWEEN 1 AND 4000),
    CONSTRAINT marketing_consent_texts_status_allowed CHECK (status IN ('DRAFT', 'APPROVED'))
);

CREATE OR REPLACE FUNCTION marketing_consent_texts_refuse_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'marketing_consent_texts rows are immutable: add a new version'
        USING ERRCODE = 'check_violation', CONSTRAINT = 'marketing_consent_texts_immutable';
END
$$;

DROP TRIGGER IF EXISTS marketing_consent_texts_immutable ON marketing_consent_texts;
CREATE TRIGGER marketing_consent_texts_immutable
    BEFORE UPDATE OR DELETE ON marketing_consent_texts
    FOR EACH ROW EXECUTE FUNCTION marketing_consent_texts_refuse_change();

INSERT INTO marketing_consent_texts (version, text, status)
VALUES ('marketing-2026-01',
        'Send me news, tips and offers from RightProperty365 by email. I can stop them at any time.',
        'DRAFT')
ON CONFLICT (version) DO NOTHING;

CREATE TABLE IF NOT EXISTS marketing_consent_events (
    id                   uuid PRIMARY KEY,
    user_id              uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    opt_in               boolean NOT NULL,
    changed_at           timestamptz NOT NULL,
    source               text NOT NULL,
    context              text NOT NULL,
    consent_text_version text NULL,
    CONSTRAINT marketing_consent_events_source_allowed CHECK (source IN ('web', 'app', 'api')),
    CONSTRAINT marketing_consent_events_context_allowed
        CHECK (context IN ('REGISTRATION', 'PREFERENCES', 'MIGRATION', 'ADMIN', 'UNSUBSCRIBE')),
    CONSTRAINT marketing_consent_events_version_length
        CHECK (consent_text_version IS NULL OR char_length(consent_text_version) BETWEEN 1 AND 40),
    CONSTRAINT marketing_consent_events_version_on_opt_in CHECK (opt_in OR consent_text_version IS NULL)
);
CREATE INDEX IF NOT EXISTS marketing_consent_events_by_user
    ON marketing_consent_events (user_id, changed_at DESC, id DESC);

CREATE OR REPLACE FUNCTION marketing_consent_events_refuse_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'marketing_consent_events is append-only'
        USING ERRCODE = 'check_violation', CONSTRAINT = 'marketing_consent_events_no_update';
END
$$;

DROP TRIGGER IF EXISTS marketing_consent_events_no_update ON marketing_consent_events;
CREATE TRIGGER marketing_consent_events_no_update
    BEFORE UPDATE ON marketing_consent_events
    FOR EACH ROW EXECUTE FUNCTION marketing_consent_events_refuse_update();

INSERT INTO marketing_consent_events (id, user_id, opt_in, changed_at, source, context, consent_text_version)
SELECT gen_random_uuid(), u.id, true, u.created_at, 'api', 'MIGRATION', NULL
FROM users u
WHERE u.marketing_opt_in
  AND NOT EXISTS (SELECT 1 FROM marketing_consent_events e WHERE e.user_id = u.id);
