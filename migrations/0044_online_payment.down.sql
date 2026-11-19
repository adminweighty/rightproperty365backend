-- Reverts 0044_online_payment. Payment records are DELETED: export them
-- first (GET /v1/admin/reports/payments.csv). Subscription history rows the
-- older checks do not allow (REFUNDED, actor GATEWAY) are deleted too.

DROP TABLE IF EXISTS payment_events;
DROP TABLE IF EXISTS payment_refunds;
DROP TABLE IF EXISTS payments;

DROP INDEX IF EXISTS listing_package_assignments_one_per_subscription;
ALTER TABLE listing_package_assignments DROP CONSTRAINT IF EXISTS listing_package_assignments_subscription_is_invoice;
ALTER TABLE listing_package_assignments DROP COLUMN IF EXISTS subscription_id;

DELETE FROM subscription_events WHERE action = 'REFUNDED' OR actor_type = 'GATEWAY';
ALTER TABLE subscription_events DROP CONSTRAINT IF EXISTS subscription_events_action_allowed;
ALTER TABLE subscription_events ADD CONSTRAINT subscription_events_action_check
    CHECK (action IN ('REQUESTED', 'RENEWAL_REQUESTED', 'CANCELLED', 'PAYMENT_RECORDED', 'ACTIVATED', 'REJECTED', 'EXPIRED'));
ALTER TABLE subscription_events DROP CONSTRAINT IF EXISTS subscription_events_actor_allowed;
ALTER TABLE subscription_events ADD CONSTRAINT subscription_events_actor_type_check
    CHECK (actor_type IN ('SUBSCRIBER', 'ADMIN', 'SYSTEM'));

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_payment_recorder_allowed;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS payment_provider;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS payment_recorded_by_kind;

ALTER TABLE subscription_plans DROP CONSTRAINT IF EXISTS subscription_plans_package_code_format;
ALTER TABLE subscription_plans DROP COLUMN IF EXISTS package_code;
