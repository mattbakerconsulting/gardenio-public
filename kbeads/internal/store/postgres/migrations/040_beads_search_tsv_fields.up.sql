-- ka-0000005839: extend the search_tsv FTS signal to cover a decision's
-- free-text FIELDS — the `context` string and the option label/short text —
-- so `gb decision list --search <ref>` matches a reference that appears ONLY
-- inside an option label or the context field, not just the prompt (bead
-- title) or description. Follow-up to migration 039 (ka-0000003832).
--
-- THE GAP (empirically confirmed 2026-07-11): a decision stores its prompt as
-- the bead TITLE (indexed by 039) but its `context` and `options` JSON live in
-- the bead FIELDS jsonb column, which 039's search_tsv did NOT index. A
-- decision whose only "kubectl" mention was in an option label did not match
-- `gb decision list --search kubectl`.
--
-- WHAT THIS MIGRATION DOES (DDL only; no eager backfill — see policy below):
--   1. Introduces an IMMUTABLE SQL function beads_search_tsv_expr(title,
--      description, fields) as the SINGLE source of truth for a bead's
--      search_tsv. It weights title 'A' > description 'B' > context 'C' >
--      option label/short 'D', so a title hit still ranks above a
--      description hit above an option-text hit — the same intent as 039's
--      A/B weighting, now extended to the fields text.
--   2. Rewrites the beads_search_tsv_update() trigger function to call that
--      single expression (kills the sync-drift risk between the trigger and
--      the Go-side backfill, which previously duplicated the vector SQL).
--   3. WIDENS the trigger to fire on `fields` changes too (was title,
--      description only) so editing a decision's options/context refreshes
--      its vector automatically.
--
-- WHY a shared IMMUTABLE function: 039 kept the tsvector SQL in TWO places —
-- the trigger fn (new/updated rows) and the Go `searchTSVExpr` const (legacy
-- backfill) — with a comment begging future edits to keep them in sync. This
-- migration collapses both onto beads_search_tsv_expr() so the trigger and the
-- backfill are provably identical by construction. IMMUTABLE is honest: the
-- result depends only on the three arguments (2-arg to_tsvector with an
-- explicit 'english' regconfig is immutable; jsonb_array_elements/string_agg
-- over the input arg are deterministic).
--
-- ROBUST to malformed option shapes: options are normally an array of objects
-- ({id, short, label, artifact_type}), but the expression guards every step —
-- a non-array `options` (or absent key) coerces to '[]' via a jsonb_typeof
-- CASE, and only object elements contribute label/short text
-- (jsonb_typeof(opt) = 'object'); bare-string / malformed elements are skipped
-- rather than erroring. A NULL fields column yields the title+description
-- vector unchanged.
--
-- Migration policy (kbeads/CLAUDE.md): idempotent DDL only (CREATE OR REPLACE
-- FUNCTION / DROP TRIGGER IF EXISTS + CREATE), and NO eager full-table
-- backfill inside the migration transaction. The trigger keeps new/updated
-- rows current from here on; the EXISTING rows that 039 already filled with a
-- title+description-only vector are refreshed separately, in batches, via
-- `kd admin backfill-search-tsv --reindex` (one UPDATE per keyset page, each
-- its own transaction) — never a table-locking pass. The GIN index
-- (idx_beads_search_tsv) and the column belong to 039 and are untouched here,
-- so this migration adds no index build.

-- 1. Single source of truth for the search_tsv vector: title(A) + description(B)
--    + fields->>'context' (C) + option label/short text (D). IMMUTABLE so it is
--    safe in the trigger and in the batched backfill UPDATE.
CREATE OR REPLACE FUNCTION beads_search_tsv_expr(
    p_title       text,
    p_description text,
    p_fields      jsonb
) RETURNS tsvector AS $$
    SELECT
        setweight(to_tsvector('english', COALESCE(p_title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(p_description, '')), 'B') ||
        setweight(to_tsvector('english', COALESCE(p_fields->>'context', '')), 'C') ||
        setweight(to_tsvector('english', COALESCE(
            (
                SELECT string_agg(
                           COALESCE(opt->>'label', '') || ' ' || COALESCE(opt->>'short', ''),
                           ' '
                       )
                FROM jsonb_array_elements(
                         CASE WHEN jsonb_typeof(p_fields->'options') = 'array'
                              THEN p_fields->'options'
                              ELSE '[]'::jsonb
                         END
                     ) AS opt
                WHERE jsonb_typeof(opt) = 'object'
            ),
            ''
        )), 'D')
$$ LANGUAGE sql IMMUTABLE;

-- 2. Trigger function now delegates to the single expression above. COALESCE /
--    typeof guards live inside the expression, so this is a one-liner.
CREATE OR REPLACE FUNCTION beads_search_tsv_update() RETURNS trigger AS $$
BEGIN
    NEW.search_tsv := beads_search_tsv_expr(NEW.title, NEW.description, NEW.fields);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- 3. Re-wire the trigger, widened to also fire on `fields` UPDATEs so option /
--    context edits refresh the vector. DROP-then-CREATE is the idempotent form
--    (CREATE TRIGGER has no IF NOT EXISTS before PG14; DROP IF EXISTS is safe).
DROP TRIGGER IF EXISTS beads_search_tsv_trigger ON beads;
CREATE TRIGGER beads_search_tsv_trigger
    BEFORE INSERT OR UPDATE OF title, description, fields ON beads
    FOR EACH ROW
    EXECUTE FUNCTION beads_search_tsv_update();
