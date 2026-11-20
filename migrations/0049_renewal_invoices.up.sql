-- 0049: admin-issued renewal invoices (API 0.47.0, BR-015, FR-037, FR-038,
-- FR-039/040, Handover section 10).
--
--   * invoices: a finance administrator's invoice for the renewal of a
--     subscription. DRAFT (no number, editable, can be discarded) ->
--     ISSUED (numbered, immutable, emailed) -> PART_PAID / PAID / OVERDUE /
--     VOID / DISPUTED. The FSD's SENT and PENDING_PAYMENT are ISSUED plus the
--     delivery fields (sent_at, last_sent_at, send_count). Issuing creates
--     the renewal subscription (PENDING_PAYMENT, subscription_id) that the
--     payment pays; settlement activates it. Money is numeric(12,2) like
--     subscriptions; amount_paid and amount_credited are kept on the row.
--   * invoice_lines: the plan period and any extra lines a draft has.
--   * invoice_payments: full or part payments, each with a receipt number;
--     a gateway payment is unique per invoice and source key (idempotent).
--   * credit_notes: corrections of an issued invoice, numbered.
--   * invoice_events: the history (never edited).
--   * invoice_counters: gap-free numbers per kind (INV, RCT, CRN), market
--     and year, taken under a row lock in the issuing transaction.
--   * subscription_events gains INVOICED: the renewal subscription an invoice
--     created (no subscription-invoice email is sent for it; the invoice's
--     own renewal-invoice email is).
--   * admin_audit_events gains the INVOICE target.

CREATE TABLE IF NOT EXISTS invoice_counters (
    kind        text    NOT NULL CONSTRAINT invoice_counters_kind_check CHECK (kind IN ('INV', 'RCT', 'CRN')),
    market_code text    NOT NULL CONSTRAINT invoice_counters_market_check CHECK (market_code ~ '^[A-Z]{2}$'),
    year        integer NOT NULL CONSTRAINT invoice_counters_year_check CHECK (year BETWEEN 2000 AND 2999),
    last        integer NOT NULL DEFAULT 0,
    PRIMARY KEY (kind, market_code, year)
);

CREATE TABLE IF NOT EXISTS invoices (
    id                     uuid PRIMARY KEY,
    number                 text UNIQUE CONSTRAINT invoices_number_format CHECK (number ~ '^INV-[A-Z]{2}-[0-9]{4}-[0-9]{6,}$'),
    status                 text NOT NULL CONSTRAINT invoices_status_check
                           CHECK (status IN ('DRAFT', 'ISSUED', 'PART_PAID', 'PAID', 'OVERDUE', 'VOID', 'DISPUTED')),
    status_before_dispute  text CONSTRAINT invoices_status_before_dispute_check
                           CHECK (status_before_dispute IN ('ISSUED', 'PART_PAID', 'PAID', 'OVERDUE')),
    market_code            text NOT NULL,
    currency_code          text NOT NULL,
    subscriber_kind        text NOT NULL CONSTRAINT invoices_subscriber_kind_check CHECK (subscriber_kind IN ('INDIVIDUAL', 'ORGANISATION')),
    subscriber_id          uuid NOT NULL,
    organisation_id        uuid REFERENCES organisations (id),
    payer_name             text NOT NULL,
    renews_subscription_id uuid NOT NULL REFERENCES subscriptions (id),
    subscription_id        uuid UNIQUE REFERENCES subscriptions (id),
    plan_id                uuid NOT NULL,
    plan_name              text NOT NULL,
    period                 text NOT NULL CONSTRAINT invoices_period_check CHECK (period IN ('QUARTERLY', 'YEARLY')),
    service_from           date NOT NULL,
    service_to             date NOT NULL,
    issue_on               date,
    due_on                 date NOT NULL,
    terms                  text NOT NULL DEFAULT '' CONSTRAINT invoices_terms_length CHECK (char_length(terms) <= 2000),
    subtotal               numeric(12,2) NOT NULL CONSTRAINT invoices_subtotal_positive CHECK (subtotal > 0),
    tax_name               text,
    tax_rate               numeric(7,4),
    tax_inclusive          boolean,
    tax_amount             numeric(12,2) NOT NULL DEFAULT 0,
    tax_is_sample          boolean NOT NULL DEFAULT false,
    total                  numeric(12,2) NOT NULL CONSTRAINT invoices_total_positive CHECK (total > 0),
    amount_paid            numeric(12,2) NOT NULL DEFAULT 0 CONSTRAINT invoices_paid_non_negative CHECK (amount_paid >= 0),
    amount_credited        numeric(12,2) NOT NULL DEFAULT 0 CONSTRAINT invoices_credited_non_negative CHECK (amount_credited >= 0),
    void_reason            text,
    dispute_reason         text,
    replaces_invoice_id    uuid REFERENCES invoices (id),
    sent_at                timestamptz,
    last_sent_at           timestamptz,
    send_count             integer NOT NULL DEFAULT 0,
    overdue_notified_at    timestamptz,
    created_by             uuid REFERENCES users (id),
    created_by_name        text NOT NULL DEFAULT '',
    issued_by              uuid REFERENCES users (id),
    created_at             timestamptz NOT NULL,
    updated_at             timestamptz NOT NULL,
    CONSTRAINT invoices_numbered_unless_draft CHECK ((status = 'DRAFT') = (number IS NULL)),
    CONSTRAINT invoices_paid_within_total CHECK (amount_paid + amount_credited <= total),
    CONSTRAINT invoices_service_order CHECK (service_to > service_from)
);
CREATE INDEX IF NOT EXISTS invoices_status_due_idx ON invoices (status, due_on);
CREATE INDEX IF NOT EXISTS invoices_subscriber_idx ON invoices (subscriber_kind, subscriber_id, created_at DESC);
CREATE INDEX IF NOT EXISTS invoices_renews_idx ON invoices (renews_subscription_id);

