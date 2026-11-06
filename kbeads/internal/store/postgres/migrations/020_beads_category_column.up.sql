-- Materialize beads.category from DeriveCategory (kbeads/internal/server/category.go)
-- via triggers. Avoids per-row category derivation in the hot path of
-- /operator-inbox queries (see inbox-constellation §7.3, ka-0000006175).
--
-- STORED generated columns are disqualified: PostgreSQL only recomputes
-- STORED expressions on UPDATE of the host row, not on UPDATE of an
-- FK-referenced row. Reparenting a bead changes its category but the
-- referenced parent row is what changed, not the bead's own row.
--
-- Solution: BEFORE INSERT/UPDATE trigger on beads (recompute on row change)
-- + AFTER INSERT/UPDATE/DELETE trigger on deps for parent-child edges
-- (recompute affected child when its parent edge changes).
--
-- Mirrors the rule table in kbeads/internal/server/category.go DeriveCategory.
-- Keep both in sync; the Go function is the spec, this trigger is the
-- materialization. Test parity is enforced by the rule-table tests in
-- kbeads/internal/server/category_test.go.
--
-- Limitations (out of scope for this migration):
--   - Cascade through grandchildren when an ancestor's category flips is
--     NOT handled here. In practice rule 4 ("epic under intake-classified
--     parent → intake") is the only multi-hop dependency, and it only
--     bites when an epic-of-epics is reparented. A follow-up trigger
--     (AFTER UPDATE OF category ON beads) would close this gap; intentionally
--     deferred to keep this migration scoped to the two triggers the
--     design called for.
--   - Label changes (INSERT/DELETE on labels) DO affect rules 2 and 5 but
--     are similarly out of scope for this initial migration. The bead's
--     category will refresh on the next UPDATE of the beads row (a label
--     change is rarely standalone in practice). Documented as follow-up.

-- Idempotent DDL so a partial-apply (column landed but downstream failed,
-- leaving schema_migrations.dirty=true) can be retried without tripping on
-- `column "category" already exists`.
ALTER TABLE beads ADD COLUMN IF NOT EXISTS category TEXT;

-- Pure derivation function. Reads parent / parent.category / parent.labels
-- via SQL because trigger context only gives us the bead row directly.
-- Does NOT call itself recursively: rule 4/5/6 consult the already-stored
-- parent.category column, which is maintained by this same trigger family.
--
-- p_id is the bead's id, p_type is its type (passed in from the trigger so
-- BEFORE-INSERT calls don't need to read the not-yet-committed row).
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

-- BEFORE INSERT/UPDATE trigger on beads: stamp category in-place.
CREATE OR REPLACE FUNCTION beads_set_category() RETURNS trigger AS $$
BEGIN
    NEW.category := derive_bead_category(NEW.id, NEW.type);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS beads_set_category_trigger ON beads;
CREATE TRIGGER beads_set_category_trigger
BEFORE INSERT OR UPDATE ON beads
FOR EACH ROW EXECUTE FUNCTION beads_set_category();

-- AFTER INSERT/UPDATE/DELETE trigger on deps: when a parent-child edge
-- changes, recompute the affected child's category. The UPDATE on beads
-- re-fires the BEFORE UPDATE trigger which calls derive_bead_category
-- with the now-current parent edge.
CREATE OR REPLACE FUNCTION deps_recompute_child_category() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.type = 'parent-child' THEN
            UPDATE beads SET category = category WHERE id = OLD.bead_id;
        END IF;
        RETURN OLD;
    END IF;

    IF TG_OP = 'INSERT' THEN
        IF NEW.type = 'parent-child' THEN
            UPDATE beads SET category = category WHERE id = NEW.bead_id;
        END IF;
        RETURN NEW;
    END IF;

    -- UPDATE
    IF NEW.type = 'parent-child' OR OLD.type = 'parent-child' THEN
        IF NEW.bead_id IS DISTINCT FROM OLD.bead_id THEN
            UPDATE beads SET category = category WHERE id = OLD.bead_id;
        END IF;
        UPDATE beads SET category = category WHERE id = NEW.bead_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS deps_recompute_child_category_trigger ON deps;
CREATE TRIGGER deps_recompute_child_category_trigger
AFTER INSERT OR UPDATE OR DELETE ON deps
FOR EACH ROW EXECUTE FUNCTION deps_recompute_child_category();

-- NOTE (ka-0000000879): the eager fixed-point backfill that lived here
-- was removed. On the gardenio-next 313K-row env it held ACCESS EXCLUSIVE
-- on `beads` for 15+ minutes — every read AND write blocked for the
-- whole upgrade window (ka-0000006380). Even with the pre-upgrade
-- migration Job (ka-0000007607) decoupling time from the kubelet
-- startup probe, the lock window is itself the service-degradation.
--
-- Going-forward policy: schema migrations stay DDL-only. Data backfills
-- belong in either a controller cron, a one-shot `kd admin` command,
-- or a SQL script the operator runs in batches outside any long
-- transaction (each batch as its own commit so the lock is released).
-- See kbeads/CLAUDE.md.
--
-- Effect on existing rows: legacy beads land with category=NULL. The
-- BEFORE INSERT/UPDATE trigger above stamps category on every write
-- going forward, so any bead touched after this migration applies will
-- end up correctly categorized. The inbox query filters
-- `category='intake'` and is therefore correct-by-elision for legacy
-- rows (they don't appear in the inbox surface until they're written
-- to, which is the right behavior — silent rows aren't intake).
--
-- One-shot batched backfill for envs that want full historical
-- coverage. Each iteration is its own implicit transaction (psql
-- autocommits standalone -c statements), so the row-level locks are
-- released between 5000-row chunks rather than held for the whole
-- table:
--
--   while [ "$(psql -tAc "SELECT count(*) FROM beads WHERE category IS NULL")" -gt 0 ]; do
--     psql -c "UPDATE beads SET category = derive_bead_category(id, type)
--              WHERE id IN (SELECT id FROM beads WHERE category IS NULL LIMIT 5000)"
--   done

-- Partial index for the /operator-inbox hot path. urgency_score (ka-0000003203)
-- does not yet exist; the index is currently keyed only on (operated_by_id,
-- category) and the urgency_score ordering will be added in a follow-up
-- migration once that column lands.
CREATE INDEX IF NOT EXISTS idx_beads_inbox_predicate
    ON beads (operated_by_id, category)
    WHERE category = 'intake';
