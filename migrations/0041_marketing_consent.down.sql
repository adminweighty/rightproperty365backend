-- Reverts 0041: drops the marketing consent records, the consent wording and their triggers.
-- users.marketing_opt_in (migration 0002) keeps its values.

DROP TABLE IF EXISTS marketing_consent_events;
DROP FUNCTION IF EXISTS marketing_consent_events_refuse_update();
DROP TABLE IF EXISTS marketing_consent_texts;
DROP FUNCTION IF EXISTS marketing_consent_texts_refuse_change();
