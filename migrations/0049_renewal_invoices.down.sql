DELETE FROM admin_audit_events WHERE target_type = 'INVOICE';
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW', 'LISTING_PACKAGE', 'ORGANISATION',
                           'LEGAL_DOCUMENT_VERSION', 'TAX_RULE', 'DEVELOPMENT'));

DELETE FROM subscription_events WHERE action = 'INVOICED';
ALTER TABLE subscription_events DROP CONSTRAINT IF EXISTS subscription_events_action_allowed;
ALTER TABLE subscription_events ADD CONSTRAINT subscription_events_action_allowed
    CHECK (action IN ('REQUESTED', 'RENEWAL_REQUESTED', 'CANCELLED', 'PAYMENT_RECORDED', 'ACTIVATED', 'REJECTED', 'EXPIRED', 'REFUNDED'));

DROP TABLE IF EXISTS invoice_events;
DROP TABLE IF EXISTS credit_notes;
DROP TABLE IF EXISTS invoice_payments;
DROP TABLE IF EXISTS invoice_lines;
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS invoice_counters;
