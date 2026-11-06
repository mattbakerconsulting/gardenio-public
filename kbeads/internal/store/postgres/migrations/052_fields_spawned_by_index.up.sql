-- Perf (gd-g25jihMHFs): index the spawned_by lineage lookup.
--
-- The controller's gardener spend sync (cmd/controller/gardener_spend_sync.go)
-- walks each gardener's crew breadth-first, issuing one
-- `type IN ('agent') AND fields->>'spawned_by' = $name` list per agent name.
-- Without an index each lookup is a full sequential scan of beads. Measured
-- 2026-10-15 on 2026.288.7 with pg_stat_statements: ~6.5 calls/s at ~280 ms
-- mean (~27K buffer hits per call, 1 row returned), which alone kept
-- gardenio-postgres-0 at ~1.8 of its 2 cores and slowed every other query.
--
-- Partial B-tree expression index, same shape and rationale as 031
-- (schedule_id), 034 (trigger_id) and 037 (thread_id): single-key equality on
-- a sparse JSON key. Idempotent DDL only, not CONCURRENTLY (matches 037).

CREATE INDEX IF NOT EXISTS idx_beads_fields_spawned_by
    ON beads ((fields->>'spawned_by'))
    WHERE fields ? 'spawned_by';

ANALYZE beads;
