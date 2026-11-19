-- 0035: properties, units and tenancies (API 0.36.0, BR-019, FR-047,
-- Phase 2 part a). The foundation the rent schedule and receipts (b),
-- maintenance (c) and inspections (d) attach to.
--
--   * properties: an organisation's (organisation_id) or an approved direct
--     owner's (book_user_id) property, exactly one set, the same books as
--     the CRM and the ledger. Never deleted by the API: archived
--     (deleted_at), so the tenancies, maintenance requests and inspections
--     that used it keep pointing at it. listing_id is an optional link to
--     one of the book's listings.
--   * property_units: the lettable units of a property (a property with no
--     units is one unit). Archived (deleted_at), never deleted; a live label
--     is unique per property, case-insensitive. (id, property_id) is unique
--     so a tenancy's unit is provably one of its property's.
--   * tenancies: property, optional unit, rent (exact numeric(20,4) with an
--     ISO 4217 code, frequency and due day), optional deposit (the rent's
--     currency), start / agreed end dates (market-local dates), status
--     DRAFT -> ACTIVE -> ENDED | TERMINATED, the actual end (ended_on) and
--     who activated and ended it. One ACTIVE tenancy per unit (or per whole
--     property when it has no unit): a partial unique index. book_name is
--     the manager's name at creation (shown to tenants; anonymised on the
--     direct owner's account deletion). The ledger's tenancy_id
--     (migration 0033) now references tenancies (its CHECK still keeps it
--     NULL until part b).
--   * tenancy_parties: the tenants (1+) and landlords of a tenancy, CRM
--     contacts of the same book (checked by the API). The contact foreign
--     key is DEFERRABLE INITIALLY DEFERRED so a development re-seed may
--     delete and re-insert a contact with the same id in one transaction;
--     the API never hard-deletes contacts.
--   * organisation_audit_events gains TENANCY_CREATED, TENANCY_ACTIVATED,
--     TENANCY_ENDED, TENANCY_DELETED and the target type TENANCY.
--
-- Constraint names: inline CHECKs get Postgres's <table>_<column>_check;
-- the table-level constraints below are named differently on purpose.
--
-- Books, creators and actors reference organisations and users ON DELETE
-- RESTRICT / SET NULL; users are anonymised on account deletion, never
-- deleted, so the accounts module handles user-keyed rows explicitly.
--
-- Safe on a database with data: new tables, a new foreign key on a column
-- that is always NULL, and wider checks.

