-- The audit target stays widened: audit rows may reference it.
DROP TABLE IF EXISTS email_delivery_alerts;
DROP TABLE IF EXISTS email_suppressions;
DROP TABLE IF EXISTS email_delivery_events;
DROP INDEX IF EXISTS email_messages_provider_message_idx;
ALTER TABLE email_messages DROP COLUMN IF EXISTS delivery_state, DROP COLUMN IF EXISTS delivery_state_at, DROP COLUMN IF EXISTS bounce_type;
