-- Captain-scoped monotonic counter for stable, human-meaningful agent_handle
-- naming (ka-0000002870, persistent-agents epic ka-0000006566 §4). The handle
-- replaces the opaque `thread-<unixts>-<n>` agent name with `<captain>-<N>`,
-- where N is a per-captain sequence that NEVER resets when old agent beads are
-- pruned. That "never resets after pruning" invariant is exactly why the counter
-- cannot be derived from live type=agent beads (a prune would rewind it) and
-- must live in its own durable table, decoupled from bead lifecycle.
--
-- One row per captain_id (the canonical 4-tier id: explicit captain_id,
-- schedule_id/trigger_id, synthetic-captain:<spawn_source>, or
-- synthetic-captain:unknown). next_seq holds the last value handed out; the
-- allocator (queryAllocateAgentHandleSeq) does an atomic
-- `INSERT ... ON CONFLICT DO UPDATE SET next_seq = next_seq + 1 RETURNING`,
-- so concurrent agent creates for the same captain each get a distinct,
-- monotonically increasing value with no read-modify-write race.
--
-- Migration policy (kbeads/CLAUDE.md): idempotent DDL only (CREATE TABLE IF NOT
-- EXISTS), no backfill (going-forward allocation only; legacy agents keep their
-- existing names), runs via `kd migrate` in a pre-upgrade Job.
CREATE TABLE IF NOT EXISTS agent_handle_counters (
    captain_id TEXT PRIMARY KEY,
    next_seq   BIGINT NOT NULL DEFAULT 0
);
