-- 0032: showroom and venue hourly rental (API 0.33.0, BR-007, FR-022 to
-- FR-025).
--
--   * reference_items gains hourly_bookable: true for the property types
--     that may be booked by the hour (admin-editable; NULL outside
--     property_types). New commercial types: showroom, venue / event space,
--     hall, conference room and studio; office becomes hourly-bookable.
--   * venue_settings: a listing's hourly settings (rate, ISO currency,
--     increment, durations, cleaning buffer, capacity, deposit, opening
--     hours and exceptions as jsonb, cancellation policy, lead time,
--     horizon). Its row is also the PER-LISTING LOCK: a confirmation takes
--     it FOR UPDATE before it re-checks the CONFIRMED bookings.
--   * venue_fees: tax and fee rules (PERCENT, FIXED, PER_HOUR) with
--     inclusive effective dates; the tax is the fee whose code is TAX.
--   * venue_quotes: priced quotes (30 minutes), one currency each; a
--     booking request, a reschedule or a proposal uses one (used_by).
--   * venue_bookings: the bookings (service type VENUE_HOURLY, a table of
--     their own, so appointment reports never count them). occupied is the
--     tstzrange of the slot plus its own cleaning buffer, indexed with
--     btree_gist for the capacity checks of the rows that hold capacity.
--   * venue_booking_events: every transition, with actor, time and reason.
--   * organisation_audit_events gains the VENUE_BOOKING_* host decisions
--     and the target type VENUE_BOOKING.
--
-- Safe on a database with data: new tables, a new nullable column and
-- wider checks.

CREATE EXTENSION IF NOT EXISTS btree_gist;

ALTER TABLE reference_items ADD COLUMN IF NOT EXISTS hourly_bookable boolean;
UPDATE reference_items SET hourly_bookable = false WHERE set_code = 'property_types' AND hourly_bookable IS NULL;
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code, hourly_bookable) VALUES ('property_types', 'showroom', 'Showroom', NULL, 130, NULL, 'commercial', true) ON CONFLICT (set_code, code) DO UPDATE SET hourly_bookable = true, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code, hourly_bookable) VALUES ('property_types', 'event-space', 'Venue / event space', NULL, 140, NULL, 'commercial', true) ON CONFLICT (set_code, code) DO UPDATE SET hourly_bookable = true, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code, hourly_bookable) VALUES ('property_types', 'hall', 'Hall', NULL, 150, NULL, 'commercial', true) ON CONFLICT (set_code, code) DO UPDATE SET hourly_bookable = true, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code, hourly_bookable) VALUES ('property_types', 'conference-room', 'Conference room', NULL, 160, NULL, 'commercial', true) ON CONFLICT (set_code, code) DO UPDATE SET hourly_bookable = true, updated_at = now();
INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code, hourly_bookable) VALUES ('property_types', 'studio', 'Studio', NULL, 170, NULL, 'commercial', true) ON CONFLICT (set_code, code) DO UPDATE SET hourly_bookable = true, updated_at = now();
UPDATE reference_items SET hourly_bookable = true, updated_at = now() WHERE set_code = 'property_types' AND code = 'office';

CREATE TABLE IF NOT EXISTS venue_settings (
    listing_id              uuid PRIMARY KEY REFERENCES listings (id) ON DELETE CASCADE,
    enabled                 boolean NOT NULL DEFAULT false,
    currency                text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    rate_per_hour           numeric(18, 3) NOT NULL CHECK (rate_per_hour > 0),
    increment_minutes       integer NOT NULL CHECK (increment_minutes IN (15, 30, 60)),
    min_minutes             integer NOT NULL CHECK (min_minutes BETWEEN 15 AND 1440),
    max_minutes             integer NOT NULL CHECK (max_minutes BETWEEN 15 AND 1440),
    cleaning_buffer_minutes integer NOT NULL DEFAULT 0 CHECK (cleaning_buffer_minutes BETWEEN 0 AND 240),
    capacity                integer NOT NULL DEFAULT 1 CHECK (capacity BETWEEN 1 AND 50),
    deposit                 numeric(18, 3) CHECK (deposit >= 0),
    opening_hours           jsonb NOT NULL DEFAULT '[]'::jsonb,
    exceptions              jsonb NOT NULL DEFAULT '[]'::jsonb,
    cancellation_policy     text NOT NULL DEFAULT '',
    free_cancellation_hours integer NOT NULL DEFAULT 0 CHECK (free_cancellation_hours BETWEEN 0 AND 720),
    lead_time_hours         integer NOT NULL DEFAULT 0 CHECK (lead_time_hours BETWEEN 0 AND 720),
    horizon_days            integer NOT NULL DEFAULT 90 CHECK (horizon_days BETWEEN 1 AND 730),
    updated_by              uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CHECK (min_minutes <= max_minutes)
);

