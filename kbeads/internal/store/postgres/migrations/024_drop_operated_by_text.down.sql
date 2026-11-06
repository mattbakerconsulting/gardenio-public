-- P6 rollback: best-effort recreation of the operated_by TEXT columns and
-- the partial indexes from migration 012. Declared "roll forward" — the
-- recreated TEXT columns are derived via JOIN to operators on operated_by_id
-- where the FK is set, otherwise empty string (the migration 012 default).
-- Per-row write history of the original TEXT column cannot be reconstructed.
--
-- ADR §6 Phase 6: "Recovery from a bad P6 deploy is via redeploying the
-- previous daemon image plus a cold restore, NOT migration rollback." This
-- file exists so `kd migrate down` does not error, not because rollback is
-- supported.

-- Recreate columns with the migration 012 schema (NOT NULL DEFAULT '').
ALTER TABLE beads
    ADD COLUMN IF NOT EXISTS operated_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS operated_by_display TEXT;

ALTER TABLE comments         ADD COLUMN IF NOT EXISTS operated_by TEXT NOT NULL DEFAULT '';
ALTER TABLE deps             ADD COLUMN IF NOT EXISTS operated_by TEXT NOT NULL DEFAULT '';
ALTER TABLE events           ADD COLUMN IF NOT EXISTS operated_by TEXT NOT NULL DEFAULT '';
ALTER TABLE mutation_history ADD COLUMN IF NOT EXISTS operated_by TEXT NOT NULL DEFAULT '';

-- Best-effort reverse-derive: where operated_by_id is set, materialize the
-- canonical "kind:subject" string from operators. Rows with NULL _id stay
-- empty (the legacy default). operated_by_display is similarly best-effort.
UPDATE beads SET
        operated_by         = COALESCE(o.kind || ':' || o.subject, ''),
        operated_by_display = COALESCE(o.display, '')
    FROM operators o
    WHERE beads.operated_by_id = o.id;

UPDATE comments SET operated_by = COALESCE(o.kind || ':' || o.subject, '')
    FROM operators o WHERE comments.operated_by_id = o.id;

UPDATE deps SET operated_by = COALESCE(o.kind || ':' || o.subject, '')
    FROM operators o WHERE deps.operated_by_id = o.id;

UPDATE events SET operated_by = COALESCE(o.kind || ':' || o.subject, '')
    FROM operators o WHERE events.operated_by_id = o.id;

UPDATE mutation_history SET operated_by = COALESCE(o.kind || ':' || o.subject, '')
    FROM operators o WHERE mutation_history.operated_by_id = o.id;

-- Recreate the partial indexes from migration 012 (TEXT-keyed). These mirror
-- the original definitions verbatim so a follow-up restore from cold backup
-- can re-attach without index rebuild churn.
CREATE INDEX IF NOT EXISTS idx_beads_operated_by
    ON beads(operated_by)
    WHERE operated_by <> '';

CREATE INDEX IF NOT EXISTS idx_beads_operated_by_fleet_status
    ON beads(fleet, operated_by, status)
    WHERE operated_by <> '';

CREATE INDEX IF NOT EXISTS idx_events_operated_by_created_at
    ON events(operated_by, created_at)
    WHERE operated_by <> '';

CREATE INDEX IF NOT EXISTS idx_mutation_history_operated_by_created_at
    ON mutation_history(operated_by, created_at)
    WHERE operated_by <> '';
