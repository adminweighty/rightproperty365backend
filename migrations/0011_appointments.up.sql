-- 0011_appointments: appointments for property viewings and consultations.
--
-- An appointment is created together with a viewing or consultation request
-- that carries a preferred slot (same transaction), or explicitly by a
-- signed-in customer. It is routed with the rules of property requests and
-- business enquiries. Every change of status is one immutable row in
-- appointment_timeline and one outbox event.
--
-- The customer's contact details are a private snapshot: they are returned
-- only to the authorised professional. Listing and profile names are a
-- snapshot too, so the history stays readable after a listing expires.
--
-- NOT RUN: there is no PostgreSQL on the machine this was written on.

CREATE TABLE appointments (
    id                        uuid PRIMARY KEY,
    reference                 text NOT NULL CONSTRAINT appointments_reference_key UNIQUE,
    market_id                 uuid NOT NULL REFERENCES markets (id),
    -- The market's IANA time zone at creation; times are shown to people in it.
    timezone                  text NOT NULL,
    type                      text NOT NULL CONSTRAINT appointments_type_check
                              CHECK (type IN ('VIEWING', 'CONSULTATION')),
    status                    text NOT NULL CONSTRAINT appointments_status_check
                              CHECK (status IN ('REQUESTED', 'CONFIRMED', 'RESCHEDULE_REQUESTED', 'COMPLETED', 'NO_SHOW', 'CANCELLED')),

    -- What it is about. No ON DELETE CASCADE: an appointment outlives a
    -- listing's publication, and the names below are kept as they were.
    listing_id                uuid REFERENCES listings (id),
    listing_slug              text,
    listing_title             text,
    listing_reference         text,
    -- The directory profile it was addressed to (explicit creation only).
    profile_kind              text,
    profile_slug              text,
    -- The PUBLIC profile of the professional or business the customer meets.
    with_kind                 text,
    with_slug                 text,
    with_name                 text,

    -- What created it.
    property_request_id       uuid REFERENCES property_requests (id),
    business_enquiry_id       uuid REFERENCES business_enquiries (id),
    linked_reference          text,

    -- The customer: an account (NULL for a guest) and a private snapshot.
    customer_user_id          uuid REFERENCES users (id),
    customer_name             text NOT NULL,
    customer_email            text NOT NULL,
    customer_phone            text,
    preferred_contact_method  text NOT NULL,
    customer_note             text CONSTRAINT appointments_customer_note_length CHECK (char_length(customer_note) <= 2000),

    -- The professional side. recipient_user_id is the assigned user (the
    -- agent, or the direct owner of the listing); NULL means the organisation.
    recipient_type            text NOT NULL CONSTRAINT appointments_recipient_type_check
                              CHECK (recipient_type IN ('AGENT', 'ORGANISATION', 'SELLER')),
    recipient_organisation_id uuid REFERENCES organisations (id),
    recipient_user_id         uuid REFERENCES users (id),

    start_at                  timestamptz NOT NULL,
    duration_minutes          integer NOT NULL DEFAULT 30 CONSTRAINT appointments_duration_check
                              CHECK (duration_minutes BETWEEN 15 AND 240),
    -- start_at + duration_minutes, stored so that range queries need no
    -- (non-immutable) timestamp arithmetic.
    end_at                    timestamptz NOT NULL,
    location_note             text CONSTRAINT appointments_location_note_length CHECK (char_length(location_note) <= 200),

    proposed_start_at         timestamptz,
    proposed_by               text CONSTRAINT appointments_proposed_by_check CHECK (proposed_by IN ('CUSTOMER', 'PROFESSIONAL')),
    cancelled_by              text CONSTRAINT appointments_cancelled_by_check CHECK (cancelled_by IN ('CUSTOMER', 'PROFESSIONAL', 'SYSTEM')),
    cancel_reason             text CONSTRAINT appointments_cancel_reason_length CHECK (char_length(cancel_reason) <= 500),

    -- Explicit creation only. The unique constraint makes concurrent retries
    -- safe; the key space is separate from requests and enquiries.
    idempotency_key           text CONSTRAINT appointments_idempotency_key_key UNIQUE,
    request_hash              text,
    consent_privacy_version   text,
    consent_accepted_at       timestamptz,

    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT appointments_end_check CHECK (end_at > start_at),
    CONSTRAINT appointments_target_check CHECK (listing_id IS NOT NULL OR profile_slug IS NOT NULL),
    CONSTRAINT appointments_recipient_check
        CHECK (recipient_organisation_id IS NOT NULL OR recipient_user_id IS NOT NULL),
    CONSTRAINT appointments_proposal_check
        CHECK ((status = 'RESCHEDULE_REQUESTED') = (proposed_start_at IS NOT NULL)
               AND (proposed_start_at IS NULL) = (proposed_by IS NULL)),
    CONSTRAINT appointments_cancel_check CHECK ((status = 'CANCELLED') = (cancelled_by IS NOT NULL))
);
-- One appointment per request at most.
CREATE UNIQUE INDEX appointments_property_request_key ON appointments (property_request_id)
    WHERE property_request_id IS NOT NULL;
