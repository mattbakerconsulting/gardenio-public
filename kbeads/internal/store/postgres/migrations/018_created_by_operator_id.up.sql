-- beads.created_by_operator_id: immortal originator FK.
--
-- Mirrors `operated_by_id` (migration 015) but records WHO RAISED the bead,
-- not the current owner. `operated_by_id` flips on every dibs handoff;
-- `created_by_operator_id` is set once at create-time and never changes.
-- Surfaces the raise-only operator on the inbox card (inbox-constellation
-- design §4.3.1, plan §3.2: BeadDetail.CreatedByOperatorID).
--
-- NULLABLE: legacy rows with no recoverable originator stay NULL. The
-- resolver populates it on new bead writes; this migration only backfills
-- where a `fields->>'created_by_operator_alias'` hint exists alongside a
-- matching `operator_aliases` row.
--
-- No index added: the inbox-card read path fetches a single bead by id
-- (covered by the PK). Any future aggregate query that filters by
-- `created_by_operator_id` (e.g. "all beads I ever raised") can add a
-- partial index in a follow-up — premature now.
--
-- Bead: ka-0000006467. Parent epic: ka-0000006157.

ALTER TABLE beads
    ADD COLUMN created_by_operator_id BIGINT REFERENCES operators(id);

-- Backfill from a `<kind>:<subject>` alias hint stashed in the fields JSON.
-- Mirrors the operated_by → operated_by_id backfill split (queries_operators_backfill.go),
-- but on a single field on a single table so a one-shot UPDATE is fine.
-- Rows without the hint stay NULL — there is no other authoritative source
-- for the originator on pre-attribution rows.
UPDATE beads b
   SET created_by_operator_id = a.operator_id
  FROM operator_aliases a
 WHERE b.created_by_operator_id IS NULL
   AND b.fields ? 'created_by_operator_alias'
   AND position(':' in b.fields ->> 'created_by_operator_alias') > 1
   AND a.kind    = split_part(b.fields ->> 'created_by_operator_alias', ':', 1)
   AND a.subject = substring(b.fields ->> 'created_by_operator_alias' from position(':' in b.fields ->> 'created_by_operator_alias') + 1);