CREATE TABLE IF NOT EXISTS venue_fees (
    id             uuid PRIMARY KEY,
    listing_id     uuid NOT NULL REFERENCES venue_settings (listing_id) ON DELETE CASCADE,
    position       integer NOT NULL,
    code           text NOT NULL CHECK (code ~ '^[A-Z][A-Z0-9_]{1,31}$' AND code <> 'RATE'),
    label          text NOT NULL,
    kind           text NOT NULL CHECK (kind IN ('PERCENT', 'FIXED', 'PER_HOUR')),
    amount         numeric(18, 3) NOT NULL CHECK (amount >= 0),
    effective_from date NOT NULL,
    effective_to   date,
    CHECK (effective_to IS NULL OR effective_to >= effective_from),
    CHECK (kind <> 'PERCENT' OR amount <= 100),
    UNIQUE (listing_id, position)
);

CREATE TABLE IF NOT EXISTS venue_quotes (
    id             uuid PRIMARY KEY,
    listing_id     uuid NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
    start_at       timestamptz NOT NULL,
    end_at         timestamptz NOT NULL,
    buffer_minutes integer NOT NULL CHECK (buffer_minutes >= 0),
    time_zone      text NOT NULL,
    currency       text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    pricing        jsonb NOT NULL,
    used_by        uuid,
    created_at     timestamptz NOT NULL,
    expires_at     timestamptz NOT NULL,
    CHECK (end_at > start_at)
);
CREATE INDEX IF NOT EXISTS venue_quotes_expires_idx ON venue_quotes (expires_at) WHERE used_by IS NULL;

CREATE TABLE IF NOT EXISTS venue_bookings (
    id                      uuid PRIMARY KEY,
    reference               text NOT NULL UNIQUE,
    service_type            text NOT NULL DEFAULT 'VENUE_HOURLY' CHECK (service_type = 'VENUE_HOURLY'),
    listing_id              uuid NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
    listing_slug            text NOT NULL DEFAULT '',
    listing_title           text NOT NULL DEFAULT '',
    listing_reference       text NOT NULL DEFAULT '',
    organisation_id         uuid REFERENCES organisations (id) ON DELETE SET NULL,
    seller_user_id          uuid REFERENCES users (id) ON DELETE SET NULL,
    customer_user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    customer_name           text NOT NULL,
    customer_email          text NOT NULL,
    status                  text NOT NULL CHECK (status IN ('REQUESTED', 'CONFIRMED', 'RESCHEDULE_REQUESTED', 'DECLINED',
                                                            'CANCELLED', 'COMPLETED', 'NO_SHOW')),
    start_at                timestamptz NOT NULL,
    end_at                  timestamptz NOT NULL,
    buffer_minutes          integer NOT NULL CHECK (buffer_minutes >= 0),
    -- end_at + buffer_minutes, written by the API (timestamptz + interval
    -- is not immutable, so it cannot be a generated expression).
    block_end_at            timestamptz NOT NULL,
    occupied                tstzrange GENERATED ALWAYS AS (tstzrange(start_at, block_end_at, '[)')) STORED,
    time_zone               text NOT NULL,
    attendees               integer CHECK (attendees BETWEEN 1 AND 10000),
    message                 text,
    quote_id                uuid NOT NULL UNIQUE REFERENCES venue_quotes (id),
    pricing                 jsonb NOT NULL,
    policy_text             text NOT NULL DEFAULT '',
    free_cancellation_hours integer NOT NULL DEFAULT 0,
    held                    boolean NOT NULL DEFAULT false,
    proposal                jsonb,
    cancelled_by            text CHECK (cancelled_by IN ('CUSTOMER', 'HOST')),
    cancel_reason           text,
    cancelled_at            timestamptz,
    cancel_free             boolean,
    decline_reason          text,
    confirmations           integer NOT NULL DEFAULT 0,
    consent_privacy_version text NOT NULL,
    consent_accepted_at     timestamptz NOT NULL,
    created_at              timestamptz NOT NULL,
    updated_at              timestamptz NOT NULL,
    CHECK (end_at > start_at),
    CHECK (block_end_at >= end_at),
    CHECK (status <> 'CONFIRMED' OR held)
);
-- The capacity checks read the rows that hold capacity by overlap.
CREATE INDEX IF NOT EXISTS venue_bookings_occupied_idx ON venue_bookings USING gist (listing_id, occupied)
    WHERE status = 'CONFIRMED' OR (status = 'RESCHEDULE_REQUESTED' AND held);
