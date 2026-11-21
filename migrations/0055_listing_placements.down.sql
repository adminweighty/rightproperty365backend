-- The audit constraint keeps 'LISTING': audit rows of placements are never deleted.
ALTER TABLE listings DROP COLUMN IF EXISTS placement_kind;
DROP TABLE IF EXISTS listing_placements;
