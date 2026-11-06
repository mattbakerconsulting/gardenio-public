-- beads.urgency_score: composite ranking score for inbox-constellation default sort.
-- Implements inbox-constellation-design.md §4.4.3 + §7.9 follow-up
-- (parent epic ka-0000006157, follow-up bead ka-0000003203).
--
-- Formula (D3-r2 round-3 resolution):
--
--   urgency_score = priority_weight(priority)
--                 × age_factor(now - updated_at)
--                 × (1 + downstream_blocked_count)
--
-- where downstream_blocked_count is the number of OPEN beads (excluding
-- closed/deleted) that have a deps row pointing at this bead — i.e. work
-- waiting on this bead to land. The "+1" floor keeps leaf beads ranked
-- above nothing (a P1 leaf still beats a P3 leaf).
--
-- Recompute cadence (default chosen, see MR for review):
--   - BEFORE INSERT/UPDATE on beads: recomputes the bead's own urgency_score
--     in NEW (no extra UPDATE round-trip).
--   - AFTER UPDATE on beads when status flips (open<->closed/deleted):
--     refresh urgency_score for every bead this one depends on (their
--     downstream_blocked_count just changed).
--   - AFTER INSERT/DELETE on deps: refresh urgency_score for the
--     depends_on_id (its downstream count changed).
--   - age_factor drifts by the day, not the second; the column is rounded
--     to whole days and capped at 30, so most beads only need refresh on
--     real changes. A nightly full-table refresh is left to the controller
--     cron (calls SELECT refresh_all_urgency_scores()), not embedded here.
--
-- Column is NULLABLE so existing INSERTs that don't supply it still succeed;
-- the BEFORE trigger fills it in.
--
-- Idempotency: every DDL below uses IF NOT EXISTS / OR REPLACE / DROP-then-
-- CREATE so a partially-applied 019 (e.g. the column landed but a downstream
-- statement failed, leaving schema_migrations.dirty=true) can be retried
-- safely. Without this, the migrate library re-runs from the top and trips
-- on `column "urgency_score" already exists`.

ALTER TABLE beads ADD COLUMN IF NOT EXISTS urgency_score INTEGER;

-- Pure formula function. Inputs are all explicit so the function is
-- IMMUTABLE — same inputs always produce the same output. Callers compute
-- age_seconds = EXTRACT(EPOCH FROM (NOW() - updated_at)) at the call site.
CREATE OR REPLACE FUNCTION compute_urgency_score(
    p_priority           INTEGER,
    p_age_seconds        DOUBLE PRECISION,
    p_downstream_blocked INTEGER
) RETURNS INTEGER
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    v_priority_weight INTEGER;
    v_age_days        INTEGER;
BEGIN
    -- priority_weight: lower numeric priority = higher urgency.
    -- 0 (highest) -> 16, 1 -> 8, 2 (default) -> 4, 3 -> 2, anything else -> 1.
    v_priority_weight := CASE
        WHEN p_priority IS NULL OR p_priority <= 0 THEN 16
        WHEN p_priority = 1 THEN 8
        WHEN p_priority = 2 THEN 4
        WHEN p_priority = 3 THEN 2
        ELSE 1
    END;

    -- age_factor: 1 + floor(age_in_days), clamped to [0,30]. Negative ages
    -- (clock skew) clamp to 0; old beads cap at 30 so a P1 fresh bead
    -- (16 * 1 * 1 = 16) can still beat an ancient leaf (1 * 31 * 1 = 31)
    -- only when it has downstream pressure — preserving "priority + recent"
    -- as the dominant signal.
    v_age_days := GREATEST(
        0,
        LEAST(30, FLOOR(COALESCE(p_age_seconds, 0) / 86400)::INTEGER)
    );

    RETURN v_priority_weight
         * (1 + v_age_days)
         * (1 + GREATEST(0, COALESCE(p_downstream_blocked, 0)));
END;
$$;

-- Counts open downstream beads. A "downstream" of bead X is any bead Y
-- such that Y has a deps row depending on X (Y.deps.depends_on_id = X.id).
-- "Open" excludes status='closed' and soft-deleted rows; status='deferred'
-- and 'blocked' still count as work waiting on X.
CREATE OR REPLACE FUNCTION count_downstream_blocked(p_bead_id TEXT)
RETURNS INTEGER
LANGUAGE plpgsql STABLE AS $$
DECLARE
    v_count INTEGER;
BEGIN
    SELECT COUNT(*)::INTEGER INTO v_count
      FROM deps d
      JOIN beads b ON b.id = d.bead_id
     WHERE d.depends_on_id = p_bead_id
       AND b.status <> 'closed'
       AND b.deleted_at IS NULL;
    RETURN COALESCE(v_count, 0);
END;
$$;

