-- Reverts 0040: drops the listing packages, their prices and assignments.
-- The admin audit target CHECK keeps the name admin_audit_events_target_type_allowed
-- (the 0039 values; a named constraint never takes the <table>_<column>_check form).

DELETE FROM admin_audit_events WHERE target_type IN ('LISTING_PACKAGE', 'ORGANISATION');
ALTER TABLE admin_audit_events DROP CONSTRAINT IF EXISTS admin_audit_events_target_type_allowed;
ALTER TABLE admin_audit_events ADD CONSTRAINT admin_audit_events_target_type_allowed
    CHECK (target_type IN ('USER', 'SUBSCRIPTION_PLAN', 'SUBSCRIPTION', 'REFERENCE_ITEM', 'PROVIDER_REVIEW'));

DROP TABLE IF EXISTS listing_package_assignments;
DROP TABLE IF EXISTS listing_package_prices;
DROP TABLE IF EXISTS listing_packages;
