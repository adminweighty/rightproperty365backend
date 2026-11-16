-- 0025: lease templates owned by organisations, and generated stamps (API 0.26.0).
--
--   * lease_templates.organisation_id: the organisation that owns a template
--     (agencies and landlords design their own). NULL is a sample template
--     of the platform library, which anyone may copy and use. Approving no
--     longer needs a legal review: legal_review stays, optional.
--   * agreement_fields.type gains STAMP: a place for a signer's generated
--     stamp (a rubber-stamp style image the server draws from the text the
--     signer typed; not a certificate seal).
--   * agreement_recipients.stamp: the stamp a signer applied
--     ({shape, colour, top_text, centre_lines, bottom_text, include_date}).
--   * user_signatures.stamp: a signed-in user's saved stamp.
--
-- Safe on a database with data: new nullable columns, a relaxed check and a
-- widened check.

ALTER TABLE lease_templates ADD COLUMN IF NOT EXISTS organisation_id uuid REFERENCES organisations (id) ON DELETE RESTRICT;
CREATE INDEX IF NOT EXISTS lease_templates_organisation_idx ON lease_templates (organisation_id, status) WHERE organisation_id IS NOT NULL;

ALTER TABLE lease_templates DROP CONSTRAINT IF EXISTS lease_templates_review_check;
ALTER TABLE lease_templates ADD CONSTRAINT lease_templates_review_check CHECK (status = 'DRAFT' OR approved_at IS NOT NULL);

ALTER TABLE agreement_fields DROP CONSTRAINT IF EXISTS agreement_fields_type_check;
ALTER TABLE agreement_fields ADD CONSTRAINT agreement_fields_type_check
    CHECK (type IN ('SIGNATURE', 'INITIALS', 'DATE_SIGNED', 'NAME', 'TEXT', 'CHECKBOX', 'STAMP'));

ALTER TABLE agreement_recipients ADD COLUMN IF NOT EXISTS stamp jsonb;
ALTER TABLE user_signatures ADD COLUMN IF NOT EXISTS stamp jsonb;
