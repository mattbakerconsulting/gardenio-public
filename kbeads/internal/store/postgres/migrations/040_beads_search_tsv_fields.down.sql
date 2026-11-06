-- Reverse of 040_beads_search_tsv_fields.up.sql: restore migration 039's
-- title+description-only search_tsv. Restore the trigger function and the
-- trigger to their 039 form, then drop the shared expression function. Each
-- statement is idempotent so a partially-applied up recovers cleanly.
--
-- NOTE: this does NOT re-narrow the already-widened vectors on rows that were
-- reindexed while 040 was live — those rows simply carry EXTRA (context/option)
-- lexemes until their next write re-fires the (now 039-form) trigger. That is
-- harmless: recall is a strict superset and the 039 query path is unchanged, so
-- a down is safe without a table-locking re-backfill (no-eager-backfill policy).

-- 1. Restore the 039 trigger function (title 'A' + description 'B' only).
CREATE OR REPLACE FUNCTION beads_search_tsv_update() RETURNS trigger AS $$
BEGIN
    NEW.search_tsv :=
        setweight(to_tsvector('english', COALESCE(NEW.title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(NEW.description, '')), 'B');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- 2. Restore the 039 trigger (fires on title, description only — not fields).
DROP TRIGGER IF EXISTS beads_search_tsv_trigger ON beads;
CREATE TRIGGER beads_search_tsv_trigger
    BEFORE INSERT OR UPDATE OF title, description ON beads
    FOR EACH ROW
    EXECUTE FUNCTION beads_search_tsv_update();

-- 3. Drop the shared expression function 040 introduced.
DROP FUNCTION IF EXISTS beads_search_tsv_expr(text, text, jsonb);
