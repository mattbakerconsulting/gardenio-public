-- Case-fold gitlab and github login subjects in operator_aliases and operators
-- (gd-9AXizUgtzG). The daemon now lowercases gitlab subjects in
-- canonicalAliasSubject (github and oidc already were) and in resolveOperator,
-- because GitLab and GitHub logins are case-insensitive and the bridge user
-- directory matches them so (gd-yrnMQbmLgp). A mixed-case row written before
-- that would never match again, so fold the existing rows to the same form.
--
-- The key is lower(btrim(subject)), what canonicalAliasSubject produces.
-- Duplicates under that key are handled before any rename, so no UPDATE can
-- hit the (kind, subject) primary key:
--
--   1. Spellings of one login that point at the SAME operator collapse to one
--      row: the already-canonical spelling if there is one, else the oldest.
--   2. A key that still has several rows points at DIFFERENT operators. Those
--      rows are left as they are, with a WARNING: choosing a survivor means
--      merging operators and repointing audit rows, which is an admin call
--      (POST /v1/admin/operators/link-aliases), not a migration's.
--   3. Every other mixed-case alias row is renamed to its canonical form.
--   4. operators rows of these kinds are renamed the same way, unless another
--      operator already holds the canonical subject. Only gitlab rows exist in
--      practice (github has no operator email derivation); email and slug are
--      left alone.
--
-- Small tables (one row per alias / per human), so this is not a bead-scale
-- backfill. Both tables are locked so a running old pod cannot insert a
-- colliding spelling between the dedupe and the rename. Idempotent: a second
-- run finds nothing to fold.
DO $$
DECLARE
    deduped    BIGINT;
    renamed    BIGINT;
    ops        BIGINT;
    conflicts  BIGINT;
BEGIN
    LOCK TABLE operators, operator_aliases IN SHARE ROW EXCLUSIVE MODE;

    WITH ranked AS (
        SELECT kind, subject,
               row_number() OVER (
                   PARTITION BY kind, lower(btrim(subject)), operator_id
                   ORDER BY (subject = lower(btrim(subject))) DESC, created_at, subject
               ) AS rn
          FROM operator_aliases
         WHERE kind IN ('gitlab', 'github')
    )
    DELETE FROM operator_aliases a
     USING ranked r
     WHERE a.kind = r.kind AND a.subject = r.subject AND r.rn > 1;
    GET DIAGNOSTICS deduped = ROW_COUNT;

    SELECT count(DISTINCT (a.kind, lower(btrim(a.subject)))) INTO conflicts
      FROM operator_aliases a
     WHERE a.kind IN ('gitlab', 'github')
       AND EXISTS (SELECT 1 FROM operator_aliases b
                    WHERE b.kind = a.kind
                      AND lower(btrim(b.subject)) = lower(btrim(a.subject))
                      AND b.subject <> a.subject);

    UPDATE operator_aliases a
       SET subject = lower(btrim(a.subject))
     WHERE a.kind IN ('gitlab', 'github')
       AND a.subject <> lower(btrim(a.subject))
       AND NOT EXISTS (SELECT 1 FROM operator_aliases b
                        WHERE b.kind = a.kind
                          AND lower(btrim(b.subject)) = lower(btrim(a.subject))
                          AND b.subject <> a.subject);
    GET DIAGNOSTICS renamed = ROW_COUNT;

    UPDATE operators o
       SET subject = lower(btrim(o.subject)), updated_at = NOW()
     WHERE o.kind IN ('gitlab', 'github')
       AND o.subject <> lower(btrim(o.subject))
       AND NOT EXISTS (SELECT 1 FROM operators p
                        WHERE p.kind = o.kind
                          AND p.id <> o.id
                          AND lower(btrim(p.subject)) = lower(btrim(o.subject)));
    GET DIAGNOSTICS ops = ROW_COUNT;

    RAISE NOTICE '054: gitlab/github aliases deduped=% renamed=%, operators renamed=%',
        deduped, renamed, ops;
    IF conflicts > 0 THEN
        RAISE WARNING '054: % gitlab/github login(s) differ only by case across different operators; left unchanged, merge them with POST /v1/admin/operators/link-aliases',
            conflicts;
    END IF;
END
$$;
