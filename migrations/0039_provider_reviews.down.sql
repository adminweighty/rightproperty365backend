-- Reverts 0039_provider_reviews.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every provider review, its reply and its reports, the
-- administrator audit records of review moderation, and which enquiries
-- were marked as replied to.
--
-- The admin audit target CHECK keeps the name admin_audit_events_target_type_allowed
-- (the 0024 values; a named constraint never takes the <table>_<column>_check form).

DELETE FROM admin_audit_events WHERE target_type = 'PROVIDER_REVIEW';
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM'));

DROP TABLE IF EXISTS provider_review_reports;
DROP TABLE IF EXISTS provider_reviews;

DROP INDEX IF EXISTS business_enquiries_customer_profile_idx;
ALTER TABLE business_enquiries DROP CONSTRAINT IF EXISTS business_enquiries_replied_after_created;
ALTER TABLE business_enquiries DROP COLUMN IF EXISTS replied_by;
ALTER TABLE business_enquiries DROP COLUMN IF EXISTS replied_at;
DELETE FROM business_enquiry_timeline WHERE event_type = 'REPLIED';
