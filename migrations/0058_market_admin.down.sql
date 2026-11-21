-- The audit constraint keeps 'MARKET': audit rows are never deleted.
ALTER TABLE markets DROP CONSTRAINT IF EXISTS markets_enabled_follows_status;
ALTER TABLE markets DROP COLUMN IF EXISTS notice, DROP COLUMN IF EXISTS default_package_code, DROP COLUMN IF EXISTS extra_currencies;
