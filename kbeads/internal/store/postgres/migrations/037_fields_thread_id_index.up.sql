-- Threads/wishes initiative (epic ka-0000001399, task 1a-2): thread membership
-- / roll-up query on fields->>'thread_id'.
--
-- Thread beads stamp a thread_id onto every bead spawned within a Slack thread
-- (thread membership). The membership / roll-up query that materializes a
-- thread's constellation — "give me every bead in thread T" and the per-thread
-- roll-up counts derived from it — issues equality lookups of the form
-- `fields->>'thread_id' = $1`, one per rendered thread.
--
-- Without an index this is a full sequential scan of the beads table per query
-- — the exact 504-gateway-timeout failure mode migration 034 fixed for the
-- captain-index trigger_id lookups (bug ka-0000002374): as the bead table grows
-- the aggregate per-thread seq-scan cost blows past the ingress gateway
-- timeout. Every thread membership / roll-up lookup is a single-key equality on
-- a sparse JSON key, so a partial B-tree expression index is the textbook fit —
-- mirroring migration 034 for trigger_id and 031 for schedule_id.
--
-- PARTIAL B-tree expression index (WHERE fields ? 'thread_id') keeps the index
-- small — only thread-spawned beads carry the key. No GIN: single-key equality
-- is served smaller and faster by a B-tree (same rationale as 025 / 031 / 034).
--
-- Migration policy: idempotent DDL only (CREATE INDEX IF NOT EXISTS), no table
-- backfill, runs via `kd migrate` (no daemon-startup deadline). Not CONCURRENTLY
-- — matches migration 034.

CREATE INDEX IF NOT EXISTS idx_beads_fields_thread_id
    ON beads ((fields->>'thread_id'))
    WHERE fields ? 'thread_id';

ANALYZE beads;
