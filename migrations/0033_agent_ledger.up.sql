-- 0033: agent ledger / books (API 0.34.0, BR-012, FR-026 to FR-029, FSD
-- section 6). A cashbook, not double-entry (decision D-05).
--
--   * ledger_categories: an organisation's (organisation_id) or an approved
--     direct owner's (book_user_id) own categories; exactly one is set. The
--     defaults are created by the API on first use of a book. Never deleted,
--     only deactivated.
--   * ledger_entries: posted entries. amount numeric(20,4) is always
--     positive; the sign is direction (IN / OUT). One book holds many
--     currencies; nothing ever sums across them. A posted entry is never
--     edited: a REVERSAL entry (reversal_of) and, for a correction, a new
--     entry (correction_of) are posted instead, and the original gets
--     reversed_by / corrected_by once. A TRANSFER is an OUT / IN pair whose
--     transfer_pair point at each other. A RECEIVABLE / PAYABLE is a memo
--     (no method; never in a cashbook balance) that cash entries may settle
--     (settles_entry_id).
--   * Guards: the trigger ledger_entries_guard refuses DELETE (and
--     TRUNCATE), and any UPDATE of the book, type, direction, amount,
--     currency, method, effective date, author, recorded_at, reversal_of,
--     correction_of or settles_entry_id; reversed_by, corrected_by and
--     transfer_pair may be set once (NULL -> value) and never changed. Only
--     the reconciliation fields and the links to contacts, listings and
--     leases (ON DELETE SET NULL) may change. A CHECK refuses
--     GATEWAY_CONFIRMED without a gateway_reference (the API never sets it:
--     there is no gateway). A partial unique index allows one live
--     (unreversed) opening balance per book, currency, method and branch.
--   * ledger_entry_notes (append-only) and ledger_attachments (private
--     object storage keys; downloads go through the API and are audited).
--   * organisation_audit_events gains the LEDGER_* actions and the target
--     types LEDGER_ENTRY and LEDGER_BOOK.
--
-- Safe on a database with data: new tables and wider checks. Books and
-- authors reference organisations and users ON DELETE RESTRICT: financial
-- records are never removed by a cascade.

