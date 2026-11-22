ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_time_zone_named_check,
    DROP COLUMN IF EXISTS time_zone,
    DROP COLUMN IF EXISTS time_zone_mode;
