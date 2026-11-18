-- 0029: listing analytics (API 0.30.0, BR-006, FR-033 to FR-036).
--
--   * analytics_events: the raw events, browser and server-side. event_id is
--     the primary key: a retry of the same event is never counted twice.
--     local_day is the market-local calendar day of occurred_at, worked out
--     at ingestion from markets.timezone (which every market already has, so
--     no time zone column is added). network is the TRUNCATED client network
--     only (IPv4 /24, IPv6 /48; the check refuses anything finer): the full
--     IP, the user agent and the referrer are never stored. bot rows are kept
--     but excluded from every metric and counted as filtered. Raw events are
--     deleted by the worker after ANALYTICS_RAW_RETENTION_DAYS (default 90)
--     and are never returned by any endpoint.
--   * analytics_late_events: the identity of the events dropped because they
--     arrived more than 3 days after they occurred (the rollup window), with
--     the market-local day of arrival whose late_events_dropped counts them.
--   * analytics_listing_daily: the daily rollup per (listing, market-local
--     day), recomputed for the last 3 local days every 15 minutes and kept
--     after the raw events are gone. analytics_rollup_runs holds the time of
--     the last run (refreshed_at).
--   * No foreign key to listings: figures outlive a listing, and the
--     ingestion has already checked that the listing exists.
--
-- Partitioning: analytics_events is a plain table with an (occurred_at)
-- index, ready to become a range-partitioned table by occurred_at (monthly)
-- as a follow-up. That needs the idempotency key to include the partition
-- key (or a separate event_id table), which is why it is not done here.
--
-- Safe on a database with data: new tables only.

CREATE TABLE IF NOT EXISTS analytics_events (
    event_id    uuid        PRIMARY KEY,
    event_type  text        NOT NULL,
    origin      text        NOT NULL,
    occurred_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    local_day   date        NOT NULL,
    listing_id  uuid        NOT NULL,
    market_code text        NOT NULL,
    placement   text,
    rank        integer,
    source      text,
    device      text,
    session_id  text,
    network     cidr,
    bot         boolean     NOT NULL DEFAULT false,
    bot_reason  text,
    CONSTRAINT analytics_events_type_check CHECK (event_type IN (
        'property_card_impression', 'listing_detail_view', 'contact_click', 'youtube_play_intent',
        'listing_save', 'enquiry_submit', 'viewing_request', 'appointment_confirm', 'listing_relist',
        'application_submit', 'booking_confirm', 'invoice_payment_confirmed')),
    CONSTRAINT analytics_events_origin_check CHECK (origin IN ('browser', 'server')),
    CONSTRAINT analytics_events_placement_check CHECK (placement IS NULL OR placement IN ('search', 'rail', 'similar', 'home', 'saved')),
    CONSTRAINT analytics_events_rank_check CHECK (rank IS NULL OR rank >= 0),
    CONSTRAINT analytics_events_source_check CHECK (source IS NULL OR source IN ('direct', 'search_engine', 'social', 'internal', 'email', 'other')),
    CONSTRAINT analytics_events_device_check CHECK (device IS NULL OR device IN ('mobile', 'tablet', 'desktop')),
    CONSTRAINT analytics_events_session_check CHECK (session_id IS NULL OR session_id ~ '^[A-Za-z0-9_-]{16,64}$'),
    -- Never a full address: IPv4 /24 or IPv6 /48 only.
    CONSTRAINT analytics_events_network_check CHECK (network IS NULL
        OR (family(network) = 4 AND masklen(network) = 24)
        OR (family(network) = 6 AND masklen(network) = 48)),
    CONSTRAINT analytics_events_bot_check CHECK ((bot AND bot_reason IN ('user_agent', 'rate')) OR (NOT bot AND bot_reason IS NULL)),
    -- Server-side events carry no session and no network.
    CONSTRAINT analytics_events_server_check CHECK (origin = 'browser' OR (session_id IS NULL AND network IS NULL AND NOT bot))
);

CREATE INDEX IF NOT EXISTS analytics_events_occurred_idx ON analytics_events (occurred_at);
CREATE INDEX IF NOT EXISTS analytics_events_market_day_idx ON analytics_events (market_code, local_day);
CREATE INDEX IF NOT EXISTS analytics_events_listing_day_idx ON analytics_events (listing_id, local_day);
-- The SPA de-duplication of detail views per (session, listing).
CREATE INDEX IF NOT EXISTS analytics_events_spa_idx ON analytics_events (session_id, listing_id, occurred_at)
    WHERE event_type = 'listing_detail_view' AND NOT bot AND session_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS analytics_late_events (
    event_id    uuid        PRIMARY KEY,
    event_type  text        NOT NULL,
    listing_id  uuid        NOT NULL,
    market_code text        NOT NULL,
    occurred_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    local_day   date        NOT NULL
);

CREATE INDEX IF NOT EXISTS analytics_late_events_received_idx ON analytics_late_events (received_at);
CREATE INDEX IF NOT EXISTS analytics_late_events_market_day_idx ON analytics_late_events (market_code, local_day);

CREATE TABLE IF NOT EXISTS analytics_listing_daily (
    listing_id             uuid        NOT NULL,
    day                    date        NOT NULL,
    market_code            text        NOT NULL,
    views                  integer     NOT NULL DEFAULT 0,
    unique_views           integer     NOT NULL DEFAULT 0,
    impressions            integer     NOT NULL DEFAULT 0,
    contact_clicks         integer     NOT NULL DEFAULT 0,
    video_plays            integer     NOT NULL DEFAULT 0,
    saves                  integer     NOT NULL DEFAULT 0,
    enquiries              integer     NOT NULL DEFAULT 0,
    viewing_requests       integer     NOT NULL DEFAULT 0,
    appointments_confirmed integer     NOT NULL DEFAULT 0,
    relists                integer     NOT NULL DEFAULT 0,
    filtered_bot_events    integer     NOT NULL DEFAULT 0,
    late_events_dropped    integer     NOT NULL DEFAULT 0,
    sources                jsonb       NOT NULL DEFAULT '{}'::jsonb,
    devices                jsonb       NOT NULL DEFAULT '{}'::jsonb,
    refreshed_at           timestamptz NOT NULL,
    PRIMARY KEY (listing_id, day)
);

CREATE INDEX IF NOT EXISTS analytics_listing_daily_market_day_idx ON analytics_listing_daily (market_code, day);

CREATE TABLE IF NOT EXISTS analytics_rollup_runs (
    id           smallint    PRIMARY KEY DEFAULT 1,
    refreshed_at timestamptz NOT NULL,
    CONSTRAINT analytics_rollup_runs_single CHECK (id = 1)
);
