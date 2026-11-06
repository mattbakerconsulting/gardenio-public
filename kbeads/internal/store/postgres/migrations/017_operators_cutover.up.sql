-- P4 cutover: enforce NOT NULL on operated_by_id where Option B promised it,
-- flip the dibs view + index from operated_by TEXT to operated_by_id FK.
-- Implements docs/adr/human-attribution-identity-v2.md §4.2 row 016, §4.5
-- (P2 carve-out: history-only tables stay NULLABLE), §3.3 (touched columns).
--
-- Pre-conditions enforced operationally (NOT in this file):
--   - 015 applied (introduces operators / operator_aliases / operated_by_id).
--   - 016 applied (system:legacy-pre-attribution sentinel exists).
--
-- The P3 backfill (`kd admin backfill-operators`) is now performed INLINE
-- below as Pass 1/2/3 so this migration is self-sufficient: a fresh env can
-- apply 015 → 016 → 017 in one go without a separate admin command. The
-- inline backfill mirrors `internal/store/postgres/queries_operators_backfill.go`
-- and is idempotent (every UPDATE filters on operated_by_id IS NULL), so
-- re-applying after a partial P3 run is a no-op for already-bound rows.
--
-- Why inline: the cutover originally required P3 to have run first
-- (`kd admin backfill-operators`). On envs upgrading from a daemon version
-- that pre-dates the P3 admin command (< 2026.126.1), the daemon cannot
-- run P3 and yet cannot start the new version without P3 — a catch-22 that
-- crashlooped prod (see ka-0000000668). Inlining the backfill resolves the
-- ordering hazard for every env, current and future.
--
-- Note on numbering: the cutover slot was originally planned as 016 in the
-- ADR (§4.2 row 016), but 016 was taken by the legacy-pre-attribution
-- sentinel that P3 needs in place ahead of backfill. The cutover therefore
-- lands as 017 and the legacy-TEXT-drop (P6) shifts to 018.
--
-- Bead: ka-0000000071 (P4). Umbrella: ka-0000003625.
-- Inline-backfill amendment: ka-0000000668.

-- ---------------------------------------------------------------------------
-- Pass 1: collect distinct operated_by TEXT values across all five audit
-- tables, split on the first ':' into (kind, subject), and upsert operators
-- + canonical operator_aliases. Mirrors collectAndUpsertOperators() in
-- queries_operators_backfill.go.
--
-- Quarantine policy: values without a `:`, with a leading `:`, or with a
-- trailing `:` cannot be split into a meaningful (kind, subject) and are
-- skipped by the position-of-colon predicate. Pass 3 then sentinel-binds
-- those rows on live-state tables to satisfy the NOT NULL constraint.
-- ---------------------------------------------------------------------------
INSERT INTO operators (kind, subject)
SELECT DISTINCT
    split_part(operated_by, ':', 1) AS kind,
    substring(operated_by FROM position(':' IN operated_by) + 1) AS subject
FROM (
    SELECT operated_by FROM beads            WHERE operated_by_id IS NULL AND operated_by <> ''
    UNION
    SELECT operated_by FROM comments         WHERE operated_by_id IS NULL AND operated_by <> ''
    UNION
    SELECT operated_by FROM deps             WHERE operated_by_id IS NULL AND operated_by <> ''
    UNION
    SELECT operated_by FROM events           WHERE operated_by_id IS NULL AND operated_by <> ''
    UNION
    SELECT operated_by FROM mutation_history WHERE operated_by_id IS NULL AND operated_by <> ''
) src
WHERE position(':' IN operated_by) > 1
  AND position(':' IN operated_by) < length(operated_by)
ON CONFLICT (kind, subject) DO NOTHING;

-- Ensure every operator has at least its canonical (kind, subject) alias.
-- Source = 'migration' matches the Go P3 backfill's source label.
INSERT INTO operator_aliases (operator_id, kind, subject, source)
SELECT id, kind, subject, 'migration'
  FROM operators
ON CONFLICT (kind, subject) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Pass 2: bind operated_by_id on every audit table where the legacy TEXT
-- value parses to a known (kind, subject) alias. Mirrors
-- batchedOperatorsBackfill() / singleShotOperatorsBackfill(); within a
-- migration transaction we just issue one UPDATE per table — row counts
-- (low hundreds-of-thousands at the upper bound) fit comfortably.
-- Idempotent: WHERE operated_by_id IS NULL skips already-bound rows.
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

