-- Reverse 043_beads_category_artifact.up.sql: restore the migration-020 body of
-- derive_bead_category (no `artifact` rule). Rows already stamped `artifact` are
-- NOT rewritten here (no eager backfill); they correct to the reverted
-- classification on their next write, or via a batched sweep.
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

    -- Rule 3: epic under epic / source-tagged parent → intermediate.
    IF p_type = 'epic' AND v_parent_id IS NOT NULL
       AND (v_parent_type = 'epic' OR v_parent_has_source) THEN
        RETURN 'intermediate';
    END IF;

    -- Rule 4: epic with no parent or parent classified intake → intake.
    IF p_type = 'epic'
       AND (v_parent_id IS NULL OR v_parent_category = 'intake') THEN
        RETURN 'intake';
    END IF;

    -- Rule 5: source:* label and parent NOT intermediate → intake.
    IF v_has_source AND COALESCE(v_parent_category, '') <> 'intermediate' THEN
        RETURN 'intake';
    END IF;

    -- Rule 6: task/bug under intermediate parent → intermediate.
    IF p_type IN ('task', 'bug') AND v_parent_category = 'intermediate' THEN
        RETURN 'intermediate';
    END IF;

    -- Rule 7: task/bug with no parent → intermediate.
    IF p_type IN ('task', 'bug') AND v_parent_id IS NULL THEN
        RETURN 'intermediate';
    END IF;

    -- Rule 8: config-shaped types.
    IF p_type IN ('config', 'account', 'project', 'schedule',
                  'advice', 'formula', 'agent', 'fleet') THEN
        RETURN 'config';
    END IF;

    -- Rule 9: fallback.
    RETURN 'intermediate';
END;
$$ LANGUAGE plpgsql;
