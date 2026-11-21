-- API 0.51.0: appointment availability (FR-020), consultation services
-- (FR-022) and appointment reminders (FR-021).

-- A professional's own bookable hours. Rules in Go
-- (appointments.SettingsInput.apply); the JSON arrays hold
-- [{weekday, opens, closes}] and [{date, closed, opens, closes, note}].
CREATE TABLE appointment_settings (
    user_id          uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    enabled          boolean     NOT NULL DEFAULT false,
    timezone         text        NOT NULL,
    working_hours    jsonb       NOT NULL DEFAULT '[]'::jsonb,
    exceptions       jsonb       NOT NULL DEFAULT '[]'::jsonb,
    slot_minutes     integer     NOT NULL DEFAULT 30 CHECK (slot_minutes BETWEEN 15 AND 240 AND slot_minutes % 15 = 0),
    buffer_minutes   integer     NOT NULL DEFAULT 0 CHECK (buffer_minutes BETWEEN 0 AND 120),
    lead_time_hours  integer     NOT NULL DEFAULT 24 CHECK (lead_time_hours BETWEEN 0 AND 336),
    horizon_days     integer     NOT NULL DEFAULT 30 CHECK (horizon_days BETWEEN 1 AND 180),
    max_per_day      integer     NOT NULL DEFAULT 0 CHECK (max_per_day BETWEEN 0 AND 50),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- Free consultation services a professional offers. Paid services are not
-- enabled (FR-022 needs a payment rule and an app-store review first).
CREATE TABLE consultation_services (
    id               uuid PRIMARY KEY,
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name             text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    duration_minutes integer     NOT NULL CHECK (duration_minutes BETWEEN 15 AND 240),
    channel          text        NOT NULL CHECK (channel IN ('IN_PERSON', 'VIDEO', 'PHONE')),
    location         text        CHECK (char_length(location) <= 200),
    notes            text        CHECK (char_length(notes) <= 500),
    active           boolean     NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL
);
CREATE INDEX consultation_services_user_idx ON consultation_services (user_id, created_at);

-- The service an appointment was booked for (a snapshot: the service may be
-- archived later), and the start time the reminder was sent for.
ALTER TABLE appointments
    ADD COLUMN service_id         uuid REFERENCES consultation_services (id) ON DELETE SET NULL,
    ADD COLUMN service_name       text,
    ADD COLUMN service_channel    text CHECK (service_channel IN ('IN_PERSON', 'VIDEO', 'PHONE')),
    ADD COLUMN reminded_for_start timestamptz;

-- The reminder sweep: confirmed appointments by start time.
CREATE INDEX appointments_reminder_idx ON appointments (start_at) WHERE status = 'CONFIRMED';
