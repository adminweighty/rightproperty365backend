-- Reverts 0005_business_enquiries. Destroys every business enquiry, its
-- timeline and activity records, and all directory profiles. Outbox events of
-- type BUSINESS_ENQUIRY_CREATED are removed too: their enquiries no longer exist.
DELETE FROM customer_activity WHERE business_enquiry_id IS NOT NULL;
DROP INDEX IF EXISTS customer_activity_business_enquiry_idx;
ALTER TABLE customer_activity DROP COLUMN IF EXISTS business_enquiry_id;

DELETE FROM outbox_events WHERE aggregate_type = 'business_enquiry';

DROP TABLE IF EXISTS business_enquiry_timeline;
DROP TABLE IF EXISTS business_enquiries;
DROP TABLE IF EXISTS directory_profiles;
