-- 0031: questionnaires and applications (API 0.32.0, BR-010, FR-015,
-- FR-017 to FR-019).
--
--   * forms: one VERSION of a screening questionnaire. A family (family_id)
--     has versions 1, 2, ...; at most one DRAFT and one PUBLISHED at a time
--     (editing a PUBLISHED version creates the next DRAFT, as lease
--     templates do). A form belongs to an organisation (organisation_id) or
--     an approved direct owner (owner_user_id); exactly one is set.
--     sections is the definition (sections > fields, with types, options,
--     limits and visible_if), validated by the API.
--   * forms_frozen_after_submission: a version that has a submitted
--     application can never change its content (only its status, so it can
--     be retired) and can never be deleted (FR-017: an application keeps the
--     exact questions it was submitted on).
--   * listing_application_intake: the one questionnaire of a listing
--     (FR-019) and whether intake is OPEN, PAUSED or CLOSED, with an
--     optional closing time.
--   * applications: a customer's application to a listing, on one form
--     version (form_id), with the answers as jsonb and a reference
--     (RPA-APP-<year>-<6 digits>, application_reference_seq) once submitted.
--     organisation_id / owner_user_id say who receives it (the listing's
--     organisation, or its direct owner), copied at creation. erased_at is
--     set when retention or account deletion removed the answers and files;
--     the row stays as a tombstone (reference, status, dates, listing).
--   * application_files: private uploads (object storage; served only by the
--     API). application_file_purges queues the objects of removed files for
--     the worker, so a database transaction never has to reach the storage.
--   * application_events: the timeline. application_notes: internal notes,
--     never shown to the applicant.
--   * contact_links gains the kind APPLICATION (CRM auto-link).
--   * organisation_audit_events gains APPLICATION_VIEWED and
--     APPLICATION_FILE_VIEWED (FR-018) and the target type APPLICATION.
--
-- Safe on a database with data: new tables and wider checks.

