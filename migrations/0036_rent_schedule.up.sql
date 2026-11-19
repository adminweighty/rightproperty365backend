-- 0036: rent schedule, receipts and arrears (API 0.36.1, BR-019, FR-047,
-- Phase 2 part b).
--
--   * rent_dues: one line of a tenancy's rent schedule (due date, amount
--     in the rent currency), generated from the tenancy's rent rule when it
--     is activated and extended by the worker (rolling schedules). One due
--     per tenancy and due date, so generation is idempotent. A due is
--     waived once (waived_at, by, reason); what is paid is never stored on
--     it: it is the sum of the allocations of the receipts that are not
--     reversed. (id, tenancy_id) is unique so an allocation's due is
--     provably of the receipt's tenancy.
--   * rent_receipts: money received for a tenancy, in its rent currency.
--     Each one has exactly one ledger entry (RENT_RECEIVED, ledger_entry_id
--     unique), written in the same transaction. A receipt is never deleted:
--     a ledger reversal of its entry sets reversed_at / reversal_entry_id /
--     reversed_on in the reversal's transaction, after which its
--     allocations no longer count.
--   * rent_allocations: how a receipt's amount is spread over the dues of
--     the same tenancy (composite foreign keys). The API never allocates
--     more than a due's outstanding amount (checked under row locks).
--   * ledger_entries: the CHECK that kept tenancy_id NULL is replaced by
--     one that allows it on RENT_RECEIVED entries and their REVERSALs.
--   * organisation_audit_events gains RENT_RECEIPT_RECORDED and
--     RENT_DUE_WAIVED (target TENANCY).
--
-- Constraint names: inline CHECKs get Postgres's <table>_<column>_check;
-- the table-level constraints below are named differently on purpose.
--
-- User references (waived_by, recorded_by) are ON DELETE SET NULL; users
-- are anonymised on account deletion, never deleted, so the accounts
-- module unlinks them explicitly. The receipt's contact key is DEFERRABLE
-- INITIALLY DEFERRED (as tenancy_parties) for the development re-seed.
--
-- Safe on a database with data: new tables, a wider CHECK and wider audit
-- checks.

CREATE TABLE IF NOT EXISTS rent_dues (
    id             uuid PRIMARY KEY,
    tenancy_id     uuid NOT NULL REFERENCES tenancies (id) ON DELETE RESTRICT,
    sequence       integer NOT NULL CHECK (sequence >= 1),
    due_date       date NOT NULL,
    amount         numeric(20, 4) NOT NULL CHECK (amount > 0),
    currency       char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    waived_at      timestamptz,
    waived_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    waived_by_name text NOT NULL DEFAULT '',
    waive_reason   text CHECK (waive_reason IS NULL OR char_length(waive_reason) BETWEEN 1 AND 500),
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT rent_dues_tenancy_date_key UNIQUE (tenancy_id, due_date),
    CONSTRAINT rent_dues_id_tenancy_key UNIQUE (id, tenancy_id),
    CONSTRAINT rent_dues_waiver_pair_check CHECK ((waived_at IS NULL) = (waive_reason IS NULL))
);

CREATE TABLE IF NOT EXISTS rent_receipts (
    id                uuid PRIMARY KEY,
    tenancy_id        uuid NOT NULL REFERENCES tenancies (id) ON DELETE RESTRICT,
    amount            numeric(20, 4) NOT NULL CHECK (amount > 0),
    currency          char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    method            text NOT NULL CHECK (method IN ('CASH', 'BANK', 'MOBILE_MONEY', 'OTHER')),
    received_on       date NOT NULL,
    reference         text NOT NULL DEFAULT '' CHECK (char_length(reference) <= 120),
    contact_id        uuid REFERENCES contacts (id) DEFERRABLE INITIALLY DEFERRED,
    contact_name      text NOT NULL DEFAULT '' CHECK (char_length(contact_name) <= 200),
    ledger_entry_id   uuid NOT NULL REFERENCES ledger_entries (id),
    recorded_by       uuid REFERENCES users (id) ON DELETE SET NULL,
    recorded_by_name  text NOT NULL DEFAULT '',
    recorded_at       timestamptz NOT NULL DEFAULT now(),
    reversed_at       timestamptz,
    reversal_entry_id uuid REFERENCES ledger_entries (id),
    reversed_on       date,
    CONSTRAINT rent_receipts_ledger_entry_key UNIQUE (ledger_entry_id),
    CONSTRAINT rent_receipts_id_tenancy_key UNIQUE (id, tenancy_id),
    CONSTRAINT rent_receipts_reversal_state_check CHECK ((reversed_at IS NULL) = (reversal_entry_id IS NULL)
        AND (reversed_at IS NULL) = (reversed_on IS NULL))
);
CREATE INDEX IF NOT EXISTS rent_receipts_tenancy_idx ON rent_receipts (tenancy_id, received_on DESC);

CREATE TABLE IF NOT EXISTS rent_allocations (
    receipt_id uuid NOT NULL,
    tenancy_id uuid NOT NULL,
    due_id     uuid NOT NULL,
    amount     numeric(20, 4) NOT NULL CHECK (amount > 0),
    PRIMARY KEY (receipt_id, due_id),
    CONSTRAINT rent_allocations_receipt_ref_fkey FOREIGN KEY (receipt_id, tenancy_id) REFERENCES rent_receipts (id, tenancy_id),
    CONSTRAINT rent_allocations_due_ref_fkey FOREIGN KEY (due_id, tenancy_id) REFERENCES rent_dues (id, tenancy_id)
);
CREATE INDEX IF NOT EXISTS rent_allocations_due_idx ON rent_allocations (due_id);

-- The ledger's tenancy link: allowed on rent received and its reversals.
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_tenancy_check;
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_tenancy_type_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_tenancy_type_check
    CHECK (tenancy_id IS NULL OR type IN ('RENT_RECEIVED', 'REVERSAL'));
CREATE INDEX IF NOT EXISTS ledger_entries_tenancy_idx ON ledger_entries (tenancy_id) WHERE tenancy_id IS NOT NULL;

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
    'TENANCY_CREATED', 'TENANCY_ACTIVATED', 'TENANCY_ENDED', 'TENANCY_DELETED',
    'RENT_RECEIPT_RECORDED', 'RENT_DUE_WAIVED'));
