-- 0026: saved listings (API 0.27.0, FR-016 "Saved").
--
--   * saved_listings: one row per (customer, listing) the customer saved.
--     saved_at is the first save (saving again changes nothing). snapshot is
--     the listing's card ({slug, reference, title, category, price,
--     price_period, city, country, photo}) as it was last seen public, at
--     snapshot_at: a listing that expires, is let, sold or withdrawn is still
--     shown to the customer from it (without a link).
--   * user_id references users ON DELETE CASCADE. Account deletion
--     anonymises the users row in place, so it also deletes the rows itself
--     (internal/modules/accounts); the cascade covers a hard delete.
--   * listing_id references listings WITHOUT a cascade: the save is kept as
--     history. Only a listing that was never published can be deleted, and
--     such a listing can never have been saved.
--
-- Safe on a database with data: a new table only.

CREATE TABLE IF NOT EXISTS saved_listings (
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    listing_id  uuid        NOT NULL REFERENCES listings (id),
    saved_at    timestamptz NOT NULL,
    snapshot    jsonb       NOT NULL,
    snapshot_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, listing_id),
    CONSTRAINT saved_listings_snapshot_check CHECK (jsonb_typeof(snapshot) = 'object')
);

CREATE INDEX IF NOT EXISTS saved_listings_user_saved_idx ON saved_listings (user_id, saved_at DESC);
