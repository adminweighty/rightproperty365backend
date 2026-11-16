-- Reverts 0025_templates_and_stamps.
--
-- The API release that needs this migration must be rolled back first.
-- LOSES: every saved and applied stamp, the STAMP fields of agreements (a
-- completed document keeps the stamp drawn on its pages), and which
-- organisation owns a template. Organisation templates are RETIRED first so
-- that they are never offered as platform samples afterwards; leases made
-- from them keep them. The legal-review check comes back NOT VALID: it holds
-- for new rows, and the templates approved without a review stay as they are.

UPDATE lease_templates SET status = 'RETIRED', retired_at = COALESCE(retired_at, now()), updated_at = now()
    WHERE organisation_id IS NOT NULL AND status <> 'RETIRED';

ALTER TABLE user_signatures DROP COLUMN IF EXISTS stamp;
ALTER TABLE agreement_recipients DROP COLUMN IF EXISTS stamp;

DELETE FROM agreement_fields WHERE type = 'STAMP';
ALTER TABLE agreement_fields DROP CONSTRAINT IF EXISTS agreement_fields_type_check;
ALTER TABLE agreement_fields ADD CONSTRAINT agreement_fields_type_check
    CHECK (type IN ('SIGNATURE', 'INITIALS', 'DATE_SIGNED', 'NAME', 'TEXT', 'CHECKBOX'));

ALTER TABLE lease_templates DROP CONSTRAINT IF EXISTS lease_templates_review_check;
ALTER TABLE lease_templates ADD CONSTRAINT lease_templates_review_check
    CHECK (status = 'DRAFT' OR (legal_review IS NOT NULL AND approved_at IS NOT NULL)) NOT VALID;

DROP INDEX IF EXISTS lease_templates_organisation_idx;
ALTER TABLE lease_templates DROP COLUMN IF EXISTS organisation_id;
