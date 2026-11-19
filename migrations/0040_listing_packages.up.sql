-- 0040: listing packages and quotas (API 0.37.0, FR-008, BR/FR-007).
--
--   * listing_packages: the packages of one market (Starter, Business,
--     Professional ...): a code unique per market, a name, a description, the
--     number of active published listings it allows (listing_limit), whether
--     it is offered (active), its order, and whether it is the market's
--     default (at most one per market, listing_packages_one_default): the
--     package of every account that holds no assignment there. A market with
--     no default is not limited.
--   * listing_package_prices: a package's price per billing term (MONTHLY,
--     QUARTERLY, ANNUAL) in the market's currency, as an exact decimal.
--     `sample` marks the development seed's sample prices; a price an
--     administrator enters is never a sample.
--   * listing_package_assignments: the package a listing account holds in a
--     market, from starts_on to ends_on (inclusive, UTC dates; ends_on NULL
--     is open-ended). The account is an organisation or a direct owner (a
--     user), exactly one (listing_package_assignments_one_account). source
--     is ADMIN (an administrator) or INVOICE (reserved for paid invoices,
--     item G). A replaced assignment is ended the day before or cancelled
--     (cancelled_at); the service keeps at most one in force per account,
--     market and date under the same advisory lock the listing approval
--     takes.
--   * admin_audit_events gains the targets LISTING_PACKAGE and ORGANISATION.
--     Its target CHECK keeps the name admin_audit_events_target_type_allowed
--     (re-created wider).
--
-- Nothing is seeded here: the packages and their (sample) prices are the
-- development seed's (internal/devseed), and administrators enter the real
-- ones. A market without packages is not limited.
--
-- Constraint names: inline CHECKs get Postgres's <table>_<column>_check;
-- the table-level constraints below are named differently on purpose.
--
-- User references are ON DELETE SET NULL; users are anonymised on account
-- deletion, never deleted, so the accounts module handles them explicitly
-- (packages.ForgetUserTx).
--
-- Safe on a database with data: new tables and a wider check.

CREATE TABLE IF NOT EXISTS listing_packages (
    id            uuid PRIMARY KEY,
    market_code   text NOT NULL REFERENCES markets (code) ON DELETE RESTRICT,
    code          text NOT NULL CHECK (code ~ '^[A-Z][A-Z0-9_]{1,31}$'),
    name          text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    description   text NOT NULL DEFAULT '' CHECK (char_length(description) <= 1000),
    listing_limit integer NOT NULL CHECK (listing_limit BETWEEN 0 AND 100000),
    active        boolean NOT NULL DEFAULT true,
    is_default    boolean NOT NULL DEFAULT false,
    sort_order    integer NOT NULL DEFAULT 100 CHECK (sort_order BETWEEN 0 AND 10000),
    created_by    uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    CONSTRAINT listing_packages_code_per_market UNIQUE (market_code, code),
    CONSTRAINT listing_packages_default_is_active CHECK (NOT is_default OR active)
);
CREATE UNIQUE INDEX IF NOT EXISTS listing_packages_one_default ON listing_packages (market_code) WHERE is_default;

CREATE TABLE IF NOT EXISTS listing_package_prices (
    package_id    uuid NOT NULL REFERENCES listing_packages (id) ON DELETE CASCADE,
    billing_term  text NOT NULL CHECK (billing_term IN ('MONTHLY', 'QUARTERLY', 'ANNUAL')),
    amount        numeric(18, 4) NOT NULL CHECK (amount > 0),
    currency_code text NOT NULL CHECK (currency_code ~ '^[A-Z]{3}$'),
    sample        boolean NOT NULL DEFAULT false,
    CONSTRAINT listing_package_prices_one_per_term PRIMARY KEY (package_id, billing_term)
);

CREATE TABLE IF NOT EXISTS listing_package_assignments (
    id               uuid PRIMARY KEY,
    organisation_id  uuid REFERENCES organisations (id) ON DELETE CASCADE,
    user_id          uuid REFERENCES users (id) ON DELETE CASCADE,
    market_code      text NOT NULL REFERENCES markets (code) ON DELETE RESTRICT,
    package_id       uuid NOT NULL REFERENCES listing_packages (id) ON DELETE RESTRICT,
    starts_on        date NOT NULL,
    ends_on          date,
    source           text NOT NULL CHECK (source IN ('ADMIN', 'INVOICE')),
    note             text NOT NULL DEFAULT '' CHECK (char_length(note) <= 500),
    assigned_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    assigned_by_name text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL,
    cancelled_at     timestamptz,
    cancelled_by     uuid REFERENCES users (id) ON DELETE SET NULL,
    CONSTRAINT listing_package_assignments_one_account CHECK ((organisation_id IS NULL) <> (user_id IS NULL)),
    CONSTRAINT listing_package_assignments_dates_ordered CHECK (ends_on IS NULL OR ends_on >= starts_on)
);
CREATE INDEX IF NOT EXISTS listing_package_assignments_org_idx ON listing_package_assignments (organisation_id, market_code, starts_on)
    WHERE organisation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS listing_package_assignments_user_idx ON listing_package_assignments (user_id, market_code, starts_on)
    WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS listing_package_assignments_package_idx ON listing_package_assignments (package_id);

ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_check;
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW', 'LISTING_PACKAGE', 'ORGANISATION'));
