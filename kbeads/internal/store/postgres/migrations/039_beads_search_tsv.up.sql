-- ka-search relevance Option 2 (task ka-0000003832, follow-up to the merged
-- Option-1 scoped fix ka-0000004745 / MR !4860).
--
-- Option-1 pushed multi-term tokenization + a title-tier relevance CASE into
-- the ILIKE WHERE/ORDER BY of queryListBeads (in-app, no index). Option-2
-- pushes a real Postgres full-text-search signal into the database: a stored
-- `search_tsv tsvector` column over title+description, a GIN index over it,
-- and a trigger that keeps it current on every write. queryListBeads then
-- folds a `ts_rank_cd` component into the relevance score and ORs an FTS
-- predicate into the match so the GIN index is load-bearing.
--
-- WHY a stored column + trigger (not a bare expression index):
--   The no-eager-backfill policy (kbeads/CLAUDE.md "Migration policy") forbids
--   a full-table backfill inside the migration transaction — on a 300K+ row
--   prod DB that stalls every read AND write for the migration window
--   (ka-0000006380). A stored generated/maintained column lets us:
--     (a) install the trigger here so EVERY new INSERT/UPDATE is current with
--         zero backfill, and
--     (b) fill legacy rows separately, in batches, via the lazy backfill
--         (`kd admin backfill-search-tsv` → POST /v1/admin/backfill-search-tsv
--         → store.BackfillSearchTSVPage), each batch its own transaction.
--   At migrate time the column is all-NULL for legacy rows, so the GIN index
--   below builds over an essentially-empty column and is cheap; it grows
--   incrementally as the batched backfill fills rows.
--
-- WHY this is correct during the backfill window:
--   queryListBeads keeps the Option-1 per-term ILIKE clauses as the recall
--   floor (they match regardless of search_tsv being NULL) and only ADDS the
--   FTS predicate/score. A legacy row with search_tsv IS NULL still matches
--   via ILIKE and simply contributes 0 to the ts_rank component (COALESCE),
--   so ranking degrades gracefully to Option-1 behavior until it is filled.
--
-- Title is weighted 'A' and description 'B' so ts_rank ranks a title hit above
-- a description-only hit — the same intent as the Option-1 title-tier CASE,
-- now expressed inside the FTS signal too. 'english' regconfig matches the
-- websearch_to_tsquery('english', …) used at query time.
--
-- Migration policy: idempotent DDL only (ADD COLUMN / CREATE FUNCTION OR
-- REPLACE / DROP TRIGGER IF EXISTS + CREATE / CREATE INDEX IF NOT EXISTS), no
-- eager backfill, runs via `kd migrate` (no daemon-startup deadline). Not
-- CONCURRENTLY — golang-migrate wraps each migration in a transaction and
-- CREATE INDEX CONCURRENTLY cannot run in one; the index is cheap here because
-- the column starts all-NULL (see above), matching the lock profile of the
-- partial expression indexes in migrations 037 / 038.

-- 1. The stored FTS column. Nullable: legacy rows stay NULL until the lazy
--    backfill fills them; new/updated rows are filled by the trigger below.
ALTER TABLE beads ADD COLUMN IF NOT EXISTS search_tsv tsvector;

-- 2. Trigger function: recompute search_tsv from title + description on every
--    INSERT/UPDATE. Weighted A (title) > B (description). COALESCE guards NULL
--    text. Kept in sync with store.searchTSVExpr (Go-side backfill) and the
--    websearch_to_tsquery('english', …) at query time — all three use the
--    'english' regconfig.
CREATE OR REPLACE FUNCTION beads_search_tsv_update() RETURNS trigger AS $$
BEGIN
    NEW.search_tsv :=
        setweight(to_tsvector('english', COALESCE(NEW.title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(NEW.description, '')), 'B');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- 3. Wire the trigger. DROP-then-CREATE is the idempotent form (CREATE TRIGGER
--    has no IF NOT EXISTS before PG14, and DROP IF EXISTS is universally safe).
DROP TRIGGER IF EXISTS beads_search_tsv_trigger ON beads;
CREATE TRIGGER beads_search_tsv_trigger
    BEFORE INSERT OR UPDATE OF title, description ON beads
    FOR EACH ROW
    EXECUTE FUNCTION beads_search_tsv_update();

-- 4. GIN index over the FTS column — the index queryListBeads' FTS predicate
--    (search_tsv @@ websearch_to_tsquery(...)) hits. Cheap to build now (column
--    is all-NULL for legacy rows); grows as the batched backfill fills rows.
CREATE INDEX IF NOT EXISTS idx_beads_search_tsv
    ON beads USING GIN (search_tsv);

ANALYZE beads;
