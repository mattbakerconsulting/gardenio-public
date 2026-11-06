-- Task ka-0000003620 (Phase 2 of design ka-0000004060, operator-approved
-- decision ka-0000006300 option 'full'). Fixes ka-0000003621 (kd search
-- intermittent HTTP 500) and ka-0000006261 (BeadsDaemonDown false page).
--
-- ROOT CAUSE. queryListBeads' text-search WHERE is
--   (per-term (title/description ILIKE '%term%' OR comment-EXISTS) AND'd)
--   OR (search_tsv @@ websearch_to_tsquery('english', <query>))
-- The leading-wildcard ILIKE floor is non-sargable, so OR'd with the FTS
-- predicate the planner cannot use idx_beads_search_tsv for the combined match
-- and full-seq-scans the 695k-row prod beads table (measured 57.6s cold). At
-- the daemon's 30s request timeout Postgres cancels the statement (pq 57014)
-- and the handler returns a bare HTTP 500. The SAME seq scan on the /metrics
-- scrape path is what page-fires BeadsDaemonDown on a healthy daemon.
--
-- THE FIX HAS TWO HALVES that MUST both land, in order (design §"Phase 2"):
--   1. This migration: pg_trgm + GIN trigram indexes making the leading-
--      wildcard ILIKE branch sargable — on beads.title, beads.description AND
--      comments.text (comment recall is KEPT, per decision ka-0000006300 which
--      rejected the 'no-comment-index' option).
--   2. The queryListBeads SQL change (same MR) that collapses the per-term
--      correlated comment EXISTS into an uncorrelated `beads.id = ANY(ARRAY(
--      SELECT bead_id FROM comments WHERE text ILIKE ...))` so the whole
--      disjunction planner-collapses to a single BitmapOr of index scans
--      instead of a seq scan.
--   The trgm indexes alone are NOT sufficient: with the OLD correlated-EXISTS
--   shape the planner still seq-scans beads even with the indexes present
--   (verified — the comment subplans become bitmap scans but the top-level
--   Seq Scan on beads and its 5M-cost estimate remain). Only the query rewrite
--   turns the four disjuncts (title_trgm, description_trgm, search_tsv, and the
--   comment-id array via beads_pkey) into a BitmapOr. Measured on a synthetic
--   200k-bead / 180k-comment corpus at prod-like term selectivity: the 3-term
--   query drops from a 5,078,783-cost Seq Scan (425 ms) to a 437-cost BitmapOr
--   (16 ms), with byte-identical recall (0 rows diverge either direction).
--
-- CONCURRENTLY / PROD APPLY (this is the load-bearing operational note).
-- golang-migrate wraps each migration in a single transaction, and
-- `CREATE INDEX CONCURRENTLY` cannot run inside a transaction. So on prod the
-- three trgm index builds (large: ~1.4 GB beads + 149 MB comments) are
-- HAND-APPLIED off-peak with CREATE INDEX CONCURRENTLY, ONE AT A TIME, by an
-- operator/DBA BEFORE this migration runs — exactly the pattern of migrations
-- 032 / 033 / 041 / 042, whose supporting indexes were built CONCURRENTLY on
-- the live RDS and whose migration bodies are the idempotent
-- `CREATE INDEX IF NOT EXISTS` no-op that records the same object in
-- schema_migrations. The inline (non-CONCURRENTLY) `CREATE INDEX IF NOT EXISTS`
-- statements below are therefore a NO-OP on prod (the CONCURRENTLY-built index
-- of the same name already exists) and a correct-but-locking eager build only
-- on a fresh/small store (CI, gasbox, a new fleet) where the lock window is
-- immaterial. See the paired escalation for the exact off-peak apply script.
--
-- CREATE EXTENSION pg_trgm needs a privileged role on RDS. If the daemon's DB
-- role lacks it, migrate_preflight.go fails early with a specific message and
-- an operator must `CREATE EXTENSION pg_trgm` once (escalation-tracked). It is
-- idempotent (IF NOT EXISTS) and a no-op once installed.
--
-- Migration policy (kbeads/CLAUDE.md): idempotent DDL only (CREATE EXTENSION /
-- CREATE INDEX with IF NOT EXISTS), no eager full-table backfill of a data
-- column. Runs via `kd migrate` in a pre-upgrade Job (no startup-probe
-- deadline).

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- GIN trigram index on beads.title — makes `title ILIKE '%term%'` sargable.
CREATE INDEX IF NOT EXISTS idx_beads_title_trgm
    ON beads USING gin (title gin_trgm_ops);

-- GIN trigram index on beads.description — makes `description ILIKE '%term%'`
-- sargable.
CREATE INDEX IF NOT EXISTS idx_beads_description_trgm
    ON beads USING gin (description gin_trgm_ops);

-- GIN trigram index on comments.text — makes the comment-body recall branch
-- (ka-0000008243) an index scan instead of a per-term seq scan over the 183k
-- comments. KEEPS comment recall (decision ka-0000006300 rejected gating it
-- behind a flag). This is the dominant cost the design measured (~9-14s per
-- term, ~50s of the 57s total).
CREATE INDEX IF NOT EXISTS idx_comments_text_trgm
    ON comments USING gin (text gin_trgm_ops);

ANALYZE beads;
ANALYZE comments;
