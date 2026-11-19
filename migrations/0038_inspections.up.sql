-- 0038: inspections (API 0.36.3, BR-019, FR-047, Phase 2 part d).
--
--   * inspections: a property inspection in a book (organisation_id XOR
--     book_user_id, as properties), on a property, optionally one of its
--     units (composite key: the unit is of that property) and a tenancy
--     (ON DELETE SET NULL). Type MOVE_IN / ROUTINE / MOVE_OUT; status
--     SCHEDULED -> IN_PROGRESS -> COMPLETED, or CANCELLED. The checklist
--     (areas, items and their findings) is the inspection's own copy of a
--     template, as JSON: later edits of the template never change it.
--     `version` counts the writes that replaced the row (optimistic
--     locking). The reference RPI-<year>-<6 digits> comes from
--     inspection_reference_seq (the seed uses 9000xx).
--   * A COMPLETED or CANCELLED inspection is frozen: inspections_freeze
--     refuses any change but unlinking people (account deletion) and the
--     tenancy link being cleared by its foreign key.
--   * inspection_photos: images of a checklist item (item_id is an id
--     inside the checklist JSON) in the private object storage
--     (re-encoded JPEG, no metadata), served only through the API.
--   * reference_items gains the set inspection_checklist_templates (a tree
--     by parent_code: template, area, item) with two default templates.
--   * organisation_audit_events gains INSPECTION_COMPLETED,
--     INSPECTION_CANCELLED and the target INSPECTION.
--
-- Constraint names: inline CHECKs get Postgres's <table>_<column>_check;
-- the table-level constraints below are named differently on purpose.
--
-- User references are ON DELETE SET NULL; users are anonymised on account
-- deletion, never deleted, so the accounts module unlinks them explicitly.
--
-- Safe on a database with data: new tables, a new function and trigger,
-- new reference rows and wider checks.

CREATE SEQUENCE IF NOT EXISTS inspection_reference_seq;

