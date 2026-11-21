DROP INDEX IF EXISTS appointments_reminder_idx;
ALTER TABLE appointments
    DROP COLUMN IF EXISTS reminded_for_start,
    DROP COLUMN IF EXISTS service_channel,
    DROP COLUMN IF EXISTS service_name,
    DROP COLUMN IF EXISTS service_id;
DROP TABLE IF EXISTS consultation_services;
DROP TABLE IF EXISTS appointment_settings;
