-- Reverts 0011_appointments. Destroys every appointment and its timeline.
-- Outbox events about appointments are removed too: their appointments no
-- longer exist.
DELETE FROM outbox_events WHERE aggregate_type = 'appointment';

DROP TRIGGER IF EXISTS appointment_timeline_no_update ON appointment_timeline;
DROP FUNCTION IF EXISTS appointment_timeline_immutable();
DROP TABLE IF EXISTS appointment_timeline;
DROP TABLE IF EXISTS appointments;