UPDATE events t
   SET operated_by_id = a.operator_id
  FROM operator_aliases a
 WHERE t.operated_by_id IS NULL
   AND t.operated_by <> ''
   AND position(':' IN t.operated_by) > 1
   AND a.kind    = split_part(t.operated_by, ':', 1)
   AND a.subject = substring(t.operated_by FROM position(':' IN t.operated_by) + 1);

UPDATE mutation_history t
   SET operated_by_id = a.operator_id
  FROM operator_aliases a
 WHERE t.operated_by_id IS NULL
   AND t.operated_by <> ''
   AND position(':' IN t.operated_by) > 1
   AND a.kind    = split_part(t.operated_by, ':', 1)
   AND a.subject = substring(t.operated_by FROM position(':' IN t.operated_by) + 1);

-- ---------------------------------------------------------------------------
-- Pass 3: bind every still-NULL operated_by_id row on the live-state tables
-- (beads / comments / deps) to the system:legacy-pre-attribution sentinel
-- inserted by migration 016. Per ADR §4.5: history-only tables (events,
-- mutation_history) keep NULL when "we genuinely do not know who operated
-- this" — they are deliberately excluded from sentinel binding.
--
-- Catches both empty-string operated_by rows AND quarantined rows whose
-- TEXT could not be split into (kind, subject). The NOT NULL constraint
-- below requires every live-state row to carry a known FK; sentinel is
-- the right default for "operator is unknown / pre-attribution".
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

-- Live-state tables (§4.5 P2 policy): every row carries a known operator
-- after backfill — the legacy-pre-attribution sentinel covers
-- pre-attribution rows so NOT NULL is structural.
ALTER TABLE beads    ALTER COLUMN operated_by_id SET NOT NULL;
ALTER TABLE comments ALTER COLUMN operated_by_id SET NOT NULL;
ALTER TABLE deps     ALTER COLUMN operated_by_id SET NOT NULL;

-- mutation_history and events stay NULLABLE: history-only tables where
-- "we genuinely don't know" is a feature (§4.5 P1 policy).

-- Drop the dibs partial index keyed on the legacy TEXT column. Its FK-keyed
-- replacement idx_mutation_history_op_id_dibs already exists (created by
-- 015) and is the index the rewritten view + future dibs writes rely on.
DROP INDEX IF EXISTS idx_mutation_history_op_dibs;

-- Rewrite bead_operator_history (originally added by 014) to project
-- operated_by_id via JOIN to operators, surfacing the canonical
-- "kind:subject" string so existing readers (operator chip, ownership tab)
-- keep their wire shape (W1, ADR §3.4).
--
-- The view's `field` predicate widens to ('operated_by', 'operated_by_id')
-- so historical rows written before the cutover (field='operated_by')
-- continue to surface alongside post-cutover rows (field='operated_by_id').
-- Both forms join on mh.operated_by_id which P3 backfilled on every
-- non-empty TEXT row.
DROP VIEW IF EXISTS bead_operator_history;

CREATE VIEW bead_operator_history AS
SELECT
    mh.id,
    mh.bead_id,
    -- Canonical alias string for display, sourced via FK JOIN. Empty when
    -- operated_by_id IS NULL (history-only NULLABLE rows).
    COALESCE(o.kind || ':' || o.subject, '') AS operator_id,
    mh.created_at AS became_owner_at,
    mh.old_value AS prev_operator,
    mh.actor,
    CASE
        WHEN mh.mutation_type = 'create'                       THEN 'create'
        WHEN o.id IS NULL                                      THEN 'unknown-attempt'
        WHEN o.kind IN ('system', 'fleet')                     THEN 'sentinel-attempt'
        WHEN mh.old_value = o.kind || ':' || o.subject         THEN 'refresh'
        ELSE                                                        'dibs-flip'
    END AS source_kind
FROM mutation_history mh
LEFT JOIN operators o ON o.id = mh.operated_by_id
WHERE mh.field IN ('operated_by', 'operated_by_id');
