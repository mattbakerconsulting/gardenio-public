-- Rename-in-progress twin of agent_handle_counters.captain_id (gd-Cizer9ykpz
-- phase 0: additive dual-read/dual-write shim, zero visible rename; full
-- rationale gd-5O9QUlA8zr). Nullable, dual-written alongside captain_id by
-- queryAllocateAgentHandleSeq; readers resolve gardener_id ?? captain_id.
-- Schema reservation only — no backfill of existing rows, matching the
-- migration policy's "no eager full-table backfill" rule.
ALTER TABLE agent_handle_counters ADD COLUMN IF NOT EXISTS gardener_id TEXT;
