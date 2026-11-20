ALTER TABLE property_requests
    DROP COLUMN IF EXISTS professional_last_read_at,
    DROP COLUMN IF EXISTS customer_last_read_at,
    DROP COLUMN IF EXISTS last_message_author,
    DROP COLUMN IF EXISTS last_message_at,
    DROP COLUMN IF EXISTS message_count;
DROP TABLE IF EXISTS property_request_messages;