CREATE TABLE IF NOT EXISTS ledger_categories (
    id              uuid PRIMARY KEY,
    organisation_id uuid REFERENCES organisations (id) ON DELETE RESTRICT,
    book_user_id    uuid REFERENCES users (id) ON DELETE RESTRICT,
    book_id         uuid GENERATED ALWAYS AS (COALESCE(organisation_id, book_user_id)) STORED,
    code            text NOT NULL CHECK (code ~ '^[a-z0-9][a-z0-9-]{0,39}$'),
    label           text NOT NULL CHECK (char_length(label) BETWEEN 1 AND 80),
    kind            text NOT NULL CHECK (kind IN ('IN', 'OUT', 'ANY')),
    active          boolean NOT NULL DEFAULT true,
    is_default      boolean NOT NULL DEFAULT false,
    sort_order      integer NOT NULL DEFAULT 0,
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ledger_categories_book_check CHECK ((organisation_id IS NULL) <> (book_user_id IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS ledger_categories_book_code_idx ON ledger_categories (book_id, code);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id                 uuid PRIMARY KEY,
    organisation_id    uuid REFERENCES organisations (id) ON DELETE RESTRICT,
    book_user_id       uuid REFERENCES users (id) ON DELETE RESTRICT,
    book_id            uuid GENERATED ALWAYS AS (COALESCE(organisation_id, book_user_id)) STORED,
    type               text NOT NULL CHECK (type IN ('INCOME', 'EXPENSE', 'RENT_RECEIVED', 'COMMISSION', 'DEPOSIT_HELD',
                           'DEPOSIT_RETURNED', 'TRANSFER', 'OPENING_BALANCE', 'RECEIVABLE', 'PAYABLE', 'REVERSAL')),
    direction          text NOT NULL CHECK (direction IN ('IN', 'OUT')),
    category           text CHECK (category IS NULL OR category ~ '^[a-z0-9][a-z0-9-]{0,39}$'),
    method             text CHECK (method IS NULL OR method IN ('CASH', 'BANK', 'MOBILE_MONEY', 'OTHER')),
    amount             numeric(20, 4) NOT NULL CHECK (amount > 0),
    currency           char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    effective_date     date NOT NULL,
    recorded_at        timestamptz NOT NULL DEFAULT now(),
    author_user_id     uuid REFERENCES users (id) ON DELETE RESTRICT,
    author_name        text NOT NULL DEFAULT '',
    status             text NOT NULL DEFAULT 'USER_RECORDED_UNVERIFIED'
                           CHECK (status IN ('USER_RECORDED_UNVERIFIED', 'RECONCILED', 'GATEWAY_CONFIRMED')),
    gateway_reference  text,
    reconciled_at      timestamptz,
    reconciled_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    reconciled_by_name text NOT NULL DEFAULT '',
    reconcile_note     text CHECK (reconcile_note IS NULL OR char_length(reconcile_note) <= 500),
    reversal_of        uuid REFERENCES ledger_entries (id),
    reversed_by        uuid REFERENCES ledger_entries (id),
    correction_of      uuid REFERENCES ledger_entries (id),
    corrected_by       uuid REFERENCES ledger_entries (id),
    transfer_pair      uuid REFERENCES ledger_entries (id),
    settles_entry_id   uuid REFERENCES ledger_entries (id),
    reason             text CHECK (reason IS NULL OR char_length(reason) <= 500),
    contact_id         uuid REFERENCES contacts (id) ON DELETE SET NULL,
    listing_id         uuid REFERENCES listings (id) ON DELETE SET NULL,
    lease_id           uuid REFERENCES leases (id) ON DELETE SET NULL,
    -- Tenancies do not exist yet (feature 10): always NULL today.
    tenancy_id         uuid,
    branch             text CHECK (branch IS NULL OR char_length(branch) BETWEEN 1 AND 80),
    description        text NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    reference          text NOT NULL DEFAULT '' CHECK (char_length(reference) <= 120),
    CONSTRAINT ledger_entries_book_check CHECK ((organisation_id IS NULL) <> (book_user_id IS NULL)),
    -- FR-028: nothing claims gateway confirmation without a gateway reference.
    CONSTRAINT ledger_entries_gateway_check CHECK (status <> 'GATEWAY_CONFIRMED' OR gateway_reference IS NOT NULL),
    CONSTRAINT ledger_entries_reconciled_check CHECK ((status = 'RECONCILED') = (reconciled_at IS NOT NULL)),
    CONSTRAINT ledger_entries_reversal_check CHECK ((type = 'REVERSAL') = (reversal_of IS NOT NULL)),
    -- Named apart from the column's own CHECK, which Postgres calls
    -- ledger_entries_method_check.
    CONSTRAINT ledger_entries_method_required_check CHECK (method IS NOT NULL OR type IN ('RECEIVABLE', 'PAYABLE', 'REVERSAL')),
    CONSTRAINT ledger_entries_memo_method_check CHECK (method IS NULL OR type NOT IN ('RECEIVABLE', 'PAYABLE')),
    CONSTRAINT ledger_entries_tenancy_check CHECK (tenancy_id IS NULL),
    CONSTRAINT ledger_entries_self_links_check CHECK (reversal_of IS DISTINCT FROM id AND reversed_by IS DISTINCT FROM id
        AND correction_of IS DISTINCT FROM id AND corrected_by IS DISTINCT FROM id AND transfer_pair IS DISTINCT FROM id
        AND settles_entry_id IS DISTINCT FROM id)
);
CREATE INDEX IF NOT EXISTS ledger_entries_book_idx ON ledger_entries (book_id, effective_date DESC, recorded_at DESC);
CREATE INDEX IF NOT EXISTS ledger_entries_book_currency_idx ON ledger_entries (book_id, currency, effective_date);
CREATE INDEX IF NOT EXISTS ledger_entries_contact_idx ON ledger_entries (contact_id) WHERE contact_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS ledger_entries_settles_idx ON ledger_entries (settles_entry_id) WHERE settles_entry_id IS NOT NULL;
-- One live opening balance per book, currency, method and branch.
CREATE UNIQUE INDEX IF NOT EXISTS ledger_entries_opening_idx
    ON ledger_entries (book_id, currency, method, (COALESCE(lower(branch), '')))
    WHERE type = 'OPENING_BALANCE' AND reversed_by IS NULL;

CREATE OR REPLACE FUNCTION ledger_entries_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'ledger entries are never deleted: post a reversal instead'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.organisation_id IS DISTINCT FROM OLD.organisation_id
       OR NEW.book_user_id IS DISTINCT FROM OLD.book_user_id
       OR NEW.type IS DISTINCT FROM OLD.type
       OR NEW.direction IS DISTINCT FROM OLD.direction
       OR NEW.amount IS DISTINCT FROM OLD.amount
       OR NEW.currency IS DISTINCT FROM OLD.currency
       OR NEW.method IS DISTINCT FROM OLD.method
       OR NEW.category IS DISTINCT FROM OLD.category
       OR NEW.effective_date IS DISTINCT FROM OLD.effective_date
       OR NEW.recorded_at IS DISTINCT FROM OLD.recorded_at
       OR NEW.author_user_id IS DISTINCT FROM OLD.author_user_id
       OR NEW.author_name IS DISTINCT FROM OLD.author_name
       OR NEW.reversal_of IS DISTINCT FROM OLD.reversal_of
       OR NEW.correction_of IS DISTINCT FROM OLD.correction_of
       OR NEW.settles_entry_id IS DISTINCT FROM OLD.settles_entry_id
       OR NEW.reason IS DISTINCT FROM OLD.reason
       OR NEW.branch IS DISTINCT FROM OLD.branch
       OR NEW.description IS DISTINCT FROM OLD.description
       OR NEW.reference IS DISTINCT FROM OLD.reference
       OR NEW.tenancy_id IS DISTINCT FROM OLD.tenancy_id
       OR NEW.gateway_reference IS DISTINCT FROM OLD.gateway_reference
       OR (OLD.reversed_by IS NOT NULL AND NEW.reversed_by IS DISTINCT FROM OLD.reversed_by)
       OR (OLD.corrected_by IS NOT NULL AND NEW.corrected_by IS DISTINCT FROM OLD.corrected_by)
       OR (OLD.transfer_pair IS NOT NULL AND NEW.transfer_pair IS DISTINCT FROM OLD.transfer_pair) THEN
        RAISE EXCEPTION 'posted ledger entries are immutable: post a reversal or a correcting entry instead'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS ledger_entries_immutable ON ledger_entries;
CREATE TRIGGER ledger_entries_immutable BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_guard();
DROP TRIGGER IF EXISTS ledger_entries_no_truncate ON ledger_entries;
CREATE TRIGGER ledger_entries_no_truncate BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_guard();

CREATE TABLE IF NOT EXISTS ledger_entry_notes (
    id             uuid PRIMARY KEY,
    entry_id       uuid NOT NULL REFERENCES ledger_entries (id),
    text           text NOT NULL CHECK (char_length(text) BETWEEN 1 AND 5000),
    author_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    author_name    text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ledger_entry_notes_entry_idx ON ledger_entry_notes (entry_id, created_at);

CREATE TABLE IF NOT EXISTS ledger_attachments (
    id               uuid PRIMARY KEY,
    entry_id         uuid NOT NULL REFERENCES ledger_entries (id),
    file_name        text NOT NULL CHECK (char_length(file_name) BETWEEN 1 AND 200),
    content_type     text NOT NULL CHECK (content_type IN ('application/pdf', 'image/jpeg', 'image/png', 'image/webp')),
    size_bytes       bigint NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 10485760),
    sha256           text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    object_key       text NOT NULL UNIQUE,
    uploaded_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    uploaded_by_name text NOT NULL DEFAULT '',
    uploaded_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ledger_attachments_entry_idx ON ledger_attachments (entry_id, uploaded_at);

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
    'LEDGER_ENTRY_UNRECONCILED', 'LEDGER_ATTACHMENT_ADDED', 'LEDGER_ATTACHMENT_VIEWED', 'LEDGER_EXPORTED'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION',
                           'VENUE_BOOKING', 'LEDGER_ENTRY', 'LEDGER_BOOK'));
