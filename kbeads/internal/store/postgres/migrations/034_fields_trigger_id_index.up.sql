-- Bug ka-0000002374: /beads/captains 504 gateway timeout.
--
-- The beads-viewer Captain index (/captains) repairs the "Runs" column for
-- legacy / zero-tally captains (bug ka-0000005661) by deriving each captain's
-- spawned/completed run count from its iteration agent beads, unioning the dual
-- ownership keys an iteration can carry — trigger_id (modern bridge path) OR
-- schedule_id (legacy scheduler path). See
-- gardenio/controller/cmd/beads-viewer/captains_enrich.go: deriveCaptainRunCount
-- issues count_only queries `fields->>'trigger_id' = $1` and
-- `fields->>'schedule_id' = $1` per zero-tally captain.
--
-- Migration 031 indexed fields->>'schedule_id' but trigger_id was left on a
-- sequential scan. As the captain fleet grew (the enrich code was written for
-- ~21 zero-tally captains; the live fleet hit ~75), each page load issued ~75
-- captains × several trigger_id count_only queries, every one a full scan of
-- the beads table. The aggregate sequential-scan cost blew past the ingress
-- gateway timeout → 504 (viewer logs: "context canceled" on the count_only
-- requests, "broken pipe" on the aborted template render).
--
-- Every modern bridge-spawned captain iteration carries trigger_id = <captain
-- trigger bead ID> (stampCaptainLoopContext / extraFields in
-- gardenio/controller/internal/bridge/triggers.go), so a single-key equality
-- lookup is the textbook B-tree fit — mirroring migration 031 for schedule_id.
--
-- PARTIAL B-tree expression index (WHERE fields ? 'trigger_id') keeps the index
-- small — only trigger-spawned beads carry the key. No GIN: single-key equality
-- is served smaller and faster by a B-tree (same rationale as 025 / 031).
--
-- Migration policy: idempotent DDL only (CREATE INDEX IF NOT EXISTS), no table
-- backfill, runs via `kd migrate` (no daemon-startup deadline).

CREATE INDEX IF NOT EXISTS idx_beads_fields_trigger_id
    ON beads ((fields->>'trigger_id'))
    WHERE fields ? 'trigger_id';
