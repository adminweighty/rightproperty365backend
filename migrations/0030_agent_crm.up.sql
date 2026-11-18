-- 0030: the agent CRM (API 0.31.0, BR-004, FR-014).
--
--   * contacts: one contact of a book. A book is an organisation's
--     (organisation_id) or an approved direct owner's personal one
--     (book_user_id); exactly one is set. owner_user_id is the relationship
--     owner (a field, not a visibility rule). stage is the lead stage
--     (NEW > CONTACTED > VIEWING_REQUESTED > VIEWING_CONFIRMED > NEGOTIATING >
--     CLOSED / UNSUCCESSFUL). type and source are codes of the reference sets
--     contact_types and contact_sources. marketing is
--     {granted, granted_at, source, recorded_by}. A soft-deleted contact has
--     deleted_at; a merged one is a tombstone with merged_into (the contact
--     it was merged into), keeping its own emails and phones.
--   * contact_emails (lower-cased) and contact_phones (E.164), indexed per
--     book: duplicates and automatic links are matched on them, exactly.
--   * contact_tags, contact_notes (append-only; merged_from is the contact a
--     note was written on before a merge), contact_reminders (notified_at is
--     when the worker wrote CONTACT_REMINDER_DUE), contact_events (the
--     activity log: stage and owner changes, edits, links, merges) and
--     contact_links (kind + id: a listing, property request, business
--     enquiry or appointment; auto for a link made by matching).
--   * organisation_audit_events gains the actions CONTACT_OWNER_CHANGED,
--     CONTACTS_MERGED, CONTACTS_IMPORTED and CONTACTS_EXPORTED, and the
--     target types CONTACT and CONTACT_BOOK.
--   * reference_items gains the sets contact_types and contact_sources.
--
-- Safe on a database with data: new tables, wider checks and new reference rows.

CREATE TABLE IF NOT EXISTS contacts (
    id              uuid PRIMARY KEY,
    organisation_id uuid REFERENCES organisations (id) ON DELETE CASCADE,
    book_user_id    uuid REFERENCES users (id) ON DELETE CASCADE,
    name            text NOT NULL CONSTRAINT contacts_name_length CHECK (char_length(name) BETWEEN 1 AND 200),
    type            text NOT NULL,
    stage           text NOT NULL DEFAULT 'NEW' CONSTRAINT contacts_stage_check CHECK (stage IN (
                        'NEW', 'CONTACTED', 'VIEWING_REQUESTED', 'VIEWING_CONFIRMED', 'NEGOTIATING', 'CLOSED', 'UNSUCCESSFUL')),
    owner_user_id   uuid REFERENCES users (id) ON DELETE SET NULL,
    source          text,
    marketing       jsonb,
    last_contact_at timestamptz,
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    merged_into     uuid REFERENCES contacts (id),
    CONSTRAINT contacts_book_check CHECK ((organisation_id IS NULL) <> (book_user_id IS NULL)),
    CONSTRAINT contacts_merged_check CHECK (merged_into IS NULL OR merged_into <> id)
);
CREATE INDEX IF NOT EXISTS contacts_organisation_idx ON contacts (organisation_id, created_at DESC)
    WHERE organisation_id IS NOT NULL AND deleted_at IS NULL AND merged_into IS NULL;
CREATE INDEX IF NOT EXISTS contacts_book_user_idx ON contacts (book_user_id, created_at DESC)
    WHERE book_user_id IS NOT NULL AND deleted_at IS NULL AND merged_into IS NULL;
CREATE INDEX IF NOT EXISTS contacts_owner_idx ON contacts (owner_user_id) WHERE owner_user_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS contact_emails (
    contact_id      uuid NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    organisation_id uuid,
    book_user_id    uuid,
    email           text NOT NULL CONSTRAINT contact_emails_lower CHECK (email = lower(email) AND char_length(email) <= 254),
    position        integer NOT NULL,
    PRIMARY KEY (contact_id, email)
);
CREATE INDEX IF NOT EXISTS contact_emails_organisation_idx ON contact_emails (organisation_id, email) WHERE organisation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS contact_emails_book_user_idx ON contact_emails (book_user_id, email) WHERE book_user_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS contact_phones (
    contact_id      uuid NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    organisation_id uuid,
    book_user_id    uuid,
    phone           text NOT NULL CONSTRAINT contact_phones_e164 CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
    position        integer NOT NULL,
    PRIMARY KEY (contact_id, phone)
);
CREATE INDEX IF NOT EXISTS contact_phones_organisation_idx ON contact_phones (organisation_id, phone) WHERE organisation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS contact_phones_book_user_idx ON contact_phones (book_user_id, phone) WHERE book_user_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS contact_tags (
    contact_id uuid NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    tag        text NOT NULL CONSTRAINT contact_tags_length CHECK (char_length(tag) BETWEEN 1 AND 40),
    position   integer NOT NULL,
    PRIMARY KEY (contact_id, tag)
);
CREATE INDEX IF NOT EXISTS contact_tags_tag_idx ON contact_tags (tag);

