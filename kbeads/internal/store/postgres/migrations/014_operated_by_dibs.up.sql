-- Operated_by transitivity ("dibs") substrate (epic ka-0000003639, S1).
-- Authorizing decision: ka-0000004787 — approved scope is "operated_by_set_at
-- column on mutation_history" + the bead_operator_history VIEW + R4 §7
-- partial indexes. No new tables; no column on beads.
--
-- See:
--   - gardenio/docs/research/operated-by-transitivity-r3-dibs.md §4 (rule)
--   - gardenio/docs/research/operated-by-transitivity-r4-audit.md §4/§7 (DDL)
--   - gardenio/docs/research/operated-by-transitivity-r5-rollout.md §2 (S1)
--
-- Dark-deploy contract: every column added here has a sane DEFAULT and the
-- VIEW is a projection; with BEADS_OPERATED_BY_ENABLED unset the resolver
-- returns "" and the dibs hot path never writes a field='operated_by' row,
-- so the view stays inert and behavior matches a pre-flag deploy.

-- operated_by_set_at: the moment the dibs rule (or the insert resolver)
-- last assigned an operator state on this row. Defaults to NOW() so existing
-- mutation_history rows acquire a coherent timestamp at migration time
-- without a backfill — they are treated as "operator state stamped at
-- migration time," conservative wrt downstream debounce.
ALTER TABLE mutation_history
    ADD COLUMN operated_by_set_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- D1 (R4 §7): bead-side history of operator transitions.
-- Powers Q1 (ownership tab), Q5 (debounce probe), Q7 (activity feed).
-- Partial — only field='operated_by' rows participate.
CREATE INDEX idx_mutation_history_bead_operated_by
    ON mutation_history(bead_id, created_at)
    WHERE field = 'operated_by';

-- D2 (R4 §7): operator-side ever-owned lookup.
-- Powers Q4 (`--ever-operated-by=me`). Excludes empty operator rows so
-- pre-cutover / unresolved entries don't bloat the partial set.
CREATE INDEX idx_mutation_history_op_dibs
    ON mutation_history(operated_by, bead_id)
    WHERE field = 'operated_by' AND operated_by <> '';

-- bead_operator_history (R4 §4 Option C): named projection of the dibs
-- timeline over mutation_history. Hides the field='operated_by' filter and
-- the source-kind classification CASE so every reader (viewer ownership
-- tab, future `kd ownership` command, audit reports) targets one stable
-- shape. The CASE classifies each transition as create / refresh /
-- sentinel-attempt / unknown-attempt / dibs-flip per R4 §4.
CREATE VIEW bead_operator_history AS
SELECT
    id,
    bead_id,
    operated_by AS operator_id,
    created_at AS became_owner_at,
    old_value AS prev_operator,
    actor,
    CASE
        WHEN mutation_type = 'create'                                  THEN 'create'
        WHEN operated_by = old_value                                   THEN 'refresh'
        WHEN operated_by LIKE 'system:%' OR operated_by LIKE 'fleet:%' THEN 'sentinel-attempt'
        WHEN operated_by = ''                                          THEN 'unknown-attempt'
        ELSE                                                                'dibs-flip'
    END AS source_kind
FROM mutation_history
WHERE field = 'operated_by';
