-- API 0.58.0: campaign attribution (FR-013). The campaign a visit came from
-- (utm_source, utm_medium, utm_campaign, cleaned by the API), stored on
-- property requests and raw analytics events. NULL is "no campaign" (none
-- given, or Do Not Track / Global Privacy Control).
ALTER TABLE property_requests
    ADD COLUMN utm_source   text CHECK (char_length(utm_source) BETWEEN 1 AND 100),
    ADD COLUMN utm_medium   text CHECK (char_length(utm_medium) BETWEEN 1 AND 100),
    ADD COLUMN utm_campaign text CHECK (char_length(utm_campaign) BETWEEN 1 AND 100);
ALTER TABLE analytics_events
    ADD COLUMN utm_source   text CHECK (char_length(utm_source) BETWEEN 1 AND 100),
    ADD COLUMN utm_medium   text CHECK (char_length(utm_medium) BETWEEN 1 AND 100),
    ADD COLUMN utm_campaign text CHECK (char_length(utm_campaign) BETWEEN 1 AND 100);
