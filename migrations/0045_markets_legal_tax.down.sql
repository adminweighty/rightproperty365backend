-- Reverts 0045. Legal document versions and tax rules are DELETED, and so are
-- the acceptances of kinds other than the privacy notice (and the audit
-- records of both). The user_consents type check is restored under its 0002
-- name, user_consents_type_check.

DELETE FROM admin_audit_events WHERE target_type IN ('LEGAL_DOCUMENT_VERSION', 'TAX_RULE');
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW', 'LISTING_PACKAGE', 'ORGANISATION'));

DROP TABLE IF EXISTS tax_rules;

DROP INDEX IF EXISTS user_consents_user_type_idx;
DELETE FROM user_consents WHERE consent_type <> 'PRIVACY_NOTICE';
ALTER TABLE user_consents DROP CONSTRAINT IF EXISTS user_consents_context_allowed;
ALTER TABLE user_consents DROP CONSTRAINT IF EXISTS user_consents_market_format;
ALTER TABLE user_consents DROP COLUMN IF EXISTS context;
ALTER TABLE user_consents DROP COLUMN IF EXISTS market_code;
ALTER TABLE user_consents DROP CONSTRAINT IF EXISTS user_consents_type_allowed;
ALTER TABLE user_consents ADD CONSTRAINT user_consents_type_check CHECK (consent_type IN ('PRIVACY_NOTICE'));

DROP TRIGGER IF EXISTS legal_document_versions_frozen ON legal_document_versions;
DROP TABLE IF EXISTS legal_document_versions;
DROP FUNCTION IF EXISTS legal_document_versions_guard();
