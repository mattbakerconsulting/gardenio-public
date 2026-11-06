-- Captain Page (epic ka-0000001579, task ka-0000000412): index
-- beads.fields->>'schedule_id' so "all iterations of captain X" is an
-- O(log n) indexed lookup, not the sequential scan that the daemon's
-- fields->>'schedule_id' = $1 equality filter degrades to today.
--
-- Every agent bead a captain spawns carries schedule_id = <captain trigger
-- bead ID> (stamped by stampCaptainLoopContext in
-- gardenio/controller/internal/bridge/triggers.go). The beads-viewer Captain
-- Page lists a captain's whole life-thread via
-- ListBeadsFiltered{Types:["agent"], Fields:{"schedule_id": captainID}},
-- which the daemon serves as fields->>'schedule_id' = $1 — a single-key
-- equality, the textbook B-tree fit.
--
-- Mirrors migration 025_fields_hot_indexes: a PARTIAL B-tree expression
-- index (WHERE fields ? 'schedule_id') keeps the index small — only
-- scheduler/captain-spawned beads carry the key. No GIN: single-key
-- equality is served smaller and faster by a B-tree (same rationale as 025).
--
-- Migration policy: idempotent DDL only (CREATE INDEX IF NOT EXISTS), no
-- table backfill, runs via `kd migrate` (no daemon-startup deadline).

CREATE INDEX IF NOT EXISTS idx_beads_fields_schedule_id
    ON beads ((fields->>'schedule_id'))
    WHERE fields ? 'schedule_id';
