-- idx_beads_open_decisions_id: partial index over the open-decision set
-- keyed on id, used by the blocked_on_decision_bead_id projection's
-- MIN(d.id) lookup (queries.go:104). Without it the planner picks
-- beads_pkey to walk ids in ascending order, then filters by
-- (type='decision' AND status='open' AND deleted_at IS NULL AND id IN
-- hashed-subplan), which on the gardenio-e2e dataset rejected ~315k rows
-- per outer loop. Switching to this partial index is an Index Only Scan
-- over ~5k rows; on the live e2e DB the per-loop buffer hits dropped
-- from 318,552 to 1, taking the 50-row sample plan from 15.9M shared
-- buffers to 50 (≈300,000× reduction on this subplan).
--
-- Migration 032 (idx_deps_depends_on_id_type) covers the EXISTS subqueries
-- that walk deps; this one covers the outer MIN(d.id) over decisions.
-- Both are needed for the projection to run cheaply when an HTTP caller
-- has opted in via with_blocked_on_decision=true.
--
-- Idempotent so the matching CREATE INDEX CONCURRENTLY hand-applied to
-- the gardenio-e2e RDS is a no-op when this lands.
CREATE INDEX IF NOT EXISTS idx_beads_open_decisions_id
    ON beads (id)
    WHERE type = 'decision' AND status = 'open' AND deleted_at IS NULL;
