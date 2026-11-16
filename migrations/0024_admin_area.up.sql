-- 0024_admin_area: the platform administrators' area (API 0.25.0).
--
--   * users: when and why an account was suspended (suspended_at,
--     suspension_reason, suspended_by) and when it was deleted (deleted_at).
--     users.status already allowed SUSPENDED and DELETED (0001). A deleted
--     account is never removed: its row is anonymised in place so that audit
--     records, signed agreements and leases keep their references.
--   * admin_audit_events: the durable audit trail of what platform
--     administrators do (suspend, reactivate and delete accounts, plans,
--     subscription decisions, reference data). Immutable, like
--     organisation_audit_events (0021): a row only ever loses its actor.
--   * subscription_plans and subscription_plan_prices: the plans professionals
--     can take, with a price per currency for a QUARTERLY and/or a YEARLY
--     period. There is no online payment: a request is invoiced, paid
--     outside the platform (for example by bank transfer) and activated by an
--     administrator who records the payment.
--   * subscriptions: PENDING_PAYMENT -> ACTIVE (payment recorded) -> EXPIRED,
--     or REJECTED / CANCELLED. A renewal is a new PENDING_PAYMENT row that
--     starts when the current one ends (renewal_of). At most one
--     PENDING_PAYMENT per subscriber.
--   * subscription_events: each subscription's immutable history.
--
-- Safe on a database with data: only new columns (all nullable), new tables,
-- a new sequence and new indexes, each created only if missing. The one
-- UPDATE backfills suspended_at for accounts that were already SUSPENDED and
-- deleted_at for accounts already DELETED (from updated_at); nothing else is
-- rewritten.

-- ---------------------------------------------------------------------------
-- Users: suspension and deletion
-- ---------------------------------------------------------------------------
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS suspended_at      timestamptz,
    ADD COLUMN IF NOT EXISTS suspension_reason text
        CONSTRAINT users_suspension_reason_length CHECK (suspension_reason IS NULL OR char_length(suspension_reason) BETWEEN 1 AND 1000),
    ADD COLUMN IF NOT EXISTS suspended_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS deleted_at        timestamptz;

UPDATE users SET suspended_at = updated_at WHERE status = 'SUSPENDED' AND suspended_at IS NULL;
UPDATE users SET deleted_at = updated_at WHERE status = 'DELETED' AND deleted_at IS NULL;