CREATE TABLE IF NOT EXISTS properties (
    id              uuid PRIMARY KEY,
    organisation_id uuid REFERENCES organisations (id) ON DELETE RESTRICT,
    book_user_id    uuid REFERENCES users (id) ON DELETE RESTRICT,
    book_id         uuid GENERATED ALWAYS AS (COALESCE(organisation_id, book_user_id)) STORED,
    name            text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    address         text NOT NULL CHECK (char_length(address) BETWEEN 1 AND 500),
    market_code     text NOT NULL CHECK (market_code ~ '^[A-Z]{2}$'),
    listing_id      uuid REFERENCES listings (id) ON DELETE SET NULL,
    notes           text NOT NULL DEFAULT '' CHECK (char_length(notes) <= 5000),
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by_name text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    CONSTRAINT properties_one_book_check CHECK ((organisation_id IS NULL) <> (book_user_id IS NULL))
);
CREATE INDEX IF NOT EXISTS properties_book_idx ON properties (book_id, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS properties_listing_idx ON properties (listing_id) WHERE listing_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS property_units (
    id          uuid PRIMARY KEY,
    property_id uuid NOT NULL REFERENCES properties (id) ON DELETE RESTRICT,
    label       text NOT NULL CHECK (char_length(label) BETWEEN 1 AND 80),
    position    integer NOT NULL CHECK (position >= 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz,
    CONSTRAINT property_units_id_property_key UNIQUE (id, property_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS property_units_live_label_idx ON property_units (property_id, lower(label)) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS tenancies (
    id                uuid PRIMARY KEY,
    organisation_id   uuid REFERENCES organisations (id) ON DELETE RESTRICT,
    book_user_id      uuid REFERENCES users (id) ON DELETE RESTRICT,
    book_id           uuid GENERATED ALWAYS AS (COALESCE(organisation_id, book_user_id)) STORED,
    book_name         text NOT NULL DEFAULT '' CHECK (char_length(book_name) <= 200),
    property_id       uuid NOT NULL REFERENCES properties (id) ON DELETE RESTRICT,
    unit_id           uuid,
    lease_id          uuid REFERENCES leases (id) ON DELETE SET NULL,
    rent_amount       numeric(20, 4) NOT NULL CHECK (rent_amount > 0),
    rent_currency     char(3) NOT NULL CHECK (rent_currency ~ '^[A-Z]{3}$'),
    rent_frequency    text NOT NULL CHECK (rent_frequency IN ('WEEKLY', 'MONTHLY', 'QUARTERLY', 'ANNUALLY')),
    rent_due_day      smallint NOT NULL CHECK (rent_due_day BETWEEN 1 AND 31),
    deposit_amount    numeric(20, 4) CHECK (deposit_amount IS NULL OR deposit_amount > 0),
    deposit_currency  char(3) CHECK (deposit_currency IS NULL OR deposit_currency ~ '^[A-Z]{3}$'),
    start_date        date NOT NULL,
    end_date          date,
    ended_on          date,
    status            text NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT', 'ACTIVE', 'ENDED', 'TERMINATED')),
    notes             text NOT NULL DEFAULT '' CHECK (char_length(notes) <= 5000),
    end_reason        text CHECK (end_reason IS NULL OR char_length(end_reason) BETWEEN 1 AND 500),
    activated_at      timestamptz,
    activated_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    activated_by_name text NOT NULL DEFAULT '',
    ended_at          timestamptz,
    ended_by          uuid REFERENCES users (id) ON DELETE SET NULL,
    ended_by_name     text NOT NULL DEFAULT '',
    created_by        uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by_name   text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tenancies_one_book_check CHECK ((organisation_id IS NULL) <> (book_user_id IS NULL)),
    CONSTRAINT tenancies_unit_of_property_fkey FOREIGN KEY (unit_id, property_id) REFERENCES property_units (id, property_id),
    CONSTRAINT tenancies_end_after_start_check CHECK (end_date IS NULL OR end_date >= start_date),
    CONSTRAINT tenancies_ended_after_start_check CHECK (ended_on IS NULL OR ended_on >= start_date),
    CONSTRAINT tenancies_deposit_pair_check CHECK ((deposit_amount IS NULL) = (deposit_currency IS NULL)),
    CONSTRAINT tenancies_deposit_same_currency_check CHECK (deposit_currency IS NULL OR deposit_currency = rent_currency),
    CONSTRAINT tenancies_weekly_due_day_check CHECK (rent_frequency <> 'WEEKLY' OR rent_due_day BETWEEN 1 AND 7),
    CONSTRAINT tenancies_activated_state_check CHECK ((status = 'DRAFT') = (activated_at IS NULL)),
    CONSTRAINT tenancies_ended_state_check CHECK ((status IN ('ENDED', 'TERMINATED')) = (ended_at IS NOT NULL AND ended_on IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS tenancies_book_idx ON tenancies (book_id, created_at DESC);
CREATE INDEX IF NOT EXISTS tenancies_property_idx ON tenancies (property_id, status);
CREATE INDEX IF NOT EXISTS tenancies_lease_idx ON tenancies (lease_id) WHERE lease_id IS NOT NULL;
-- One ACTIVE tenancy per unit, or per whole property when it has no unit.
CREATE UNIQUE INDEX IF NOT EXISTS tenancies_one_active_idx
    ON tenancies (property_id, (COALESCE(unit_id, '00000000-0000-0000-0000-000000000000'::uuid)))
    WHERE status = 'ACTIVE';

CREATE TABLE IF NOT EXISTS tenancy_parties (
    tenancy_id uuid NOT NULL REFERENCES tenancies (id) ON DELETE CASCADE,
    contact_id uuid NOT NULL REFERENCES contacts (id) DEFERRABLE INITIALLY DEFERRED,
    role       text NOT NULL CHECK (role IN ('TENANT', 'LANDLORD')),
    position   integer NOT NULL CHECK (position >= 0),
    -- The name when the party was set, shown if the contact is later merged away.
    name       text NOT NULL DEFAULT '' CHECK (char_length(name) <= 200),
    PRIMARY KEY (tenancy_id, contact_id)
);
CREATE INDEX IF NOT EXISTS tenancy_parties_contact_idx ON tenancy_parties (contact_id, role);

-- The ledger's tenancy link (always NULL until part b lifts its CHECK).
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_tenancy_ref_fkey;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_tenancy_ref_fkey FOREIGN KEY (tenancy_id) REFERENCES tenancies (id);

ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_action_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_action_check CHECK (action IN (
    'INVITATION_CREATED', 'INVITATION_CANCELLED', 'INVITATION_ACCEPTED',
    'JOIN_REQUEST_APPROVED', 'JOIN_REQUEST_DECLINED', 'JOIN_REQUEST_CHANGES_REQUESTED',
    'MEMBER_ROLE_CHANGED', 'MEMBER_JOB_TITLE_CHANGED', 'MEMBER_SUSPENDED',
    'MEMBER_REACTIVATED', 'MEMBER_REMOVED', 'REQUEST_ASSIGNED', 'REQUEST_UNASSIGNED',
    'CONTACT_OWNER_CHANGED', 'CONTACTS_MERGED', 'CONTACTS_IMPORTED', 'CONTACTS_EXPORTED',
    'APPLICATION_VIEWED', 'APPLICATION_FILE_VIEWED',
    'VENUE_BOOKING_CONFIRMED', 'VENUE_BOOKING_DECLINED', 'VENUE_BOOKING_CANCELLED',
    'VENUE_BOOKING_TIME_PROPOSED', 'VENUE_BOOKING_COMPLETED', 'VENUE_BOOKING_NO_SHOW',
    'LEDGER_ENTRY_CREATED', 'LEDGER_ENTRY_REVERSED', 'LEDGER_ENTRY_CORRECTED', 'LEDGER_ENTRY_RECONCILED',
    'LEDGER_ENTRY_UNRECONCILED', 'LEDGER_ATTACHMENT_ADDED', 'LEDGER_ATTACHMENT_VIEWED', 'LEDGER_EXPORTED',
    'TENANCY_CREATED', 'TENANCY_ACTIVATED', 'TENANCY_ENDED', 'TENANCY_DELETED'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION',
                           'VENUE_BOOKING', 'LEDGER_ENTRY', 'LEDGER_BOOK', 'TENANCY'));
