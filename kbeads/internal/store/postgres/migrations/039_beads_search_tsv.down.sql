-- Reverse of 039_beads_search_tsv.up.sql. Drop in reverse order: index, then
-- trigger, then trigger function, then the column. Each is IF EXISTS so the
-- down is idempotent and a partially-applied up recovers cleanly.

DROP INDEX IF EXISTS idx_beads_search_tsv;

DROP TRIGGER IF EXISTS beads_search_tsv_trigger ON beads;

DROP FUNCTION IF EXISTS beads_search_tsv_update();

ALTER TABLE beads DROP COLUMN IF EXISTS search_tsv;