CREATE INDEX appointments_customer_idx ON appointments (customer_user_id, start_at)
    WHERE customer_user_id IS NOT NULL;
CREATE INDEX appointments_recipient_org_idx ON appointments (recipient_organisation_id, start_at)
    WHERE recipient_organisation_id IS NOT NULL;
CREATE INDEX appointments_recipient_user_idx ON appointments (recipient_user_id, start_at)
    WHERE recipient_user_id IS NOT NULL;
CREATE INDEX appointments_listing_idx ON appointments (listing_id, start_at) WHERE listing_id IS NOT NULL;
-- The overlap check reads a professional's CONFIRMED appointments.
CREATE INDEX appointments_confirmed_idx
    ON appointments ((COALESCE(recipient_user_id, recipient_organisation_id)), start_at)
    WHERE status = 'CONFIRMED';

-- One professional (the assigned user, or the organisation when nobody is
-- assigned) cannot have two CONFIRMED appointments that overlap. This is
-- enforced by a LOCKED CHECK inside the confirming transaction, not by an
-- exclusion constraint: the store takes pg_advisory_xact_lock on the
-- professional's key, looks for an overlapping CONFIRMED row and only then
-- writes (internal/modules/appointments/postgres.go). An exclusion constraint
-- would need the btree_gist extension, which this schema does not require.

CREATE TABLE appointment_timeline (
    id             uuid PRIMARY KEY,
    appointment_id uuid NOT NULL REFERENCES appointments (id) ON DELETE CASCADE,
    event          text NOT NULL CONSTRAINT appointment_timeline_event_check
                   CHECK (event IN ('REQUESTED', 'CONFIRMED', 'RESCHEDULE_REQUESTED', 'CANCELLED', 'COMPLETED', 'NO_SHOW', 'ASSIGNED')),
    actor_type     text NOT NULL CONSTRAINT appointment_timeline_actor_type_check
                   CHECK (actor_type IN ('CUSTOMER', 'PROFESSIONAL', 'SYSTEM')),
    actor_user_id  uuid REFERENCES users (id),
    from_status    text,
    to_status      text NOT NULL,
    note           text,
    -- The time the entry is about (requested, confirmed or proposed start).
    start_at       timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX appointment_timeline_appointment_idx ON appointment_timeline (appointment_id, created_at);

-- Rows are immutable (losing the actor to an account deletion is the one
-- column change allowed), as onboarding_history is.
CREATE FUNCTION appointment_timeline_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.appointment_id IS DISTINCT FROM OLD.appointment_id
       OR NEW.event IS DISTINCT FROM OLD.event
       OR NEW.actor_type IS DISTINCT FROM OLD.actor_type
       OR NEW.from_status IS DISTINCT FROM OLD.from_status
       OR NEW.to_status IS DISTINCT FROM OLD.to_status
       OR NEW.note IS DISTINCT FROM OLD.note
       OR NEW.start_at IS DISTINCT FROM OLD.start_at
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (NEW.actor_user_id IS DISTINCT FROM OLD.actor_user_id AND NEW.actor_user_id IS NOT NULL) THEN
        RAISE EXCEPTION 'appointment_timeline rows are immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER appointment_timeline_no_update BEFORE UPDATE ON appointment_timeline
    FOR EACH ROW EXECUTE FUNCTION appointment_timeline_immutable();
