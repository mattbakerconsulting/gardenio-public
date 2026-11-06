-- Reverse 045_agent_handle_counters.up.sql. Dropping the table forfeits the
-- monotonic history: a subsequent re-apply restarts every captain's sequence at
-- 1. That is acceptable only on a full rollback (no agent_handle allocation has
-- shipped), matching the up migration's "no backfill" going-forward stance.
DROP TABLE IF EXISTS agent_handle_counters;
