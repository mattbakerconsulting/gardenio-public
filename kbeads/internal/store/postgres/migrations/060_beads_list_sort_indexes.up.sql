-- Perf (gd-g25jihMHFs): index the list sort orders.
--
-- Every ListBeads page ends `ORDER BY beads.created_at DESC, beads.id` (the
-- default, parseSortClause) or sorts on updated_at (`-updated_at` is the
-- second most common sort among callers), always with the soft-delete
-- predicate `deleted_at IS NULL`. Nothing indexes either column, so a
-- 50-row page is a Seq Scan of every live bead plus a top-N heapsort: the
-- operator's EXPLAIN (ANALYZE) on 2026-10-15 put that floor at ~100 ms with
-- 20k beads. On 2026-10-29 the table holds 57k beads and an unfiltered
-- `GET /v1/beads?limit=50` takes 0.42-0.49 s at the API, of which only
-- 0.16-0.21 s is the narrow total COUNT (count_only), so the page itself
-- costs ~0.25-0.3 s and grows with the table.
--
-- With (col DESC, id) the planner walks the index from the newest bead and
-- stops after LIMIT matching rows: the DESC default and `-updated_at` match
-- the index order exactly, and the ascending sorts scan it with an
-- Incremental Sort on id. Partial on `deleted_at IS NULL`, the predicate every
-- default listing carries, so deleted rows stay out of the index.
--
-- Idempotent DDL only, not CONCURRENTLY (matches 052 and 055); the table is
-- small enough that the build lock is brief.

CREATE INDEX IF NOT EXISTS idx_beads_live_created_at_id
    ON beads (created_at DESC, id)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_beads_live_updated_at_id
    ON beads (updated_at DESC, id)
    WHERE deleted_at IS NULL;

ANALYZE beads;
