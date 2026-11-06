-- P6: drop the legacy operated_by TEXT columns and the operated_by_display
-- TEXT denorm column added by migration 012. Implements
-- docs/adr/human-attribution-identity-v2.md §4.2 row 017, §6 Phase 6.
--
-- Bead: ka-0000001094. Umbrella: ka-0000003625.
--
-- The operated_by_id (BIGINT) FK column populated by migration 015 and the
-- backfill-and-cutover sequence in migrations 016 / 017 / 022 is now the
-- sole source of truth for operator identity on every audit table. Reads
-- resolve operators.kind || ':' || operators.subject (and operators.display,
-- operators.email) via LEFT JOIN, replacing the TEXT denorm. The
-- replacement _id-keyed indexes (idx_beads_operated_by_id,
-- idx_beads_operated_by_id_fleet_status, idx_events_operated_by_id_created_at,
-- idx_mutation_history_operated_by_id_created_at) were installed by
-- migration 015 §I1-I4 and have been live throughout P5.
--
-- Pre-conditions enforced operationally (NOT in this file):
--   - 022 applied (operated_by_id NOT NULL on beads / comments / deps).
--   - P5 soak (ka-0000003546) confirmed write/read paths only need _id.
--   - All Go callers of operatedByFromContext / resolveOperatedByPure were
--     swapped to resolveOperator before this migration shipped.
--   - Application no longer writes operated_by TEXT in any INSERT / UPDATE.
--
-- POINT OF NO RETURN: rolling forward only. The .down.sql best-effort
-- recreates the columns empty + JOIN-derives values from operators, but the
-- partial indexes / row-by-row write history of the original column cannot
-- be reconstructed. Treat this as forward-only; recovery from a bad deploy
-- is via redeploy of the previous daemon image plus a cold restore, NOT a
-- migration rollback. See ADR §6 Phase 6 "roll forward" note.

-- Drop the TEXT-keyed partial indexes added by migration 012.
-- Their _id-keyed replacements (migration 015 §I1-I4) are already in use.
DROP INDEX IF EXISTS idx_beads_operated_by;
DROP INDEX IF EXISTS idx_beads_operated_by_fleet_status;
DROP INDEX IF EXISTS idx_events_operated_by_created_at;
DROP INDEX IF EXISTS idx_mutation_history_operated_by_created_at;

-- Drop the TEXT denorm columns. operated_by_set_at on mutation_history
-- (added by migration 014) is orthogonal to identity shape and intentionally
-- kept — it tracks when dibs flipped, not who flipped them.
ALTER TABLE beads             DROP COLUMN IF EXISTS operated_by;
ALTER TABLE beads             DROP COLUMN IF EXISTS operated_by_display;
ALTER TABLE comments          DROP COLUMN IF EXISTS operated_by;
ALTER TABLE deps              DROP COLUMN IF EXISTS operated_by;
ALTER TABLE events            DROP COLUMN IF EXISTS operated_by;
ALTER TABLE mutation_history  DROP COLUMN IF EXISTS operated_by;