-- The administrators' user list: newest first, filtered by status.
CREATE INDEX IF NOT EXISTS users_created_idx ON users (created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS users_status_idx ON users (status);

-- ---------------------------------------------------------------------------
-- The administrators' audit trail
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS admin_audit_events (
    id            uuid PRIMARY KEY,
    actor_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    -- The actor's name when they acted, so the record stays readable.
    actor_name    text NOT NULL,
    action        text NOT NULL CONSTRAINT admin_audit_events_action_length CHECK (char_length(action) BETWEEN 1 AND 80),
    target_type   text NOT NULL CONSTRAINT admin_audit_events_target_type_check
                  CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM')),
    target_id     text NOT NULL CONSTRAINT admin_audit_events_target_id_length CHECK (char_length(target_id) BETWEEN 1 AND 200),
    target_label  text NOT NULL DEFAULT '',
    reason        text NOT NULL DEFAULT '',
    before_state  jsonb,
    after_state   jsonb,
    created_at    timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS admin_audit_events_target_idx ON admin_audit_events (target_type, target_id, created_at DESC);
CREATE INDEX IF NOT EXISTS admin_audit_events_created_idx ON admin_audit_events (created_at DESC, id DESC);

CREATE OR REPLACE FUNCTION admin_audit_events_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.actor_name IS DISTINCT FROM OLD.actor_name
       OR NEW.action IS DISTINCT FROM OLD.action
       OR NEW.target_type IS DISTINCT FROM OLD.target_type
       OR NEW.target_id IS DISTINCT FROM OLD.target_id
       OR NEW.target_label IS DISTINCT FROM OLD.target_label
       OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.before_state IS DISTINCT FROM OLD.before_state
       OR NEW.after_state IS DISTINCT FROM OLD.after_state
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (NEW.actor_user_id IS DISTINCT FROM OLD.actor_user_id AND NEW.actor_user_id IS NOT NULL) THEN
        RAISE EXCEPTION 'admin_audit_events rows are immutable';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS admin_audit_events_no_update ON admin_audit_events;
CREATE TRIGGER admin_audit_events_no_update BEFORE UPDATE ON admin_audit_events
    FOR EACH ROW EXECUTE FUNCTION admin_audit_events_immutable();

-- ---------------------------------------------------------------------------
-- Subscription plans
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS subscription_plans (
    id                 uuid PRIMARY KEY,
    name               text NOT NULL CONSTRAINT subscription_plans_name_length CHECK (char_length(name) BETWEEN 1 AND 120),
    description        text NOT NULL DEFAULT '' CONSTRAINT subscription_plans_description_length CHECK (char_length(description) <= 2000),
    -- INDIVIDUAL: taken by a professional for themselves. ORGANISATION: taken
    -- by an organisation's OWNER for the organisation.
    subscriber_kind    text NOT NULL CONSTRAINT subscription_plans_subscriber_kind_check
                       CHECK (subscriber_kind IN ('INDIVIDUAL', 'ORGANISATION')),
    -- Account role codes (approved, ACTIVE) that may take the plan.
    eligible_roles     text[] NOT NULL CONSTRAINT subscription_plans_eligible_roles_check CHECK (cardinality(eligible_roles) >= 1),
    -- [{code, limit}], limit null = unlimited (subscriptions.FeatureCodes).
    features           jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- Free-text benefits, shown as a list.
    benefits           text[] NOT NULL DEFAULT '{}',
    status             text NOT NULL DEFAULT 'ACTIVE' CONSTRAINT subscription_plans_status_check CHECK (status IN ('ACTIVE', 'RETIRED')),
    sort_order         integer NOT NULL DEFAULT 0,
    created_by         uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    retired_at         timestamptz,
    CONSTRAINT subscription_plans_retired_check CHECK ((status = 'RETIRED') = (retired_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS subscription_plans_status_idx ON subscription_plans (status, sort_order, name);

CREATE TABLE IF NOT EXISTS subscription_plan_prices (
    plan_id       uuid NOT NULL REFERENCES subscription_plans (id) ON DELETE CASCADE,
    currency_code text NOT NULL CONSTRAINT subscription_plan_prices_currency_check CHECK (currency_code ~ '^[A-Z]{3}$'),
    period        text NOT NULL CONSTRAINT subscription_plan_prices_period_check CHECK (period IN ('QUARTERLY', 'YEARLY')),
    amount        numeric(20, 4) NOT NULL CONSTRAINT subscription_plan_prices_amount_check CHECK (amount > 0),
    PRIMARY KEY (plan_id, currency_code, period)
);

-- ---------------------------------------------------------------------------
-- Subscriptions
-- ---------------------------------------------------------------------------
CREATE SEQUENCE IF NOT EXISTS subscription_invoice_seq;

CREATE TABLE IF NOT EXISTS subscriptions (
    id                   uuid PRIMARY KEY,
    -- RPS-<year>-<6+ digits>: quoted with the payment.
    invoice_number       text NOT NULL CONSTRAINT subscriptions_invoice_number_key UNIQUE
                         CONSTRAINT subscriptions_invoice_number_check CHECK (invoice_number ~ '^RPS-[0-9]{4}-[0-9]{6,}$'),
    plan_id              uuid NOT NULL REFERENCES subscription_plans (id),
    plan_name            text NOT NULL,
    period               text NOT NULL CONSTRAINT subscriptions_period_check CHECK (period IN ('QUARTERLY', 'YEARLY')),
    status               text NOT NULL CONSTRAINT subscriptions_status_check
                         CHECK (status IN ('PENDING_PAYMENT', 'ACTIVE', 'EXPIRED', 'REJECTED', 'CANCELLED')),
    -- The subscriber: a user (INDIVIDUAL) or an organisation (ORGANISATION).
    subscriber_kind      text NOT NULL CONSTRAINT subscriptions_subscriber_kind_check
                         CHECK (subscriber_kind IN ('INDIVIDUAL', 'ORGANISATION')),
    subscriber_id        uuid NOT NULL,
    subscriber_name      text NOT NULL,
    organisation_id      uuid REFERENCES organisations (id) ON DELETE SET NULL,
    -- Who asked for it (the professional, or the organisation's owner).
    requested_by         uuid REFERENCES users (id) ON DELETE SET NULL,
    requested_by_name    text NOT NULL DEFAULT '',
    market_code          text NOT NULL CONSTRAINT subscriptions_market_code_check CHECK (market_code ~ '^[A-Z]{2}$'),
    currency_code        text NOT NULL CONSTRAINT subscriptions_currency_check CHECK (currency_code ~ '^[A-Z]{3}$'),
    -- The invoice amount (the plan's price when it was requested).
    amount               numeric(20, 4) NOT NULL CONSTRAINT subscriptions_amount_check CHECK (amount > 0),
    renewal_of           uuid REFERENCES subscriptions (id),
    starts_at            timestamptz,
    ends_at              timestamptz,
    activated_at         timestamptz,
    -- The payment the administrator recorded.
    payment_reference    text,
    payment_amount       numeric(20, 4) CONSTRAINT subscriptions_payment_amount_check CHECK (payment_amount IS NULL OR payment_amount > 0),
    payment_received_on  date,
    payment_note         text NOT NULL DEFAULT '',
    payment_recorded_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    payment_recorded_by_name text NOT NULL DEFAULT '',
    rejected_at          timestamptz,
    rejection_reason     text NOT NULL DEFAULT '',
    cancelled_at         timestamptz,
    cancel_reason        text NOT NULL DEFAULT '',
    cancelled_by_kind    text CONSTRAINT subscriptions_cancelled_by_check CHECK (cancelled_by_kind IN ('SUBSCRIBER', 'ADMIN', 'SYSTEM')),
    -- Admin cancellation of an active subscription: whether a pro-rata
    -- refund is due under the refund policy, and a note. No money moves here.
    refund_due           boolean NOT NULL DEFAULT false,
    refund_note          text NOT NULL DEFAULT '',
    expired_at           timestamptz,
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    CONSTRAINT subscriptions_active_check CHECK (status NOT IN ('ACTIVE', 'EXPIRED') OR
        (starts_at IS NOT NULL AND ends_at IS NOT NULL AND activated_at IS NOT NULL AND payment_amount IS NOT NULL)),
    CONSTRAINT subscriptions_period_order_check CHECK (ends_at IS NULL OR starts_at IS NULL OR ends_at > starts_at),
    -- organisation_id is the subscriber of an ORGANISATION subscription (it
    -- only becomes NULL if the organisation row is ever removed).
    CONSTRAINT subscriptions_organisation_check CHECK (subscriber_kind = 'ORGANISATION' OR organisation_id IS NULL)
);
-- At most one subscription waiting for payment per subscriber.
CREATE UNIQUE INDEX IF NOT EXISTS subscriptions_one_pending_key ON subscriptions (subscriber_kind, subscriber_id)
    WHERE status = 'PENDING_PAYMENT';
CREATE INDEX IF NOT EXISTS subscriptions_subscriber_idx ON subscriptions (subscriber_kind, subscriber_id, created_at DESC);
CREATE INDEX IF NOT EXISTS subscriptions_status_idx ON subscriptions (status, ends_at);
CREATE INDEX IF NOT EXISTS subscriptions_created_idx ON subscriptions (created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS subscriptions_payment_idx ON subscriptions (payment_received_on) WHERE payment_received_on IS NOT NULL;

CREATE TABLE IF NOT EXISTS subscription_events (
    id              uuid PRIMARY KEY,
    -- Insertion order: several events of one change share a timestamp.
    seq             bigint GENERATED ALWAYS AS IDENTITY,
    subscription_id uuid NOT NULL REFERENCES subscriptions (id) ON DELETE CASCADE,
    action          text NOT NULL CONSTRAINT subscription_events_action_check
                    CHECK (action IN ('REQUESTED', 'RENEWAL_REQUESTED', 'CANCELLED', 'PAYMENT_RECORDED', 'ACTIVATED', 'REJECTED', 'EXPIRED')),
    actor_type      text NOT NULL CONSTRAINT subscription_events_actor_type_check CHECK (actor_type IN ('SUBSCRIBER', 'ADMIN', 'SYSTEM')),
    actor_user_id   uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_name      text NOT NULL DEFAULT '',
    note            text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS subscription_events_subscription_idx ON subscription_events (subscription_id, created_at);

CREATE OR REPLACE FUNCTION subscription_events_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.subscription_id IS DISTINCT FROM OLD.subscription_id
       OR NEW.action IS DISTINCT FROM OLD.action
       OR NEW.actor_type IS DISTINCT FROM OLD.actor_type
       OR NEW.actor_name IS DISTINCT FROM OLD.actor_name
       OR NEW.note IS DISTINCT FROM OLD.note
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (NEW.actor_user_id IS DISTINCT FROM OLD.actor_user_id AND NEW.actor_user_id IS NOT NULL) THEN
        RAISE EXCEPTION 'subscription_events rows are immutable';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS subscription_events_no_update ON subscription_events;
CREATE TRIGGER subscription_events_no_update BEFORE UPDATE ON subscription_events
    FOR EACH ROW EXECUTE FUNCTION subscription_events_immutable();
