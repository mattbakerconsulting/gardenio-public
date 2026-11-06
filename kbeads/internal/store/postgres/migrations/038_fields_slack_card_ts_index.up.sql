-- Threads/wishes initiative (epic ka-0000001139, task 1b-13 ka-0000000693):
-- robust card-react close on fields->>'slack_card_ts'.
--
-- The 'done'-reaction-on-the-thread-card close gesture (1b-11) resolves the
-- reacted card → wish HEAD bead. To make that resolution robust across bridge
-- restart / agentCards cache eviction — and independent of the agent's
-- done/failed terminal state — the bridge now stamps slack_card_ts onto the
-- wish HEAD at card-post time (persistHeadCardRef). The close handler then
-- resolves card_ts → head with a direct daemon field-query of the form
-- `fields->>'slack_card_ts' = $1`, one per reaction.
--
-- Without an index this is a full sequential scan of the beads table per
-- reaction — the exact 504-gateway-timeout failure mode migration 034 fixed
-- for trigger_id lookups (bug ka-0000002374): as the bead table grows the
-- per-lookup seq-scan cost blows past the ingress gateway timeout. The lookup
-- is a single-key equality on a sparse JSON key, so a partial B-tree
-- expression index is the textbook fit — mirroring migration 037 for
-- thread_id, 034 for trigger_id, and 031 for schedule_id.
--
-- PARTIAL B-tree expression index (WHERE fields ? 'slack_card_ts') keeps the
-- index small — only card-bearing beads (agent beads + wish heads) carry the
-- key. No GIN: single-key equality is served smaller and faster by a B-tree
-- (same rationale as 025 / 031 / 034 / 037).
--
-- Migration policy: idempotent DDL only (CREATE INDEX IF NOT EXISTS), no table
-- backfill, runs via `kd migrate` (no daemon-startup deadline). Not
-- CONCURRENTLY — matches migration 037.

CREATE INDEX IF NOT EXISTS idx_beads_fields_slack_card_ts
    ON beads ((fields->>'slack_card_ts'))
    WHERE fields ? 'slack_card_ts';

ANALYZE beads;