CREATE TABLE IF NOT EXISTS inspections (
    id                uuid PRIMARY KEY,
    reference         text NOT NULL CHECK (reference ~ '^RPI-[0-9]{4}-[0-9]{6,}$'),
    organisation_id   uuid REFERENCES organisations (id) ON DELETE RESTRICT,
    book_user_id      uuid REFERENCES users (id) ON DELETE RESTRICT,
    property_id       uuid NOT NULL REFERENCES properties (id) ON DELETE RESTRICT,
    unit_id           uuid,
    tenancy_id        uuid REFERENCES tenancies (id) ON DELETE SET NULL,
    type              text NOT NULL CHECK (type IN ('MOVE_IN', 'ROUTINE', 'MOVE_OUT')),
    status            text NOT NULL CHECK (status IN ('SCHEDULED', 'IN_PROGRESS', 'COMPLETED', 'CANCELLED')),
    scheduled_for     timestamptz NOT NULL,
    inspector_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    inspector_name    text NOT NULL DEFAULT '' CHECK (char_length(inspector_name) <= 200),
    template_code     text NOT NULL CHECK (char_length(template_code) BETWEEN 1 AND 80),
    template_label    text NOT NULL CHECK (char_length(template_label) BETWEEN 1 AND 200),
    checklist         jsonb NOT NULL CHECK (jsonb_typeof(checklist) = 'object'),
    summary           text NOT NULL DEFAULT '' CHECK (char_length(summary) <= 5000),
    tenant_sign_off   text NOT NULL DEFAULT '' CHECK (char_length(tenant_sign_off) <= 200),
    agent_sign_off    text NOT NULL DEFAULT '' CHECK (char_length(agent_sign_off) <= 200),
    cancel_reason     text NOT NULL DEFAULT '' CHECK (char_length(cancel_reason) <= 500),
    version           integer NOT NULL DEFAULT 0 CHECK (version >= 0),
    started_at        timestamptz,
    completed_at      timestamptz,
    completed_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    completed_by_name text NOT NULL DEFAULT '',
    cancelled_at      timestamptz,
    cancelled_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    cancelled_by_name text NOT NULL DEFAULT '',
    created_by        uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by_name   text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    CONSTRAINT inspections_reference_key UNIQUE (reference),
    CONSTRAINT inspections_one_book CHECK ((organisation_id IS NULL) <> (book_user_id IS NULL)),
    CONSTRAINT inspections_unit_of_property_fkey FOREIGN KEY (unit_id, property_id)
        REFERENCES property_units (id, property_id) ON DELETE RESTRICT,
    CONSTRAINT inspections_completion_state CHECK ((status = 'COMPLETED') = (completed_at IS NOT NULL)),
    CONSTRAINT inspections_cancellation_state CHECK ((status = 'CANCELLED') = (cancelled_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS inspections_org_idx ON inspections (organisation_id, scheduled_for DESC)
    WHERE organisation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS inspections_owner_idx ON inspections (book_user_id, scheduled_for DESC)
    WHERE book_user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS inspections_tenancy_idx ON inspections (tenancy_id) WHERE tenancy_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS inspections_property_idx ON inspections (property_id);

-- A completed or cancelled inspection never changes, except that people
-- are unlinked (account deletion) and the tenancy link may be cleared by
-- its foreign key.
CREATE OR REPLACE FUNCTION inspections_freeze() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('COMPLETED', 'CANCELLED') AND (
           NEW.reference IS DISTINCT FROM OLD.reference
        OR NEW.organisation_id IS DISTINCT FROM OLD.organisation_id
        OR NEW.book_user_id IS DISTINCT FROM OLD.book_user_id
        OR NEW.property_id IS DISTINCT FROM OLD.property_id
        OR NEW.unit_id IS DISTINCT FROM OLD.unit_id
        OR NEW.type IS DISTINCT FROM OLD.type
        OR NEW.status IS DISTINCT FROM OLD.status
        OR NEW.scheduled_for IS DISTINCT FROM OLD.scheduled_for
        OR NEW.inspector_name IS DISTINCT FROM OLD.inspector_name
        OR NEW.template_code IS DISTINCT FROM OLD.template_code
        OR NEW.template_label IS DISTINCT FROM OLD.template_label
        OR NEW.checklist IS DISTINCT FROM OLD.checklist
        OR NEW.summary IS DISTINCT FROM OLD.summary
        OR NEW.tenant_sign_off IS DISTINCT FROM OLD.tenant_sign_off
        OR NEW.agent_sign_off IS DISTINCT FROM OLD.agent_sign_off
        OR NEW.cancel_reason IS DISTINCT FROM OLD.cancel_reason
        OR NEW.version IS DISTINCT FROM OLD.version
        OR NEW.started_at IS DISTINCT FROM OLD.started_at
        OR NEW.completed_at IS DISTINCT FROM OLD.completed_at
        OR NEW.cancelled_at IS DISTINCT FROM OLD.cancelled_at
        OR (NEW.tenancy_id IS NOT NULL AND NEW.tenancy_id IS DISTINCT FROM OLD.tenancy_id)
        OR (NEW.inspector_user_id IS NOT NULL AND NEW.inspector_user_id IS DISTINCT FROM OLD.inspector_user_id)
        OR (NEW.completed_by IS NOT NULL AND NEW.completed_by IS DISTINCT FROM OLD.completed_by)
        OR (NEW.cancelled_by IS NOT NULL AND NEW.cancelled_by IS DISTINCT FROM OLD.cancelled_by)
        OR (NEW.created_by IS NOT NULL AND NEW.created_by IS DISTINCT FROM OLD.created_by)) THEN
        RAISE EXCEPTION 'a completed or cancelled inspection is frozen'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS inspections_freeze ON inspections;
CREATE TRIGGER inspections_freeze BEFORE UPDATE ON inspections FOR EACH ROW EXECUTE FUNCTION inspections_freeze();

CREATE TABLE IF NOT EXISTS inspection_photos (
    id               uuid PRIMARY KEY,
    inspection_id    uuid NOT NULL REFERENCES inspections (id) ON DELETE CASCADE,
    item_id          uuid NOT NULL,
    object_key       text NOT NULL,
    content_type     text NOT NULL CHECK (content_type = 'image/jpeg'),
    width            integer NOT NULL CHECK (width > 0),
    height           integer NOT NULL CHECK (height > 0),
    size_bytes       bigint NOT NULL CHECK (size_bytes > 0),
    sha256           text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    uploaded_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    uploaded_by_name text NOT NULL DEFAULT '',
    uploaded_at      timestamptz NOT NULL,
    CONSTRAINT inspection_photos_object_key_key UNIQUE (object_key)
);
CREATE INDEX IF NOT EXISTS inspection_photos_inspection_idx ON inspection_photos (inspection_id, uploaded_at);
CREATE INDEX IF NOT EXISTS inspection_photos_item_idx ON inspection_photos (inspection_id, item_id);

-- Reference data: the inspection checklist templates (admin-editable).
ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'regions', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds', 'price_periods', 'contact_types', 'contact_sources',
        'maintenance_categories', 'maintenance_priorities', 'inspection_checklist_templates'));

INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential', 'Standard residential', NULL, 10, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-entrance', 'Entrance and hallway', NULL, 10, NULL, 'residential') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-entrance-door', 'Front door and locks', NULL, 10, NULL, 'residential-entrance') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-entrance-walls', 'Walls and ceiling', NULL, 20, NULL, 'residential-entrance') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-entrance-floor', 'Floor', NULL, 30, NULL, 'residential-entrance') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-entrance-lights', 'Lights and switches', NULL, 40, NULL, 'residential-entrance') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-living', 'Living room', NULL, 20, NULL, 'residential') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-living-walls', 'Walls and ceiling', NULL, 10, NULL, 'residential-living') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-living-floor', 'Floor and skirting', NULL, 20, NULL, 'residential-living') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-living-windows', 'Windows and curtains', NULL, 30, NULL, 'residential-living') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-living-lights', 'Lights, switches and sockets', NULL, 40, NULL, 'residential-living') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-kitchen', 'Kitchen', NULL, 30, NULL, 'residential') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-kitchen-sink', 'Sink and taps', NULL, 10, NULL, 'residential-kitchen') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-kitchen-cupboards', 'Cupboards and drawers', NULL, 20, NULL, 'residential-kitchen') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-kitchen-worktops', 'Worktops', NULL, 30, NULL, 'residential-kitchen') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-kitchen-stove', 'Stove and oven', NULL, 40, NULL, 'residential-kitchen') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-kitchen-walls', 'Walls and floor', NULL, 50, NULL, 'residential-kitchen') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bathroom', 'Bathroom', NULL, 40, NULL, 'residential') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bathroom-bath', 'Bath or shower', NULL, 10, NULL, 'residential-bathroom') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bathroom-toilet', 'Toilet', NULL, 20, NULL, 'residential-bathroom') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bathroom-basin', 'Basin and taps', NULL, 30, NULL, 'residential-bathroom') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bathroom-tiles', 'Tiles and grout', NULL, 40, NULL, 'residential-bathroom') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bathroom-fan', 'Extractor fan', NULL, 50, NULL, 'residential-bathroom') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bedroom1', 'Main bedroom', NULL, 50, NULL, 'residential') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bedroom1-walls', 'Walls and ceiling', NULL, 10, NULL, 'residential-bedroom1') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bedroom1-floor', 'Floor', NULL, 20, NULL, 'residential-bedroom1') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bedroom1-windows', 'Windows', NULL, 30, NULL, 'residential-bedroom1') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bedroom1-cupboards', 'Built-in cupboards', NULL, 40, NULL, 'residential-bedroom1') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bedroom2', 'Second bedroom', NULL, 60, NULL, 'residential') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bedroom2-walls', 'Walls and ceiling', NULL, 10, NULL, 'residential-bedroom2') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bedroom2-floor', 'Floor', NULL, 20, NULL, 'residential-bedroom2') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-bedroom2-windows', 'Windows', NULL, 30, NULL, 'residential-bedroom2') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-exterior', 'Exterior and garden', NULL, 70, NULL, 'residential') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-exterior-garden', 'Garden and lawn', NULL, 10, NULL, 'residential-exterior') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-exterior-gate', 'Gate and fencing', NULL, 20, NULL, 'residential-exterior') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-exterior-gutters', 'Gutters and drains', NULL, 30, NULL, 'residential-exterior') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-meters', 'Meters, keys and safety', NULL, 80, NULL, 'residential') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-meters-smoke-alarm', 'Smoke alarm', NULL, 10, NULL, 'residential-meters') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-meters-electricity', 'Electricity meter reading', NULL, 20, NULL, 'residential-meters') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-meters-water', 'Water meter reading', NULL, 30, NULL, 'residential-meters') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'residential-meters-keys', 'Keys handed over', NULL, 40, NULL, 'residential-meters') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial', 'Short commercial', NULL, 20, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-reception', 'Reception and entrance', NULL, 10, NULL, 'commercial') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-reception-doors', 'Doors and locks', NULL, 10, NULL, 'commercial-reception') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-reception-walls', 'Walls and ceiling', NULL, 20, NULL, 'commercial-reception') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-reception-floor', 'Floor', NULL, 30, NULL, 'commercial-reception') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-office', 'Office space', NULL, 20, NULL, 'commercial') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-office-walls', 'Walls and ceiling', NULL, 10, NULL, 'commercial-office') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-office-floor', 'Floor and carpets', NULL, 20, NULL, 'commercial-office') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-office-lights', 'Lights', NULL, 30, NULL, 'commercial-office') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-office-aircon', 'Air conditioning', NULL, 40, NULL, 'commercial-office') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-kitchenette', 'Kitchenette and toilets', NULL, 30, NULL, 'commercial') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-kitchenette-sink', 'Sink and taps', NULL, 10, NULL, 'commercial-kitchenette') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-kitchenette-toilets', 'Toilets', NULL, 20, NULL, 'commercial-kitchenette') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-kitchenette-basins', 'Hand basins', NULL, 30, NULL, 'commercial-kitchenette') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-safety', 'Safety and services', NULL, 40, NULL, 'commercial') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-safety-extinguishers', 'Fire extinguishers', NULL, 10, NULL, 'commercial-safety') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-safety-exits', 'Emergency exits', NULL, 20, NULL, 'commercial-safety') ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('inspection_checklist_templates', 'commercial-safety-electricity', 'Electricity meter reading', NULL, 30, NULL, 'commercial-safety') ON CONFLICT (set_code, code) DO NOTHING;

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
    'MAINTENANCE_ASSIGNED', 'MAINTENANCE_COST_RECORDED', 'MAINTENANCE_CANCELLED',
    'INSPECTION_COMPLETED', 'INSPECTION_CANCELLED'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION',
                           'VENUE_BOOKING', 'LEDGER_ENTRY', 'LEDGER_BOOK', 'TENANCY', 'MAINTENANCE_REQUEST', 'INSPECTION'));
