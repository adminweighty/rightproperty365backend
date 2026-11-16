-- Reverts 0024_admin_area.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every subscription plan, price, subscription and its history, the
-- administrators' audit trail, and the suspension reason and times of
-- accounts. Accounts keep their status: a SUSPENDED account stays suspended
-- and a DELETED (anonymised) account stays deleted; rolling back never gives
-- anyone their access back or their data back.
DROP TRIGGER IF EXISTS subscription_events_no_update ON subscription_events;
DROP FUNCTION IF EXISTS subscription_events_immutable();
DROP INDEX IF EXISTS subscription_events_subscription_idx;
DROP TABLE IF EXISTS subscription_events;

DROP INDEX IF EXISTS subscriptions_payment_idx;
DROP INDEX IF EXISTS subscriptions_created_idx;
DROP INDEX IF EXISTS subscriptions_status_idx;
DROP INDEX IF EXISTS subscriptions_subscriber_idx;
DROP INDEX IF EXISTS subscriptions_one_pending_key;
DROP TABLE IF EXISTS subscriptions;
DROP SEQUENCE IF EXISTS subscription_invoice_seq;

DROP TABLE IF EXISTS subscription_plan_prices;
DROP INDEX IF EXISTS subscription_plans_status_idx;
DROP TABLE IF EXISTS subscription_plans;

DROP TRIGGER IF EXISTS admin_audit_events_no_update ON admin_audit_events;
DROP FUNCTION IF EXISTS admin_audit_events_immutable();
DROP INDEX IF EXISTS admin_audit_events_created_idx;
DROP INDEX IF EXISTS admin_audit_events_target_idx;
DROP TABLE IF EXISTS admin_audit_events;

DROP INDEX IF EXISTS users_status_idx;
DROP INDEX IF EXISTS users_created_idx;
ALTER TABLE users
    DROP COLUMN IF EXISTS deleted_at,
    DROP COLUMN IF EXISTS suspended_by,
    DROP COLUMN IF EXISTS suspension_reason,
    DROP COLUMN IF EXISTS suspended_at;
