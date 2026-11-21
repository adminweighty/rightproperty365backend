-- API 0.54.0: listing reports and their moderation (BR-018).
CREATE TABLE listing_reports (
    id               uuid PRIMARY KEY,
    listing_id       uuid        NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
    reason           text        NOT NULL CHECK (reason IN ('FRAUD', 'MISLEADING', 'ALREADY_SOLD_OR_LET', 'WRONG_PRICE', 'DUPLICATE', 'OFFENSIVE', 'OTHER')),
    note             text        CHECK (char_length(note) <= 1000),
    -- A signed-in reporter (NULL for a guest). No IP address or contact details are stored.
    reporter_user_id uuid        REFERENCES users (id) ON DELETE SET NULL,
    status           text        NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'DISMISSED', 'ACTIONED')),
    decided_by       uuid        REFERENCES users (id),
    decided_by_name  text,
    decided_at       timestamptz,
    decision_note    text        CHECK (char_length(decision_note) <= 2000),
    created_at       timestamptz NOT NULL,
    CONSTRAINT listing_reports_decided CHECK ((status = 'OPEN') = (decided_at IS NULL))
);
CREATE INDEX listing_reports_queue_idx ON listing_reports (status, created_at DESC);
CREATE INDEX listing_reports_listing_idx ON listing_reports (listing_id) WHERE status = 'OPEN';
-- One open report per signed-in reporter and listing.
CREATE UNIQUE INDEX listing_reports_open_per_reporter ON listing_reports (listing_id, reporter_user_id)
    WHERE status = 'OPEN' AND reporter_user_id IS NOT NULL;
