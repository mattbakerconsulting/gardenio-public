-- Re-establish NOT NULL on beads/comments/deps.operated_by_id after a
-- production incident in which the constraint was manually dropped to stop
-- write 5xxs from a daemon that pre-dated the resolveAuditOperatorRequired
-- wiring (commit 27343cc8). With the resolver now in the running daemon,
-- every new write stamps a non-NULL operator_id; this migration repair-
-- backfills any rows inserted during the gap and reinstates the constraint.
--
-- The backfill mirrors migration 017 Pass 1/2/3 verbatim so callers re-
-- running 022 against an already-clean schema get a no-op (every UPDATE
-- filters on operated_by_id IS NULL). Pass 1+2 binds rows whose legacy
-- operated_by TEXT parses to a known operator_alias; Pass 3 sentinel-binds
-- the rest to system:legacy-pre-attribution (seeded by migration 016).
--
-- Tracking bead: ka-0000006380.

-- ---------------------------------------------------------------------------
-- Pass 1: backfill operators + canonical aliases for any new operated_by
-- TEXT values that arrived since 017 ran.
-- ---------------------------------------------------------------------------
INSERT INTO operators (kind, subject)
SELECT DISTINCT
    split_part(operated_by, ':', 1) AS kind,
    substring(operated_by FROM position(':' IN operated_by) + 1) AS subject
FROM (
    SELECT operated_by FROM beads    WHERE operated_by_id IS NULL AND operated_by <> ''
    UNION
    SELECT operated_by FROM comments WHERE operated_by_id IS NULL AND operated_by <> ''
    UNION
    SELECT operated_by FROM deps     WHERE operated_by_id IS NULL AND operated_by <> ''
) src
WHERE position(':' IN operated_by) > 1
  AND position(':' IN operated_by) < length(operated_by)
ON CONFLICT (kind, subject) DO NOTHING;

INSERT INTO operator_aliases (operator_id, kind, subject, source)
SELECT id, kind, subject, 'migration'
  FROM operators
ON CONFLICT (kind, subject) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Pass 2: bind operated_by_id from legacy TEXT via operator_aliases.
-- Idempotent (filters on operated_by_id IS NULL).
-- ---------------------------------------------------------------------------
UPDATE beads t
   SET operated_by_id = a.operator_id
  FROM operator_aliases a
 WHERE t.operated_by_id IS NULL
   AND t.operated_by <> ''
   AND position(':' IN t.operated_by) > 1
   AND a.kind    = split_part(t.operated_by, ':', 1)
   AND a.subject = substring(t.operated_by FROM position(':' IN t.operated_by) + 1);

UPDATE comments t
   SET operated_by_id = a.operator_id
  FROM operator_aliases a
 WHERE t.operated_by_id IS NULL
   AND t.operated_by <> ''
   AND position(':' IN t.operated_by) > 1
   AND a.kind    = split_part(t.operated_by, ':', 1)
   AND a.subject = substring(t.operated_by FROM position(':' IN t.operated_by) + 1);

UPDATE deps t
   SET operated_by_id = a.operator_id
  FROM operator_aliases a
 WHERE t.operated_by_id IS NULL
   AND t.operated_by <> ''
   AND position(':' IN t.operated_by) > 1
   AND a.kind    = split_part(t.operated_by, ':', 1)
   AND a.subject = substring(t.operated_by FROM position(':' IN t.operated_by) + 1);

-- ---------------------------------------------------------------------------
-- Pass 3: sentinel-bind every still-NULL row to system:legacy-pre-attribution.
-- Catches rows whose operated_by TEXT was empty or unsplittable — the same
-- policy migration 017 §Pass 3 applied. NOT NULL below is then structural.
-- ---------------------------------------------------------------------------
UPDATE beads
   SET operated_by_id = (SELECT id FROM operators WHERE kind = 'system' AND subject = 'legacy-pre-attribution')
 WHERE operated_by_id IS NULL;

UPDATE comments
   SET operated_by_id = (SELECT id FROM operators WHERE kind = 'system' AND subject = 'legacy-pre-attribution')
 WHERE operated_by_id IS NULL;

UPDATE deps
   SET operated_by_id = (SELECT id FROM operators WHERE kind = 'system' AND subject = 'legacy-pre-attribution')
 WHERE operated_by_id IS NULL;

-- Re-establish the constraint dropped during the incident. SET NOT NULL is a
-- no-op when the column is already NOT NULL, so this is safe to apply to
-- envs that never lost it.
ALTER TABLE beads    ALTER COLUMN operated_by_id SET NOT NULL;
ALTER TABLE comments ALTER COLUMN operated_by_id SET NOT NULL;
ALTER TABLE deps     ALTER COLUMN operated_by_id SET NOT NULL;