-- Refresh helper used by the dep-edge triggers and by the nightly job.
-- Recomputes one bead's urgency_score by re-reading priority / updated_at
-- and recounting downstream. Safe to call on a missing bead (no-op).
CREATE OR REPLACE FUNCTION refresh_urgency_score(p_bead_id TEXT)
RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
    v_priority    INTEGER;
    v_updated_at  TIMESTAMPTZ;
    v_dsb         INTEGER;
BEGIN
    SELECT priority, updated_at
      INTO v_priority, v_updated_at
      FROM beads WHERE id = p_bead_id;
    IF NOT FOUND THEN
        RETURN;
    END IF;

    v_dsb := count_downstream_blocked(p_bead_id);

    UPDATE beads
       SET urgency_score = compute_urgency_score(
               v_priority,
               EXTRACT(EPOCH FROM (NOW() - v_updated_at)),
               v_dsb
           )
     WHERE id = p_bead_id;
END;
$$;

-- Full-table nightly refresh. Returns the row count touched. Cron-callable
-- from the controller (see MR description for cadence rationale).
CREATE OR REPLACE FUNCTION refresh_all_urgency_scores()
RETURNS INTEGER
LANGUAGE plpgsql AS $$
DECLARE
    v_n INTEGER;
BEGIN
    UPDATE beads SET urgency_score = compute_urgency_score(
        priority,
        EXTRACT(EPOCH FROM (NOW() - updated_at)),
        count_downstream_blocked(id)
    );
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$;

-- BEFORE trigger on beads: stamp urgency_score on the row being written.
-- This handles INSERT (new bead, downstream=0 unless deps already exist)
-- and UPDATE of any column. Computed in-place via NEW so no recursive
-- UPDATE is fired.
CREATE OR REPLACE FUNCTION beads_urgency_before_insup()
RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    v_dsb INTEGER;
BEGIN
    v_dsb := count_downstream_blocked(NEW.id);
    NEW.urgency_score := compute_urgency_score(
        NEW.priority,
        EXTRACT(EPOCH FROM (NOW() - NEW.updated_at)),
        v_dsb
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_beads_urgency_before_insup ON beads;
CREATE TRIGGER trg_beads_urgency_before_insup
BEFORE INSERT OR UPDATE ON beads
FOR EACH ROW
EXECUTE FUNCTION beads_urgency_before_insup();

-- AFTER trigger on beads: when a bead's status or deleted_at changes,
-- the downstream_blocked_count of every bead it depends on changes too.
-- Refresh those.
CREATE OR REPLACE FUNCTION beads_urgency_after_status()
RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    v_target TEXT;
BEGIN
    IF (OLD.status IS DISTINCT FROM NEW.status)
       OR (OLD.deleted_at IS DISTINCT FROM NEW.deleted_at) THEN
        FOR v_target IN
            SELECT depends_on_id FROM deps WHERE bead_id = NEW.id
        LOOP
            PERFORM refresh_urgency_score(v_target);
        END LOOP;
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS trg_beads_urgency_after_status ON beads;
CREATE TRIGGER trg_beads_urgency_after_status
AFTER UPDATE ON beads
FOR EACH ROW
EXECUTE FUNCTION beads_urgency_after_status();

-- AFTER trigger on deps: dependency edge changes shift the
-- downstream_blocked_count of the depends_on_id bead (the one being
-- depended on). INSERT / DELETE / UPDATE all need a refresh of the
-- target side; UPDATE may also shift the OLD target if depends_on_id
-- changed.
CREATE OR REPLACE FUNCTION deps_urgency_after_change()
RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM refresh_urgency_score(NEW.depends_on_id);
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM refresh_urgency_score(OLD.depends_on_id);
    ELSIF TG_OP = 'UPDATE' THEN
        PERFORM refresh_urgency_score(NEW.depends_on_id);
        IF OLD.depends_on_id IS DISTINCT FROM NEW.depends_on_id THEN
            PERFORM refresh_urgency_score(OLD.depends_on_id);
        END IF;
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS trg_deps_urgency_after_change ON deps;
CREATE TRIGGER trg_deps_urgency_after_change
AFTER INSERT OR UPDATE OR DELETE ON deps
FOR EACH ROW
EXECUTE FUNCTION deps_urgency_after_change();

-- Backfill existing rows. Single full-table sweep matches what the nightly
-- refresh job will do, so this also exercises the formula against every
-- pre-existing bead. Wrapped in the migration transaction.
SELECT refresh_all_urgency_scores();

-- No new index in this migration. The composite partial index that the
-- inbox query needs (`(operated_by_id, urgency_score DESC) WHERE
-- category='intake'`) lands with the inbox surface (subtask-7 in
-- inbox-constellation-design.md §4.4.3 / §7.9) — it depends on the
-- `category` column from ka-0000006175 which is being added in parallel.
