-- API 0.53.0: featured and sponsored placements (FR-046, FR-047, Handover section 8).
CREATE TABLE listing_placements (
    id              uuid PRIMARY KEY,
    listing_id      uuid        NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
    kind            text        NOT NULL CHECK (kind IN ('FEATURED', 'SPONSORED')),
    starts_at       timestamptz NOT NULL,
    ends_at         timestamptz NOT NULL,
    -- Checked by the service (ACTIVE, the advertiser's); no foreign key, so the
    -- placement history outlives the subscription.
    subscription_id uuid,
    note            text        CHECK (char_length(note) <= 500),
    granted_by      uuid        NOT NULL REFERENCES users (id),
    granted_by_name text        NOT NULL,
    granted_at      timestamptz NOT NULL,
    ended_at        timestamptz,
    ended_by        uuid        REFERENCES users (id),
    end_reason      text        CHECK (char_length(end_reason) <= 500),
    CONSTRAINT listing_placements_period CHECK (ends_at > starts_at),
    CONSTRAINT listing_placements_ended CHECK ((ended_at IS NULL) = (ended_by IS NULL))
);
CREATE INDEX listing_placements_listing_idx ON listing_placements (listing_id, starts_at);
CREATE INDEX listing_placements_subscription_idx ON listing_placements (subscription_id) WHERE subscription_id IS NOT NULL;

-- The kind of the active placement, kept in step by the placement sync (with featured).
ALTER TABLE listings ADD COLUMN placement_kind text CHECK (placement_kind IN ('FEATURED', 'SPONSORED'));

-- Placements are audited against their listing.
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW', 'LISTING_PACKAGE', 'ORGANISATION',
                           'LEGAL_DOCUMENT_VERSION', 'TAX_RULE', 'DEVELOPMENT', 'INVOICE', 'LISTING'));
