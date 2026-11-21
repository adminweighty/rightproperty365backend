-- The organisation audit constraints keep the branch actions on the way down.
ALTER TABLE ledger_entries DROP COLUMN IF EXISTS branch_id;
ALTER TABLE listings DROP COLUMN IF EXISTS branch_id;
ALTER TABLE memberships DROP COLUMN IF EXISTS branch_id;
DROP TABLE IF EXISTS organisation_branches;
