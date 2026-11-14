-- 0015_availability: the availability calendar of short-stay listings, and
-- requests to stay.
--
-- NOT a booking engine: no payments, no instant booking, no holds. The lister
-- marks nights unavailable; a guest asks to stay through a property request
-- (STAY_REQUEST), which does not block the dates.
--
-- Written for a database that already holds data (migrations 0001-0014 have
-- run or will have run on staging). Only new tables and nullable columns are
-- added; no existing row is rewritten. The reference row is an upsert and
-- keeps an existing row's `active` flag.

-- ---------------------------------------------------------------------------
-- Unavailable nights. Every night of a short stay is AVAILABLE unless a row
-- here covers it. Nights are calendar dates in the time zone of the
-- listing's country; a row is an inclusive range of them, with a PRIVATE
-- reason (BLOCKED: the owner's choice; BOOKED: taken) and an optional
-- PRIVATE note. The API keeps a listing's ranges sorted, merged and
-- non-overlapping, inside one transaction that locks the listing row; the
-- unique index is a backstop, not the rule (an exclusion constraint would
-- need the btree_gist extension).
-- ---------------------------------------------------------------------------
CREATE TABLE listing_availability (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id  uuid NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
    first_night date NOT NULL,
    last_night  date NOT NULL,
    reason      text NOT NULL CONSTRAINT listing_availability_reason_check CHECK (reason IN ('BLOCKED', 'BOOKED')),
    -- PRIVATE to the lister. Plain text.
    note        text CONSTRAINT listing_availability_note_check
                CHECK (char_length(note) BETWEEN 1 AND 200 AND note !~ '[<>[:cntrl:]]'),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  uuid NOT NULL REFERENCES users (id),
    CONSTRAINT listing_availability_order_check CHECK (first_night <= last_night),
    CONSTRAINT listing_availability_first_night_key UNIQUE (listing_id, first_night)
);
-- "Which listings have an unavailable night between two dates?"
CREATE INDEX listing_availability_nights_idx ON listing_availability (first_night, last_night);

-- ---------------------------------------------------------------------------
-- The audit trail: who changed which nights to what, and when. One row per
-- change as it was asked for; rows are never updated.
-- ---------------------------------------------------------------------------
CREATE TABLE listing_availability_audit (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id    uuid NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
    actor_user_id uuid NOT NULL REFERENCES users (id),
    first_night   date NOT NULL,
    last_night    date NOT NULL,
    status        text NOT NULL CONSTRAINT listing_availability_audit_status_check CHECK (status IN ('AVAILABLE', 'BLOCKED', 'BOOKED')),
    note          text,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX listing_availability_audit_listing_idx ON listing_availability_audit (listing_id, created_at DESC);

-- When the lister last changed the calendar; NULL when they never have.
ALTER TABLE listings ADD COLUMN availability_updated_at timestamptz;

-- "My listings": the listings of an organisation or of a direct owner, whatever their status.
CREATE INDEX listings_organisation_idx ON listings (organisation_id) WHERE organisation_id IS NOT NULL;
CREATE INDEX listings_seller_idx ON listings (seller_user_id) WHERE seller_user_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Requests to stay: a property request of type STAY_REQUEST carries the
-- dates and the number of guests asked for. NULL for every other type.
-- ---------------------------------------------------------------------------
ALTER TABLE property_requests
    ADD COLUMN check_in  date,
    ADD COLUMN check_out date,
    ADD COLUMN guests    integer CONSTRAINT property_requests_guests_check CHECK (guests BETWEEN 1 AND 100),
    ADD CONSTRAINT property_requests_stay_check CHECK (
        (check_in IS NULL AND check_out IS NULL AND guests IS NULL)
        OR (check_in IS NOT NULL AND check_out IS NOT NULL AND guests IS NOT NULL AND check_out > check_in));

INSERT INTO reference_items (set_code, code, label, description, sort_order, market_id, parent_code) VALUES ('request_types', 'STAY_REQUEST', 'Request to stay', NULL, 50, NULL, NULL) ON CONFLICT (set_code, code) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description, sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code, updated_at = now();
