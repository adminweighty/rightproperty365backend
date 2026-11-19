-- 0039: reviews of service providers (API 0.36.4, BR-019, FR-047, Phase 2 part e).
--
--   * business_enquiries gains replied_at / replied_by: the recipient marks
--     an enquiry as replied to (POST /v1/inbox/business-enquiries/{id}/replied).
--     A replied enquiry at least 14 days old is a review basis for its
--     customer.
--   * provider_reviews: one review of a public service-provider directory
--     profile, on one basis (a RESOLVED or CLOSED maintenance request of the
--     reviewer's book assigned to the provider, or the reviewer's replied
--     enquiry), unique per basis. The reviewer is never anonymous to the
--     platform: reviewer_user_id (and, for an organisation, its id) is
--     stored; reviewer_name is the public name (first name and last
--     initial, or the organisation's name). Status PENDING -> PUBLISHED /
--     REJECTED, PUBLISHED <-> HIDDEN, moderated by CONTENT_ADMIN /
--     MODERATOR / SUPER_ADMIN. The provider's one reply is published
--     directly (reportable; a moderator may remove it). `version` counts
--     the writes (optimistic locking).
--   * provider_review_reports: reports of published reviews, at most one
--     OPEN report per reporter and review.
--   * admin_audit_events gains the target PROVIDER_REVIEW. Its target CHECK
--     is re-created as admin_audit_events_target_type_allowed (the old
--     constraint, admin_audit_events_target_type_check, is dropped: a named
--     constraint never takes the <table>_<column>_check form).
--
-- Constraint names: inline CHECKs get Postgres's <table>_<column>_check;
-- the table-level constraints below are named differently on purpose.
--
-- User references are ON DELETE SET NULL; users are anonymised on account
-- deletion, never deleted, so the accounts module unlinks them explicitly
-- (a customer's review then shows "Former customer").
--
-- Safe on a database with data: new tables, nullable columns and a wider
-- check.

ALTER TABLE business_enquiries
    ADD COLUMN IF NOT EXISTS replied_at timestamptz,
    ADD COLUMN IF NOT EXISTS replied_by uuid REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE business_enquiries DROP CONSTRAINT IF EXISTS business_enquiries_replied_after_created;
ALTER TABLE business_enquiries ADD CONSTRAINT business_enquiries_replied_after_created
    CHECK (replied_at IS NULL OR replied_at >= created_at);
CREATE INDEX IF NOT EXISTS business_enquiries_customer_profile_idx ON business_enquiries (customer_user_id, profile_id)
    WHERE customer_user_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS provider_reviews (
    id                       uuid PRIMARY KEY,
    profile_id               uuid NOT NULL REFERENCES directory_profiles (id) ON DELETE RESTRICT,
    -- The profile as it was reviewed (the provider's list and the
    -- reviewer's own list read these).
    provider_organisation_id uuid NOT NULL REFERENCES organisations (id) ON DELETE RESTRICT,
    provider_kind            text NOT NULL,
    provider_slug            text NOT NULL,
    provider_name            text NOT NULL CHECK (char_length(provider_name) BETWEEN 1 AND 200),
    reviewer_kind            text NOT NULL CHECK (reviewer_kind IN ('CUSTOMER', 'ORGANISATION', 'DIRECT_OWNER')),
    reviewer_user_id         uuid REFERENCES users (id) ON DELETE SET NULL,
    reviewer_organisation_id uuid REFERENCES organisations (id) ON DELETE RESTRICT,
    reviewer_name            text NOT NULL CHECK (char_length(reviewer_name) BETWEEN 1 AND 200),
    -- The member who wrote an organisation's review (private).
    author_name              text NOT NULL DEFAULT '' CHECK (char_length(author_name) <= 200),
    basis_kind               text NOT NULL CHECK (basis_kind IN ('MAINTENANCE', 'ENQUIRY')),
    basis_id                 uuid NOT NULL,
    basis_reference          text NOT NULL DEFAULT '' CHECK (char_length(basis_reference) <= 40),
    rating                   smallint NOT NULL CHECK (rating BETWEEN 1 AND 5),
    body                     text NOT NULL CHECK (char_length(body) BETWEEN 20 AND 2000),
    status                   text NOT NULL CHECK (status IN ('PENDING', 'PUBLISHED', 'HIDDEN', 'REJECTED')),
    moderation_reason        text NOT NULL DEFAULT '' CHECK (char_length(moderation_reason) <= 500),
    moderated_at             timestamptz,
    moderated_by             uuid REFERENCES users (id) ON DELETE SET NULL,
    moderated_by_name        text NOT NULL DEFAULT '',
    published_at             timestamptz,
    reply_body               text CHECK (reply_body IS NULL OR char_length(reply_body) BETWEEN 1 AND 2000),
    reply_at                 timestamptz,
    reply_updated_at         timestamptz,
    reply_by                 uuid REFERENCES users (id) ON DELETE SET NULL,
    reply_by_name            text NOT NULL DEFAULT '',
    reply_removed_at         timestamptz,
    reply_removed_reason     text NOT NULL DEFAULT '' CHECK (char_length(reply_removed_reason) <= 500),
    version                  integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at               timestamptz NOT NULL,
    updated_at               timestamptz NOT NULL,
    CONSTRAINT provider_reviews_one_per_basis UNIQUE (basis_kind, basis_id),
    CONSTRAINT provider_reviews_reviewer_shape CHECK ((reviewer_kind = 'ORGANISATION') = (reviewer_organisation_id IS NOT NULL)),
    CONSTRAINT provider_reviews_reply_shape CHECK ((reply_body IS NULL) = (reply_at IS NULL)),
    CONSTRAINT provider_reviews_published_shape CHECK (status <> 'PUBLISHED' OR published_at IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS provider_reviews_public_idx ON provider_reviews (profile_id, published_at DESC, id DESC)
    WHERE status = 'PUBLISHED';
CREATE INDEX IF NOT EXISTS provider_reviews_provider_idx ON provider_reviews (provider_organisation_id, created_at DESC);
CREATE INDEX IF NOT EXISTS provider_reviews_reviewer_user_idx ON provider_reviews (reviewer_user_id, created_at DESC)
    WHERE reviewer_user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS provider_reviews_reviewer_org_idx ON provider_reviews (reviewer_organisation_id, created_at DESC)
    WHERE reviewer_organisation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS provider_reviews_pending_idx ON provider_reviews (created_at) WHERE status = 'PENDING';

CREATE TABLE IF NOT EXISTS provider_review_reports (
    id               uuid PRIMARY KEY,
    review_id        uuid NOT NULL REFERENCES provider_reviews (id) ON DELETE CASCADE,
    reporter_user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    reason           text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500),
    status           text NOT NULL CHECK (status IN ('OPEN', 'RESOLVED')),
    created_at       timestamptz NOT NULL,
    resolved_at      timestamptz,
    resolved_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    resolution       text NOT NULL DEFAULT '' CHECK (char_length(resolution) <= 40),
    CONSTRAINT provider_review_reports_resolved_shape CHECK ((status = 'RESOLVED') = (resolved_at IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS provider_review_reports_one_open ON provider_review_reports (review_id, reporter_user_id)
    WHERE status = 'OPEN';
CREATE INDEX IF NOT EXISTS provider_review_reports_open_idx ON provider_review_reports (review_id) WHERE status = 'OPEN';

ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_check;
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW'));