CREATE TABLE IF NOT EXISTS forms (
    id              uuid PRIMARY KEY,
    family_id       uuid NOT NULL,
    version         integer NOT NULL CHECK (version >= 1),
    status          text NOT NULL CHECK (status IN ('DRAFT', 'PUBLISHED', 'RETIRED')),
    organisation_id uuid REFERENCES organisations (id) ON DELETE CASCADE,
    owner_user_id   uuid REFERENCES users (id) ON DELETE CASCADE,
    market_code     text NOT NULL,
    name            text NOT NULL,
    purpose         text NOT NULL DEFAULT '',
    privacy_notice  text NOT NULL DEFAULT '',
    retention_days  integer NOT NULL CHECK (retention_days BETWEEN 30 AND 3650),
    expires_at      timestamptz,
    sections        jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    created_by_name text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz,
    retired_at      timestamptz,
    CONSTRAINT forms_owner_check CHECK ((organisation_id IS NULL) <> (owner_user_id IS NULL)),
    CONSTRAINT forms_family_version_key UNIQUE (family_id, version)
);
CREATE UNIQUE INDEX IF NOT EXISTS forms_one_draft_idx ON forms (family_id) WHERE status = 'DRAFT';
CREATE UNIQUE INDEX IF NOT EXISTS forms_one_published_idx ON forms (family_id) WHERE status = 'PUBLISHED';
CREATE INDEX IF NOT EXISTS forms_organisation_idx ON forms (organisation_id, created_at DESC) WHERE organisation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS forms_owner_idx ON forms (owner_user_id, created_at DESC) WHERE owner_user_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS listing_application_intake (
    listing_id uuid PRIMARY KEY REFERENCES listings (id) ON DELETE CASCADE,
    form_id    uuid NOT NULL REFERENCES forms (id),
    status     text NOT NULL CHECK (status IN ('OPEN', 'PAUSED', 'CLOSED')),
    closes_at  timestamptz,
    updated_by uuid REFERENCES users (id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS listing_application_intake_form_idx ON listing_application_intake (form_id);

CREATE SEQUENCE IF NOT EXISTS application_reference_seq;

CREATE TABLE IF NOT EXISTS applications (
    id                uuid PRIMARY KEY,
    reference         text UNIQUE,
    listing_id        uuid NOT NULL REFERENCES listings (id),
    form_id           uuid NOT NULL REFERENCES forms (id),
    form_version      integer NOT NULL,
    applicant_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    applicant_name    text NOT NULL DEFAULT '',
    organisation_id   uuid REFERENCES organisations (id) ON DELETE CASCADE,
    owner_user_id     uuid REFERENCES users (id) ON DELETE SET NULL,
    status            text NOT NULL CHECK (status IN ('DRAFT', 'SUBMITTED', 'UNDER_REVIEW', 'MORE_INFO', 'SHORTLISTED',
                          'ACCEPTED', 'REJECTED', 'WITHDRAWN')),
    answers           jsonb NOT NULL DEFAULT '{}'::jsonb,
    listing_snapshot  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    submitted_at      timestamptz,
    last_submitted_at timestamptz,
    decided_at        timestamptz,
    erased_at         timestamptz,
    CONSTRAINT applications_reference_check CHECK (status = 'DRAFT' OR reference IS NOT NULL OR erased_at IS NOT NULL OR status = 'WITHDRAWN')
);
-- One application per customer, listing and form version (POST is idempotent).
CREATE UNIQUE INDEX IF NOT EXISTS applications_applicant_key ON applications (applicant_user_id, listing_id, form_id)
    WHERE applicant_user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS applications_organisation_idx ON applications (organisation_id, last_submitted_at DESC) WHERE status <> 'DRAFT';
CREATE INDEX IF NOT EXISTS applications_owner_idx ON applications (owner_user_id, last_submitted_at DESC) WHERE status <> 'DRAFT';
CREATE INDEX IF NOT EXISTS applications_listing_idx ON applications (listing_id);
CREATE INDEX IF NOT EXISTS applications_form_idx ON applications (form_id);
CREATE INDEX IF NOT EXISTS applications_retention_idx ON applications (decided_at)
    WHERE erased_at IS NULL AND status IN ('ACCEPTED', 'REJECTED', 'WITHDRAWN');

CREATE TABLE IF NOT EXISTS application_files (
    id             uuid PRIMARY KEY,
    application_id uuid NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    field_key      text NOT NULL,
    file_name      text NOT NULL,
    content_type   text NOT NULL,
    size_bytes     bigint NOT NULL CHECK (size_bytes > 0),
    sha256         text NOT NULL,
    object_key     text NOT NULL UNIQUE,
    uploaded_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS application_files_application_idx ON application_files (application_id, uploaded_at);

CREATE TABLE IF NOT EXISTS application_file_purges (
    object_key text PRIMARY KEY,
    queued_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS application_events (
    id             uuid PRIMARY KEY,
    application_id uuid NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    kind           text NOT NULL CHECK (kind IN ('CREATED', 'SUBMITTED', 'RESUBMITTED', 'STATUS_CHANGED', 'INFO_REQUESTED',
                       'WITHDRAWN', 'ERASED')),
    from_status    text,
    to_status      text,
    message        text,
    actor_type     text NOT NULL CHECK (actor_type IN ('APPLICANT', 'STAFF', 'SYSTEM')),
    actor_user_id  uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_name     text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS application_events_application_idx ON application_events (application_id, created_at);

CREATE TABLE IF NOT EXISTS application_notes (
    id             uuid PRIMARY KEY,
    application_id uuid NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    text           text NOT NULL,
    author_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    author_name    text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS application_notes_application_idx ON application_notes (application_id, created_at);

-- A form version with a submitted application is frozen: its content can
-- never change and it can never be deleted. Its status may still move
-- (PUBLISHED -> RETIRED).
CREATE OR REPLACE FUNCTION forms_frozen_after_submission() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM applications WHERE form_id = OLD.id AND submitted_at IS NOT NULL) THEN
        IF TG_OP = 'DELETE' THEN
            RAISE EXCEPTION 'form % has submitted applications and cannot be deleted', OLD.id USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.family_id IS DISTINCT FROM OLD.family_id OR NEW.version IS DISTINCT FROM OLD.version
           OR NEW.organisation_id IS DISTINCT FROM OLD.organisation_id OR NEW.owner_user_id IS DISTINCT FROM OLD.owner_user_id
           OR NEW.market_code IS DISTINCT FROM OLD.market_code OR NEW.name IS DISTINCT FROM OLD.name
           OR NEW.purpose IS DISTINCT FROM OLD.purpose OR NEW.privacy_notice IS DISTINCT FROM OLD.privacy_notice
           OR NEW.retention_days IS DISTINCT FROM OLD.retention_days OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
           OR NEW.sections IS DISTINCT FROM OLD.sections THEN
            RAISE EXCEPTION 'form % has submitted applications and cannot change', OLD.id USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS forms_frozen_after_submission ON forms;
CREATE TRIGGER forms_frozen_after_submission BEFORE UPDATE OR DELETE ON forms
    FOR EACH ROW EXECUTE FUNCTION forms_frozen_after_submission();

-- The CRM links submitted applications to contacts.
ALTER TABLE contact_links DROP CONSTRAINT IF EXISTS contact_links_kind_check;
ALTER TABLE contact_links ADD CONSTRAINT contact_links_kind_check CHECK (kind IN (
    'LISTING', 'PROPERTY_REQUEST', 'BUSINESS_ENQUIRY', 'APPOINTMENT', 'APPLICATION'));

-- The organisation audit trail records who read an application (FR-018).
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_action_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_action_check CHECK (action IN (
    'INVITATION_CREATED', 'INVITATION_CANCELLED', 'INVITATION_ACCEPTED',
    'JOIN_REQUEST_APPROVED', 'JOIN_REQUEST_DECLINED', 'JOIN_REQUEST_CHANGES_REQUESTED',
    'MEMBER_ROLE_CHANGED', 'MEMBER_JOB_TITLE_CHANGED', 'MEMBER_SUSPENDED',
    'MEMBER_REACTIVATED', 'MEMBER_REMOVED', 'REQUEST_ASSIGNED', 'REQUEST_UNASSIGNED',
    'CONTACT_OWNER_CHANGED', 'CONTACTS_MERGED', 'CONTACTS_IMPORTED', 'CONTACTS_EXPORTED',
    'APPLICATION_VIEWED', 'APPLICATION_FILE_VIEWED'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION'));
