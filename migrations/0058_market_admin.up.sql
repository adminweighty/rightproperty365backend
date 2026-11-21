-- API 0.56.0: market administration (BR-001).
ALTER TABLE markets
    ADD COLUMN extra_currencies     text[] NOT NULL DEFAULT '{}',
    ADD COLUMN default_package_code text,
    ADD COLUMN notice               text CHECK (char_length(notice) <= 280);

-- The status now means something: ACTIVE is public (enabled), PLANNED is
-- provisioned and SUSPENDED switched off (both hidden). Existing rows follow
-- their enabled flag.
UPDATE markets SET status = CASE WHEN enabled THEN 'ACTIVE' ELSE 'SUSPENDED' END;
ALTER TABLE markets ADD CONSTRAINT markets_enabled_follows_status CHECK (enabled = (status = 'ACTIVE'));

-- Market changes are audited against the market.
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW', 'LISTING_PACKAGE', 'ORGANISATION',
                           'LEGAL_DOCUMENT_VERSION', 'TAX_RULE', 'DEVELOPMENT', 'INVOICE', 'LISTING', 'MARKET'));