CREATE INDEX IF NOT EXISTS venue_bookings_customer_idx ON venue_bookings (customer_user_id, start_at);
CREATE INDEX IF NOT EXISTS venue_bookings_organisation_idx ON venue_bookings (organisation_id, start_at);
CREATE INDEX IF NOT EXISTS venue_bookings_seller_idx ON venue_bookings (seller_user_id, start_at);

ALTER TABLE venue_quotes DROP CONSTRAINT IF EXISTS venue_quotes_used_by_fkey;
ALTER TABLE venue_quotes ADD CONSTRAINT venue_quotes_used_by_fkey FOREIGN KEY (used_by) REFERENCES venue_bookings (id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS venue_booking_events (
    id            uuid PRIMARY KEY,
    booking_id    uuid NOT NULL REFERENCES venue_bookings (id) ON DELETE CASCADE,
    action        text NOT NULL CHECK (action IN ('REQUESTED', 'CONFIRMED', 'DECLINED', 'CANCELLED', 'RESCHEDULE_REQUESTED',
                                                  'PROPOSED', 'ACCEPTED', 'COMPLETED', 'NO_SHOW')),
    from_status   text,
    to_status     text NOT NULL,
    actor_type    text NOT NULL CHECK (actor_type IN ('CUSTOMER', 'HOST', 'SYSTEM')),
    actor_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_name    text NOT NULL DEFAULT '',
    reason        text,
    start_at      timestamptz,
    end_at        timestamptz,
    created_at    timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS venue_booking_events_booking_idx ON venue_booking_events (booking_id, created_at);

ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_action_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_action_check CHECK (action IN (
    'INVITATION_CREATED', 'INVITATION_CANCELLED', 'INVITATION_ACCEPTED',
    'JOIN_REQUEST_APPROVED', 'JOIN_REQUEST_DECLINED', 'JOIN_REQUEST_CHANGES_REQUESTED',
    'MEMBER_ROLE_CHANGED', 'MEMBER_JOB_TITLE_CHANGED', 'MEMBER_SUSPENDED',
    'MEMBER_REACTIVATED', 'MEMBER_REMOVED', 'REQUEST_ASSIGNED', 'REQUEST_UNASSIGNED',
    'CONTACT_OWNER_CHANGED', 'CONTACTS_MERGED', 'CONTACTS_IMPORTED', 'CONTACTS_EXPORTED',
    'APPLICATION_VIEWED', 'APPLICATION_FILE_VIEWED',
    'VENUE_BOOKING_CONFIRMED', 'VENUE_BOOKING_DECLINED', 'VENUE_BOOKING_CANCELLED',
    'VENUE_BOOKING_TIME_PROPOSED', 'VENUE_BOOKING_COMPLETED', 'VENUE_BOOKING_NO_SHOW'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION',
                           'VENUE_BOOKING'));
