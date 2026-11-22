-- API 0.62.0: personal time zone, display only. MARKET (the default: the
-- viewer's market zone), DEVICE (the client's own zone) or ZONE with an IANA
-- name. Nothing else is converted or migrated.
ALTER TABLE users
    ADD COLUMN time_zone_mode text NOT NULL DEFAULT 'MARKET'
        CONSTRAINT users_time_zone_mode_check CHECK (time_zone_mode IN ('MARKET', 'DEVICE', 'ZONE')),
    ADD COLUMN time_zone text
        CONSTRAINT users_time_zone_check CHECK (time_zone IS NULL OR char_length(time_zone) BETWEEN 1 AND 64),
    ADD CONSTRAINT users_time_zone_named_check CHECK ((time_zone_mode = 'ZONE') = (time_zone IS NOT NULL));
