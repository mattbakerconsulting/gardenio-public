-- One-shot backfill for gd-Ndam8t6aMb: give machine-held beads back to their
-- previous owner.
--
-- Until gd-Ndam8t6aMb, a write by a machine principal (machine:gardenio-
-- controller, machine:gardenio-beads-viewer, ...) took dibs: it flipped
-- beads.operated_by_id to the machine. The daemon no longer does that, but
-- beads flipped before the release stay machine-held. For every live bead
-- whose operated_by_id is a kind='machine' operator, this script restores the
-- most recent non-machine operator in its operator history (the
-- bead_operator_history rows, read from mutation_history directly so the
-- operator FK is available) and appends a field='operated_by' history row with
-- actor 'backfill:gd-Ndam8t6aMb'. bead_operator_history classifies that row as
-- source_kind='dibs-flip' ('sentinel-attempt' when the restored owner is a
-- system:/fleet: sentinel, per the view's CASE).
--
-- Beads with no non-machine operator in their history (agent and session
-- beads the controller created itself) are left alone. updated_at is not
-- touched, so the restore does not reorder recently-updated lists.
--
-- Run it AFTER the daemon carrying the gd-Ndam8t6aMb fix is rolled out,
-- otherwise the next machine write flips the bead again. It is idempotent: a
-- second run finds no machine-held bead with a non-machine predecessor.
--
--   psql "$BEADS_DSN" -v ON_ERROR_STOP=1 -f kbeads/scripts/backfill-machine-dibs.sql
--
-- The script ends in ROLLBACK so the first run is a dry run that prints the
-- counts. Change the last line to COMMIT to apply.

BEGIN;

CREATE TEMP TABLE machine_dibs_restore ON COMMIT DROP AS
SELECT b.id                       AS bead_id,
       b.type                     AS bead_type,
       mo.id                      AS machine_id,
       mo.kind || ':' || mo.subject AS machine_operator,
       r.id                       AS restore_id,
       r.kind || ':' || r.subject AS restore_operator
  FROM beads b
  JOIN operators mo ON mo.id = b.operated_by_id AND mo.kind = 'machine'
  JOIN LATERAL (
        SELECT o.id, o.kind, o.subject
          FROM mutation_history mh
          JOIN operators o ON o.id = mh.operated_by_id
         WHERE mh.bead_id = b.id
           AND mh.field IN ('operated_by', 'operated_by_id')
           AND o.kind <> 'machine'
           AND o.forgotten_at IS NULL
         ORDER BY mh.created_at DESC, mh.id DESC
         LIMIT 1
       ) r ON true
 WHERE b.deleted_at IS NULL;

-- Machine-held beads by type, split into restorable and left alone.
SELECT b.type                                       AS bead_type,
       count(*)                                     AS machine_held,
       count(r.bead_id)                             AS restorable,
       count(*) - count(r.bead_id)                  AS left_alone
  FROM beads b
  JOIN operators mo ON mo.id = b.operated_by_id AND mo.kind = 'machine'
  LEFT JOIN machine_dibs_restore r ON r.bead_id = b.id
 WHERE b.deleted_at IS NULL
 GROUP BY b.type
 ORDER BY machine_held DESC;

-- Where the restored beads go.
SELECT machine_operator, restore_operator, count(*) AS beads
  FROM machine_dibs_restore
 GROUP BY machine_operator, restore_operator
 ORDER BY beads DESC;

-- The guard on operated_by_id skips a bead a human reclaimed since the temp
-- table was built.
WITH flipped AS (
    UPDATE beads b
       SET operated_by_id = r.restore_id
      FROM machine_dibs_restore r
     WHERE b.id = r.bead_id
       AND b.operated_by_id = r.machine_id
 RETURNING r.bead_id, r.machine_operator, r.restore_operator, r.restore_id
)
INSERT INTO mutation_history (bead_id, actor, field, old_value, new_value, mutation_type, operated_by_id)
SELECT bead_id, 'backfill:gd-Ndam8t6aMb', 'operated_by', machine_operator, restore_operator, 'update', restore_id
  FROM flipped;

ROLLBACK;