CREATE TABLE IF NOT EXISTS contact_notes (
    id             uuid PRIMARY KEY,
    contact_id     uuid NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    text           text NOT NULL CONSTRAINT contact_notes_length CHECK (char_length(text) BETWEEN 1 AND 5000),
    author_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    author_name    text NOT NULL DEFAULT '',
    merged_from    uuid,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS contact_notes_contact_idx ON contact_notes (contact_id, created_at);

CREATE TABLE IF NOT EXISTS contact_reminders (
    id              uuid PRIMARY KEY,
    contact_id      uuid NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    due_at          timestamptz NOT NULL,
    text            text NOT NULL CONSTRAINT contact_reminders_length CHECK (char_length(text) BETWEEN 1 AND 500),
    done            boolean NOT NULL DEFAULT false,
    done_at         timestamptz,
    done_by         uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by_name text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    notified_at     timestamptz
);
CREATE INDEX IF NOT EXISTS contact_reminders_contact_idx ON contact_reminders (contact_id, due_at);
CREATE INDEX IF NOT EXISTS contact_reminders_due_idx ON contact_reminders (due_at) WHERE NOT done;

CREATE TABLE IF NOT EXISTS contact_events (
    id            uuid PRIMARY KEY,
    contact_id    uuid NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    kind          text NOT NULL CONSTRAINT contact_events_kind_check CHECK (kind IN (
                      'CREATED', 'EDITED', 'STAGE_CHANGED', 'OWNER_CHANGED', 'LISTING_LINKED', 'LISTING_UNLINKED',
                      'ITEM_LINKED', 'MERGED', 'MERGED_INTO', 'DELETED', 'REMINDER_ADDED', 'REMINDER_DONE',
                      'REMINDER_REOPENED', 'IMPORTED')),
    actor_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_name    text NOT NULL DEFAULT '',
    data          jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS contact_events_contact_idx ON contact_events (contact_id, created_at);

CREATE TABLE IF NOT EXISTS contact_links (
    contact_id uuid NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    kind       text NOT NULL CONSTRAINT contact_links_kind_check CHECK (kind IN (
                   'LISTING', 'PROPERTY_REQUEST', 'BUSINESS_ENQUIRY', 'APPOINTMENT')),
    item_id    uuid NOT NULL,
    auto       boolean NOT NULL DEFAULT false,
    linked_by  uuid REFERENCES users (id) ON DELETE SET NULL,
    linked_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (contact_id, kind, item_id)
);
CREATE INDEX IF NOT EXISTS contact_links_item_idx ON contact_links (kind, item_id);

-- The organisation audit trail records reassignments, merges, imports and exports.
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_action_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_action_check CHECK (action IN (
    'INVITATION_CREATED', 'INVITATION_CANCELLED', 'INVITATION_ACCEPTED',
    'JOIN_REQUEST_APPROVED', 'JOIN_REQUEST_DECLINED', 'JOIN_REQUEST_CHANGES_REQUESTED',
    'MEMBER_ROLE_CHANGED', 'MEMBER_JOB_TITLE_CHANGED', 'MEMBER_SUSPENDED',
    'MEMBER_REACTIVATED', 'MEMBER_REMOVED', 'REQUEST_ASSIGNED', 'REQUEST_UNASSIGNED',
    'CONTACT_OWNER_CHANGED', 'CONTACTS_MERGED', 'CONTACTS_IMPORTED', 'CONTACTS_EXPORTED'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK'));

-- Reference data: the contact types (FR-014: buyer, tenant, landlord, owner)
-- and the contact sources.
ALTER TABLE reference_items DROP CONSTRAINT reference_items_set_check;
ALTER TABLE reference_items ADD CONSTRAINT reference_items_set_check CHECK (set_code IN (
        'deal_types', 'property_categories', 'property_types', 'request_types', 'contact_methods',
        'account_roles', 'directory_kinds', 'service_trades', 'regions', 'cities', 'suburbs', 'listing_features',
        'onboarding_documents', 'organisation_kinds', 'price_periods', 'contact_types', 'contact_sources'));

INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_types', 'buyer', 'Buyer', NULL, 10, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_types', 'tenant', 'Tenant', NULL, 20, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_types', 'landlord', 'Landlord', NULL, 30, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_types', 'owner', 'Owner', NULL, 40, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_types', 'other', 'Other', NULL, 50, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_sources', 'enquiry', 'Enquiry', NULL, 10, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_sources', 'walk-in', 'Walk-in', NULL, 20, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_sources', 'referral', 'Referral', NULL, 30, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_sources', 'phone', 'Phone call', NULL, 40, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_sources', 'import', 'Import', NULL, 50, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_sources', 'website', 'Website', NULL, 60, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('contact_sources', 'other', 'Other', NULL, 70, NULL, NULL) ON CONFLICT (set_code, code) DO NOTHING;
