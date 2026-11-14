-- Reverts 0006_email_messages. Destroys the record of which emails were
-- attempted and accepted by the provider. After this, a notification that is
-- retried can only be deduplicated by the provider's own 24-hour idempotency window.
DROP TABLE IF EXISTS email_messages;