CREATE TABLE IF NOT EXISTS invoice_lines (
    id          uuid PRIMARY KEY,
    invoice_id  uuid NOT NULL REFERENCES invoices (id) ON DELETE CASCADE,
    position    integer NOT NULL,
    description text NOT NULL CONSTRAINT invoice_lines_description_length CHECK (char_length(description) BETWEEN 1 AND 300),
    quantity    integer NOT NULL CONSTRAINT invoice_lines_quantity_positive CHECK (quantity BETWEEN 1 AND 1000),
    unit_amount numeric(12,2) NOT NULL CONSTRAINT invoice_lines_unit_positive CHECK (unit_amount > 0),
    amount      numeric(12,2) NOT NULL
);
CREATE INDEX IF NOT EXISTS invoice_lines_invoice_idx ON invoice_lines (invoice_id, position);

CREATE TABLE IF NOT EXISTS invoice_payments (
    id             uuid PRIMARY KEY,
    invoice_id     uuid NOT NULL REFERENCES invoices (id),
    receipt_number text NOT NULL UNIQUE CONSTRAINT invoice_payments_receipt_format CHECK (receipt_number ~ '^RCT-[A-Z]{2}-[0-9]{4}-[0-9]{6,}$'),
    amount         numeric(12,2) NOT NULL CONSTRAINT invoice_payments_amount_positive CHECK (amount > 0),
    method         text NOT NULL CONSTRAINT invoice_payments_method_check
                   CHECK (method IN ('BANK_TRANSFER', 'CASH', 'CARD_OFFLINE', 'GATEWAY', 'OTHER')),
    reference      text NOT NULL DEFAULT '',
    received_on    date NOT NULL,
    source_key     text,
    recorded_by    uuid REFERENCES users (id),
    recorded_by_name text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL,
    CONSTRAINT invoice_payments_source_unique UNIQUE (invoice_id, source_key)
);
CREATE INDEX IF NOT EXISTS invoice_payments_invoice_idx ON invoice_payments (invoice_id, created_at);

CREATE TABLE IF NOT EXISTS credit_notes (
    id           uuid PRIMARY KEY,
    invoice_id   uuid NOT NULL REFERENCES invoices (id),
    number       text NOT NULL UNIQUE CONSTRAINT credit_notes_number_format CHECK (number ~ '^CRN-[A-Z]{2}-[0-9]{4}-[0-9]{6,}$'),
    amount       numeric(12,2) NOT NULL CONSTRAINT credit_notes_amount_positive CHECK (amount > 0),
    reason       text NOT NULL CONSTRAINT credit_notes_reason_length CHECK (char_length(reason) BETWEEN 3 AND 1000),
    created_by   uuid REFERENCES users (id),
    created_by_name text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS credit_notes_invoice_idx ON credit_notes (invoice_id, created_at);

CREATE TABLE IF NOT EXISTS invoice_events (
    id          uuid PRIMARY KEY,
    invoice_id  uuid NOT NULL REFERENCES invoices (id) ON DELETE CASCADE,
    action      text NOT NULL CONSTRAINT invoice_events_action_check CHECK (action IN (
                    'CREATED', 'EDITED', 'ISSUED', 'RESENT', 'PAYMENT_RECORDED', 'CREDIT_NOTE', 'OVERDUE', 'VOIDED',
                    'DISPUTED', 'DISPUTE_RESOLVED', 'REISSUED', 'PAID')),
    actor_type  text NOT NULL CONSTRAINT invoice_events_actor_check CHECK (actor_type IN ('ADMIN', 'SYSTEM', 'GATEWAY')),
    actor_user_id uuid,
    actor_name  text NOT NULL DEFAULT '',
    note        text NOT NULL DEFAULT '',
    at          timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS invoice_events_invoice_idx ON invoice_events (invoice_id, at);

ALTER TABLE subscription_events DROP CONSTRAINT IF EXISTS subscription_events_action_allowed;
ALTER TABLE subscription_events ADD CONSTRAINT subscription_events_action_allowed
    CHECK (action IN ('REQUESTED', 'RENEWAL_REQUESTED', 'CANCELLED', 'PAYMENT_RECORDED', 'ACTIVATED', 'REJECTED', 'EXPIRED', 'REFUNDED',
                      'INVOICED'));

ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW', 'LISTING_PACKAGE', 'ORGANISATION',
                           'LEGAL_DOCUMENT_VERSION', 'TAX_RULE', 'DEVELOPMENT', 'INVOICE'));
