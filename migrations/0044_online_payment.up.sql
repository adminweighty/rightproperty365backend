-- 0044: online payment of subscription invoices through a provider-neutral
-- gateway (API 0.42.0, BR-015, FR-037-040).
--
--   * payments: one row per checkout of a subscription invoice: the provider
--     (PAYMENT_GATEWAY), its checkout reference (provider_ref) and payment
--     reference (payment_ref), the invoice amount and currency, what the
--     provider reported as paid, the refunded total, the status (OPEN,
--     SUCCEEDED, FAILED, EXPIRED, REVIEW, PARTIALLY_REFUNDED, REFUNDED) and
--     its timestamps. At most one OPEN checkout per subscription
--     (payments_one_open_per_subscription). These rows and the admin audit are
--     the platform's finance record; the agent ledger is never written.
--   * payment_refunds: refunds of a payment (PENDING, SUCCEEDED, FAILED), each
--     with its own idempotency key and the provider's refund reference.
--   * payment_events: every verified webhook event, unique per provider and
--     event id (payment_events_one_per_provider_event): a repeat is a no-op.
--     processing_started_at is a short lease; processed_at marks it done.
--   * subscription_plans.package_code: the listing package a paid
--     subscription grants (by code, in the subscription's market).
--   * subscriptions.payment_recorded_by_kind (ADMIN or GATEWAY) and
--     payment_provider.
--   * subscription_events: the action REFUNDED and the actor GATEWAY (the
--     CHECKs are re-created under new names).
--   * listing_package_assignments.subscription_id: the subscription an
--     INVOICE assignment comes from (one assignment per subscription).
--
-- Constraint names: inline CHECKs get Postgres's <table>_<column>_check; the
-- table-level constraints are named differently on purpose.
--
-- User references are ON DELETE SET NULL; users are anonymised on account
-- deletion, never deleted, so the accounts module unlinks them explicitly
-- (payments.ForgetUserTx) and the payments are kept for finance.
--
-- Safe on a database with data: new tables, new nullable or defaulted
-- columns, and wider checks.

ALTER TABLE subscription_plans ADD COLUMN IF NOT EXISTS package_code text;
ALTER TABLE subscription_plans DROP CONSTRAINT IF EXISTS subscription_plans_package_code_format;
ALTER TABLE subscription_plans ADD CONSTRAINT subscription_plans_package_code_format
    CHECK (package_code IS NULL OR package_code ~ '^[A-Z][A-Z0-9_]{1,31}$');

ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS payment_recorded_by_kind text NOT NULL DEFAULT 'ADMIN';
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS payment_provider text NOT NULL DEFAULT '';
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_payment_recorder_allowed;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_payment_recorder_allowed
    CHECK (payment_recorded_by_kind IN ('ADMIN', 'GATEWAY'));

ALTER TABLE subscription_events DROP CONSTRAINT IF EXISTS subscription_events_action_check;
ALTER TABLE subscription_events DROP CONSTRAINT IF EXISTS subscription_events_action_allowed;
ALTER TABLE subscription_events ADD CONSTRAINT subscription_events_action_allowed
    CHECK (action IN ('REQUESTED', 'RENEWAL_REQUESTED', 'CANCELLED', 'PAYMENT_RECORDED', 'ACTIVATED', 'REJECTED', 'EXPIRED', 'REFUNDED'));
ALTER TABLE subscription_events DROP CONSTRAINT IF EXISTS subscription_events_actor_type_check;
ALTER TABLE subscription_events DROP CONSTRAINT IF EXISTS subscription_events_actor_allowed;
ALTER TABLE subscription_events ADD CONSTRAINT subscription_events_actor_allowed
    CHECK (actor_type IN ('SUBSCRIBER', 'ADMIN', 'SYSTEM', 'GATEWAY'));

ALTER TABLE listing_package_assignments ADD COLUMN IF NOT EXISTS subscription_id uuid REFERENCES subscriptions (id) ON DELETE SET NULL;
ALTER TABLE listing_package_assignments DROP CONSTRAINT IF EXISTS listing_package_assignments_subscription_is_invoice;
ALTER TABLE listing_package_assignments ADD CONSTRAINT listing_package_assignments_subscription_is_invoice
    CHECK (subscription_id IS NULL OR source = 'INVOICE');
CREATE UNIQUE INDEX IF NOT EXISTS listing_package_assignments_one_per_subscription
    ON listing_package_assignments (subscription_id) WHERE subscription_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS payments (
    id                 uuid PRIMARY KEY,
    subscription_id    uuid NOT NULL REFERENCES subscriptions (id) ON DELETE RESTRICT,
    invoice_number     text NOT NULL,
    subscriber_kind    text NOT NULL,
    subscriber_id      uuid NOT NULL,
    subscriber_name    text NOT NULL DEFAULT '',
    market_code        text NOT NULL,
    provider           text NOT NULL,
    provider_ref       text,
    payment_ref        text,
    idempotency_key    text NOT NULL,
    amount             numeric(20, 4) NOT NULL,
    currency_code      text NOT NULL,
    paid_amount        numeric(20, 4),
    paid_currency_code text,
    refunded_amount    numeric(20, 4) NOT NULL DEFAULT 0,
    status             text NOT NULL,
    hosted_url         text NOT NULL DEFAULT '',
    expires_at         timestamptz,
    return_url         text NOT NULL DEFAULT '',
    cancel_url         text NOT NULL DEFAULT '',
    review_reason      text NOT NULL DEFAULT '',
    failure_reason     text NOT NULL DEFAULT '',
    opened_by          uuid REFERENCES users (id) ON DELETE SET NULL,
    opened_by_name     text NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    succeeded_at       timestamptz,
    failed_at          timestamptz,
    expired_at         timestamptz,
    refunded_at        timestamptz,
    CONSTRAINT payments_status_allowed
        CHECK (status IN ('OPEN', 'SUCCEEDED', 'FAILED', 'EXPIRED', 'REVIEW', 'PARTIALLY_REFUNDED', 'REFUNDED')),
    CONSTRAINT payments_subscriber_kind_allowed CHECK (subscriber_kind IN ('INDIVIDUAL', 'ORGANISATION')),
    CONSTRAINT payments_amount_positive CHECK (amount > 0),
    CONSTRAINT payments_paid_positive CHECK (paid_amount IS NULL OR paid_amount > 0),
    CONSTRAINT payments_refunded_in_range CHECK (refunded_amount >= 0 AND (paid_amount IS NULL OR refunded_amount <= paid_amount)),
    CONSTRAINT payments_currency_format CHECK (currency_code ~ '^[A-Z]{3}$'),
    CONSTRAINT payments_paid_currency_format CHECK (paid_currency_code IS NULL OR paid_currency_code ~ '^[A-Z]{3}$'),
    CONSTRAINT payments_provider_format CHECK (provider ~ '^[a-z][a-z0-9_]{1,31}$'),
    CONSTRAINT payments_idempotency_key_unique UNIQUE (idempotency_key),
    CONSTRAINT payments_provider_ref_unique UNIQUE (provider, provider_ref)
);
CREATE UNIQUE INDEX IF NOT EXISTS payments_one_open_per_subscription ON payments (subscription_id) WHERE status = 'OPEN';
CREATE INDEX IF NOT EXISTS payments_subscription_idx ON payments (subscription_id, created_at DESC);
CREATE INDEX IF NOT EXISTS payments_status_idx ON payments (status, created_at);
CREATE INDEX IF NOT EXISTS payments_created_idx ON payments (created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS payments_payment_ref_idx ON payments (provider, payment_ref) WHERE payment_ref IS NOT NULL;

CREATE TABLE IF NOT EXISTS payment_refunds (
    id                   uuid PRIMARY KEY,
    payment_id           uuid NOT NULL REFERENCES payments (id) ON DELETE RESTRICT,
    amount               numeric(20, 4) NOT NULL,
    currency_code        text NOT NULL,
    reason               text NOT NULL DEFAULT '',
    status               text NOT NULL,
    provider_refund_ref  text,
    idempotency_key      text NOT NULL,
    requested_by         uuid REFERENCES users (id) ON DELETE SET NULL,
    requested_by_name    text NOT NULL DEFAULT '',
    failure_reason       text NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL,
    completed_at         timestamptz,
    CONSTRAINT payment_refunds_status_allowed CHECK (status IN ('PENDING', 'SUCCEEDED', 'FAILED')),
    CONSTRAINT payment_refunds_amount_positive CHECK (amount > 0),
    CONSTRAINT payment_refunds_currency_format CHECK (currency_code ~ '^[A-Z]{3}$'),
    CONSTRAINT payment_refunds_reason_length CHECK (char_length(reason) <= 1000),
    CONSTRAINT payment_refunds_idempotency_key_unique UNIQUE (idempotency_key),
    CONSTRAINT payment_refunds_provider_ref_unique UNIQUE (payment_id, provider_refund_ref)
);
CREATE INDEX IF NOT EXISTS payment_refunds_payment_idx ON payment_refunds (payment_id, created_at);

CREATE TABLE IF NOT EXISTS payment_events (
    id                    uuid PRIMARY KEY,
    provider              text NOT NULL,
    event_id              text NOT NULL,
    event_type            text NOT NULL,
    provider_ref          text NOT NULL DEFAULT '',
    payment_ref           text NOT NULL DEFAULT '',
    refund_ref            text NOT NULL DEFAULT '',
    amount                numeric(20, 4),
    currency_code         text NOT NULL DEFAULT '',
    occurred_at           timestamptz,
    received_at           timestamptz NOT NULL,
    payment_id            uuid REFERENCES payments (id) ON DELETE SET NULL,
    outcome               text NOT NULL DEFAULT '',
    processing_started_at timestamptz,
    processed_at          timestamptz,
    CONSTRAINT payment_events_event_id_length CHECK (char_length(event_id) BETWEEN 1 AND 200),
    CONSTRAINT payment_events_one_per_provider_event UNIQUE (provider, event_id)
);
CREATE INDEX IF NOT EXISTS payment_events_payment_idx ON payment_events (payment_id, received_at);
