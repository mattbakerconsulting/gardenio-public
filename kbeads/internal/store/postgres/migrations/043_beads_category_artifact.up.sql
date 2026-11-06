-- Give deliverable/artifact bead types their own materialized category
-- (ka-0000007274, follow-up to ka-0000002596). Before this migration the
-- deliverable types (bundle, dashboard, rich-report, mr, merge-request) had no
-- DeriveCategory rule and fell through to the `intermediate` fallback (rule 10)
-- — the exact category the `gb ready` leaf-work default scopes FOR — so every
-- published deliverable flooded the ready queue as unclaimable work. This adds a
-- rule-3 `artifact` category for them, mirroring the Go DeriveCategory change in
-- kbeads/internal/server/category.go. Keep both in sync; the Go function is the
-- spec, this trigger is the materialization, and parity is enforced by the
-- rule-table tests in kbeads/internal/server/category_test.go.
--
-- Migration policy (kbeads/CLAUDE.md): idempotent DDL only (CREATE OR REPLACE
-- FUNCTION), no eager full-table backfill (the ~9.4K existing deliverable rows
-- are filled by the batched `kd admin backfill-category` sweep, not here), runs
-- via `kd migrate` in a pre-upgrade Job (no startup-probe deadline). Replacing
-- derive_bead_category in place means the BEFORE INSERT/UPDATE trigger installed
-- by migration 020 immediately stamps `artifact` on every deliverable bead
-- written after this migration applies — zero backfill for going-forward rows.
--
-- Legacy deliverable rows keep whatever category they were last stamped with
-- (NULL for rows never written since migration 020, or `intermediate` for rows
-- written between 020 and this migration). They correct on their next write, or
-- via the batched backfill. This is correct-by-elision for the `gb ready` leaf
-- default, which scopes category=intermediate: a NULL-category row is already
-- excluded, and an `intermediate`-stamped legacy deliverable is swept by the
-- backfill. See ka-0000007274.

-- Replace the derivation function in place. Only the new rule-3 artifact branch
-- is added; every other rule keeps its migration-020 behavior and ordering.
-- Rule numbers below match category.go DeriveCategory after this change.
CREATE OR REPLACE FUNCTION derive_bead_category(p_id TEXT, p_type TEXT)
RETURNS TEXT AS $$
DECLARE
    v_parent_id          TEXT;
    v_parent_type        TEXT;
    v_parent_category    TEXT;
    v_has_source         BOOLEAN;
    v_has_op_blocking    BOOLEAN;
    v_parent_has_source  BOOLEAN;
BEGIN
    -- Rule 1: decision/escalation always action.
    IF p_type IN ('decision', 'escalation') THEN
        RETURN 'action';
    END IF;

    -- Rule 2: bug + flag:operator-blocking → action.
    SELECT EXISTS (
        SELECT 1 FROM labels
        WHERE bead_id = p_id AND label = 'flag:operator-blocking'
    ) INTO v_has_op_blocking;
    IF p_type = 'bug' AND v_has_op_blocking THEN
        RETURN 'action';
    END IF;

    -- Rule 3: deliverable/artifact types → artifact. Membership is type-only
    -- and unconditional (precedes the source-label rule), so a source-tagged
    -- deliverable is still `artifact`, never `intake`. Mirrors
    -- isDeliverableType in category.go.
    IF p_type IN ('bundle', 'dashboard', 'rich-report', 'mr', 'merge-request') THEN
        RETURN 'artifact';
    END IF;

    -- Look up parent via deps. A bead may have multiple parent-child rows
    -- (e.g. a child explicitly mentioned under two epics); pick the lowest
    -- depends_on_id deterministically. The Go DeriveCategory takes a single
    -- parent argument; this matches.
    SELECT depends_on_id INTO v_parent_id
    FROM deps
    WHERE bead_id = p_id AND type = 'parent-child'
    ORDER BY depends_on_id
    LIMIT 1;

    IF v_parent_id IS NOT NULL THEN
        SELECT type, category INTO v_parent_type, v_parent_category
        FROM beads WHERE id = v_parent_id;
        SELECT EXISTS (
            SELECT 1 FROM labels
            WHERE bead_id = v_parent_id AND label LIKE 'source:%'
        ) INTO v_parent_has_source;
    END IF;

    SELECT EXISTS (
        SELECT 1 FROM labels
        WHERE bead_id = p_id AND label LIKE 'source:%'
    ) INTO v_has_source;

    -- Rule 4: epic under epic / source-tagged parent → intermediate.
    IF p_type = 'epic' AND v_parent_id IS NOT NULL
       AND (v_parent_type = 'epic' OR v_parent_has_source) THEN
        RETURN 'intermediate';
    END IF;

    -- Rule 5: epic with no parent or parent classified intake → intake.
    IF p_type = 'epic'
       AND (v_parent_id IS NULL OR v_parent_category = 'intake') THEN
        RETURN 'intake';
    END IF;

    -- Rule 6: source:* label and parent NOT intermediate → intake.
    IF v_has_source AND COALESCE(v_parent_category, '') <> 'intermediate' THEN
        RETURN 'intake';
    END IF;

    -- Rule 7: task/bug under intermediate parent → intermediate.
    IF p_type IN ('task', 'bug') AND v_parent_category = 'intermediate' THEN
        RETURN 'intermediate';
    END IF;

    -- Rule 8: task/bug with no parent → intermediate.
    IF p_type IN ('task', 'bug') AND v_parent_id IS NULL THEN
        RETURN 'intermediate';
    END IF;

    -- Rule 9: config-shaped types.
    IF p_type IN ('config', 'account', 'project', 'schedule',
                  'advice', 'formula', 'agent', 'fleet') THEN
        RETURN 'config';
    END IF;

    -- Rule 10: fallback.
    RETURN 'intermediate';
END;
$$ LANGUAGE plpgsql;

-- No eager backfill here (see header + kbeads/CLAUDE.md "Migration policy").
-- The BEFORE INSERT/UPDATE trigger from migration 020 (beads_set_category_trigger)
-- calls the replaced function above, so every deliverable bead written after
-- this migration lands as category='artifact'. Existing rows are filled in
-- batches by `kd admin backfill-category`, each batch its own transaction:
--
--   while [ "$(psql -tAc "SELECT count(*) FROM beads
--            WHERE type IN ('bundle','dashboard','rich-report','mr','merge-request')
--              AND category IS DISTINCT FROM 'artifact'")" -gt 0 ]; do
--     psql -c "UPDATE beads SET category = derive_bead_category(id, type)
--              WHERE id IN (
--                SELECT id FROM beads
--                WHERE type IN ('bundle','dashboard','rich-report','mr','merge-request')
--                  AND category IS DISTINCT FROM 'artifact'
--                LIMIT 5000)"
--   done
