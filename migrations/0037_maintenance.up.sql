-- 0037: maintenance requests (API 0.36.2, BR-019, FR-047, Phase 2 part c).
--
--   * maintenance_requests: a repair or job in a book (organisation_id XOR
--     book_user_id, as properties), on a property, optionally one of its
--     units (composite key: the unit is of that property) and a tenancy.
--     The category and priority are codes of the reference sets
--     maintenance_categories / maintenance_priorities, with the label they
--     had when set. Status NEW -> TRIAGED -> ASSIGNED -> IN_PROGRESS ->
--     RESOLVED -> CLOSED, or CANCELLED. Raised by a MANAGER or a TENANT.
--     The current assignment (a directory profile and the business enquiry
--     sent to it) is kept on the row; earlier ones stay on the timeline.
--     The reference RPM-<year>-<6 digits> comes from
--     maintenance_reference_seq (the seed uses 9000xx).
--   * maintenance_events: the timeline (created, status changes, comments,
--     assignments, costs, photos, edits).
--   * maintenance_photos: images in the private object storage (re-encoded
--     JPEG, no metadata), served only through the API.
--   * maintenance_costs: one EXPENSE ledger entry each (ledger_entry_id
--     unique), written in the same transaction.
--   * ledger_entries gains property_id (a maintenance cost's property;
--     EXPENSE entries and their REVERSALs only), protected by the
--     immutability guard like every other posted column.
--   * reference_items gains the sets maintenance_categories and
--     maintenance_priorities, with their seed rows.
--   * organisation_audit_events gains MAINTENANCE_ASSIGNED,
--     MAINTENANCE_COST_RECORDED, MAINTENANCE_CANCELLED and the target
--     MAINTENANCE_REQUEST.
--
-- Constraint names: inline CHECKs get Postgres's <table>_<column>_check;
-- the table-level constraints below are named differently on purpose.
--
-- User references are ON DELETE SET NULL; users are anonymised on account
-- deletion, never deleted, so the accounts module unlinks them explicitly
-- (and replaces a tenant's name).
--
-- Safe on a database with data: new tables, a nullable column, a replaced
-- guard function and wider checks.

CREATE SEQUENCE IF NOT EXISTS maintenance_reference_seq;

CREATE TABLE IF NOT EXISTS maintenance_requests (
    id                  uuid PRIMARY KEY,
    reference           text NOT NULL CHECK (reference ~ '^RPM-[0-9]{4}-[0-9]{6,}$'),
    organisation_id     uuid REFERENCES organisations (id) ON DELETE RESTRICT,
    book_user_id        uuid REFERENCES users (id) ON DELETE RESTRICT,
    property_id         uuid NOT NULL REFERENCES properties (id) ON DELETE RESTRICT,
    unit_id             uuid,
    tenancy_id          uuid REFERENCES tenancies (id) ON DELETE SET NULL,
    category            text NOT NULL CHECK (category ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    category_label      text NOT NULL CHECK (char_length(category_label) BETWEEN 1 AND 120),
    priority            text NOT NULL CHECK (priority ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    priority_label      text NOT NULL CHECK (char_length(priority_label) BETWEEN 1 AND 120),
    title               text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    description         text NOT NULL DEFAULT '' CHECK (char_length(description) <= 5000),
    status              text NOT NULL CHECK (status IN ('NEW', 'TRIAGED', 'ASSIGNED', 'IN_PROGRESS', 'RESOLVED', 'CLOSED', 'CANCELLED')),
    raised_by_kind      text NOT NULL CHECK (raised_by_kind IN ('MANAGER', 'TENANT')),
    raised_by           uuid REFERENCES users (id) ON DELETE SET NULL,
    raised_by_name      text NOT NULL DEFAULT '' CHECK (char_length(raised_by_name) <= 200),
    provider_profile_id uuid REFERENCES directory_profiles (id) ON DELETE SET NULL,
    provider_kind       text,
    provider_slug       text,
    provider_name       text,
    enquiry_id          uuid REFERENCES business_enquiries (id) ON DELETE SET NULL,
    enquiry_reference   text,
    assignments         integer NOT NULL DEFAULT 0 CHECK (assignments >= 0),
    assigned_at         timestamptz,
    assigned_by         uuid REFERENCES users (id) ON DELETE SET NULL,
    assigned_by_name    text NOT NULL DEFAULT '',
    status_changed_at   timestamptz NOT NULL,
    resolved_at         timestamptz,
    closed_at           timestamptz,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    CONSTRAINT maintenance_requests_reference_key UNIQUE (reference),
    CONSTRAINT maintenance_requests_one_book CHECK ((organisation_id IS NULL) <> (book_user_id IS NULL)),
    CONSTRAINT maintenance_requests_unit_of_property_fkey FOREIGN KEY (unit_id, property_id)
        REFERENCES property_units (id, property_id) ON DELETE RESTRICT,
    CONSTRAINT maintenance_requests_assignment_state CHECK ((provider_name IS NULL) = (assigned_at IS NULL))
);
CREATE INDEX IF NOT EXISTS maintenance_requests_org_idx ON maintenance_requests (organisation_id, created_at DESC)
    WHERE organisation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS maintenance_requests_owner_idx ON maintenance_requests (book_user_id, created_at DESC)
    WHERE book_user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS maintenance_requests_tenancy_idx ON maintenance_requests (tenancy_id) WHERE tenancy_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS maintenance_requests_property_idx ON maintenance_requests (property_id);

CREATE TABLE IF NOT EXISTS maintenance_events (
    id            uuid PRIMARY KEY,
    request_id    uuid NOT NULL REFERENCES maintenance_requests (id) ON DELETE CASCADE,
    type          text NOT NULL CHECK (type IN ('CREATED', 'STATUS_CHANGED', 'COMMENT', 'ASSIGNED', 'COST_ADDED', 'PHOTO_ADDED', 'UPDATED')),
    actor_kind    text NOT NULL CHECK (actor_kind IN ('MANAGER', 'TENANT', 'SYSTEM')),
    actor_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_name    text NOT NULL DEFAULT '' CHECK (char_length(actor_name) <= 200),
    from_status   text,
    to_status     text,
    body          text CHECK (body IS NULL OR char_length(body) BETWEEN 1 AND 5000),
    internal      boolean NOT NULL DEFAULT false,
    details       jsonb,
    created_at    timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS maintenance_events_request_idx ON maintenance_events (request_id, created_at, id);

CREATE TABLE IF NOT EXISTS maintenance_photos (
    id               uuid PRIMARY KEY,
    request_id       uuid NOT NULL REFERENCES maintenance_requests (id) ON DELETE CASCADE,
    object_key       text NOT NULL,
    content_type     text NOT NULL CHECK (content_type = 'image/jpeg'),
    width            integer NOT NULL CHECK (width > 0),
    height           integer NOT NULL CHECK (height > 0),
    size_bytes       bigint NOT NULL CHECK (size_bytes > 0),
    sha256           text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    uploader_kind    text NOT NULL CHECK (uploader_kind IN ('MANAGER', 'TENANT')),
    uploaded_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    uploaded_by_name text NOT NULL DEFAULT '',
    uploaded_at      timestamptz NOT NULL,
    CONSTRAINT maintenance_photos_object_key_key UNIQUE (object_key)
);
CREATE INDEX IF NOT EXISTS maintenance_photos_request_idx ON maintenance_photos (request_id, uploaded_at);

CREATE TABLE IF NOT EXISTS maintenance_costs (
    id               uuid PRIMARY KEY,
    request_id       uuid NOT NULL REFERENCES maintenance_requests (id) ON DELETE RESTRICT,
    amount           numeric(20, 4) NOT NULL CHECK (amount > 0),
    currency         char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    description      text NOT NULL CHECK (char_length(description) BETWEEN 1 AND 500),
    method           text NOT NULL CHECK (method IN ('CASH', 'BANK', 'MOBILE_MONEY', 'OTHER')),
    incurred_on      date NOT NULL,
    ledger_entry_id  uuid NOT NULL REFERENCES ledger_entries (id),
    recorded_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    recorded_by_name text NOT NULL DEFAULT '',
    recorded_at      timestamptz NOT NULL,
    CONSTRAINT maintenance_costs_ledger_entry_key UNIQUE (ledger_entry_id)
);
CREATE INDEX IF NOT EXISTS maintenance_costs_request_idx ON maintenance_costs (request_id, recorded_at DESC);

-- The ledger's property link: a maintenance cost (EXPENSE) and its reversal.
ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS property_id uuid REFERENCES properties (id);
ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_property_type_rule;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_property_type_rule
    CHECK (property_id IS NULL OR type IN ('EXPENSE', 'REVERSAL'));
CREATE INDEX IF NOT EXISTS ledger_entries_property_idx ON ledger_entries (property_id) WHERE property_id IS NOT NULL;

-- The immutability guard of 0033, with property_id among the posted columns.
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
       OR NEW.property_id IS DISTINCT FROM OLD.property_id
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

-- Reference data: maintenance categories and priorities (admin-editable).
ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'regions', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds', 'price_periods', 'contact_types', 'contact_sources',
        'maintenance_categories', 'maintenance_priorities'));

INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_categories', 'plumbing', 'Plumbing', NULL, 10, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_categories', 'electrical', 'Electrical', NULL, 20, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_categories', 'appliances', 'Appliances', NULL, 30, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_categories', 'structure', 'Walls, roof and structure', NULL, 40, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_categories', 'doors-windows', 'Doors, windows and locks', NULL, 50, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_categories', 'pests', 'Pests', NULL, 60, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_categories', 'garden', 'Garden and grounds', NULL, 70, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_categories', 'other', 'Other', NULL, 80, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_priorities', 'low', 'Low', NULL, 10, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_priorities', 'normal', 'Normal', NULL, 20, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_priorities', 'high', 'High', NULL, 30, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('maintenance_priorities', 'urgent', 'Urgent (safety, no water or power)', NULL, 40, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;

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
    'RENT_RECEIPT_RECORDED', 'RENT_DUE_WAIVED',
    'MAINTENANCE_ASSIGNED', 'MAINTENANCE_COST_RECORDED', 'MAINTENANCE_CANCELLED'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION',
                           'VENUE_BOOKING', 'LEDGER_ENTRY', 'LEDGER_BOOK', 'TENANCY', 'MAINTENANCE_REQUEST'));
