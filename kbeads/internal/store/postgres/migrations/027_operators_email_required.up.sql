-- Enforce that every row in `operators` carries a non-empty email (R2 of
-- the "operator is ALWAYS a human with an email" epic ka-0000006809;
-- task ka-0000008244).
--
-- Migration 015 created `operators.email TEXT NOT NULL DEFAULT ''`, so the
-- column has been NOT NULL the whole time — but the default lets the
-- resolver's cold-path UpsertOperator land rows with an empty email, which
-- defeats the point. This migration:
--
--   Pass 1 — oidc subjects are already emails: copy subject → email.
--   Pass 2 — slack operators: JOIN the user-bead directory on
--            fields->>'slack_id' OR fields->>'slack_user_id' and use the
--            user bead's fields->>'email'.
--   Pass 3 — sentinel fallback for any still-empty row. Uses RFC 6762's
--            reserved `.local` TLD so the synthetic addresses can never
--            collide with real mail: `<kind>-<subject>@noreply.kbeads.local`.
--
-- After the three passes every row has a non-empty email. We then DROP the
-- empty-string DEFAULT, ADD a CHECK (email <> '') NOT VALID + VALIDATE
-- (the NOT VALID/VALIDATE split avoids the AccessExclusiveLock-during-scan
-- that a plain ADD CHECK would take), and re-apply SET NOT NULL idempotently
-- to match the task wording. Idempotent end-to-end: re-running on a clean
-- schema is a no-op (Pass updates filter `WHERE email = ''`, ALTERs use IF
-- NOT EXISTS where supported).
--
-- Operators is a small table (~hundreds of rows in prod), so the inline
-- backfill is safe — CLAUDE.md's "no eager full-table backfills" rule
-- targets multi-million-row tables like `beads` / `events`.

-- ---------------------------------------------------------------------------
-- Pass 1: oidc operators already carry their email as the subject.
-- Guard with position('@' IN subject) > 1 so a malformed legacy oidc row
-- (subject not an email) falls through to Pass 3 instead of writing junk.
-- ---------------------------------------------------------------------------
UPDATE operators
   SET email      = subject,
       updated_at = NOW()
 WHERE kind  = 'oidc'
   AND email = ''
   AND position('@' IN subject) > 1
   AND position('@' IN subject) < length(subject);

-- ---------------------------------------------------------------------------
-- Pass 2: slack operators — look up the user-directory bead by slack id and
-- copy its fields->>'email'. Two field names are in flight in the codebase:
--   - fields->>'slack_id'       — user-directory beads (bridge/user_directory.go)
--   - fields->>'slack_user_id'  — general slack-attributed beads
-- The UNION ALL collapses both into a single (slack_id, email) derived set.
-- DISTINCT ON keeps the deterministic "first match wins" — same shape that
-- migration 017 used for similar legacy joins.
-- ---------------------------------------------------------------------------
WITH slack_emails AS (
    SELECT DISTINCT ON (slack_id) slack_id, email
      FROM (
          SELECT fields->>'slack_id'      AS slack_id,
                 NULLIF(fields->>'email','') AS email
            FROM beads
           WHERE type = 'user'
             AND kind = 'config'
             AND fields ? 'slack_id'
             AND fields ? 'email'
          UNION ALL
          SELECT fields->>'slack_user_id' AS slack_id,
                 NULLIF(fields->>'email','') AS email
            FROM beads
           WHERE type = 'user'
             AND kind = 'config'
             AND fields ? 'slack_user_id'
             AND fields ? 'email'
      ) src
     WHERE slack_id IS NOT NULL
       AND email    IS NOT NULL
     ORDER BY slack_id, email   -- stable tiebreak across the UNION
)
UPDATE operators o
   SET email      = s.email,
       updated_at = NOW()
  FROM slack_emails s
 WHERE o.kind    = 'slack'
   AND o.email   = ''
   AND o.subject = s.slack_id;

-- ---------------------------------------------------------------------------
-- Pass 3: synthetic sentinel for any row still empty after Passes 1+2.
-- `.local` is reserved by RFC 6762 for multicast DNS — these addresses can
-- never resolve as real mail, so they are safe as audit-trail markers.
-- Format: `<kind>-<subject>@noreply.kbeads.local`. Includes kind so a
-- slack:U07 and an oidc:U07 (vanishingly unlikely but possible) do not
-- collide on the synthetic side.
--
-- Catches:
--   - all system:* and fleet:* sentinel rows (no real human behind them)
--   - oidc rows whose subject is not an email (legacy garbage, if any)
--   - slack rows whose user bead never landed (resync will overwrite later)
--   - gitlab / jira / custom-kind rows we have not wired backfill for
-- ---------------------------------------------------------------------------
UPDATE operators
   SET email      = kind || '-' || subject || '@noreply.kbeads.local',
       updated_at = NOW()
 WHERE email = '';

-- ---------------------------------------------------------------------------
-- Drop the empty-string default — new INSERTs must now provide an email.
-- The resolver's UpsertOperator wraps deriveOperatorEmail (in the same
-- commit) to satisfy this on every cold-path miss.
-- ---------------------------------------------------------------------------
ALTER TABLE operators
    ALTER COLUMN email DROP DEFAULT;

-- ---------------------------------------------------------------------------
-- CHECK (email <> '') — split into NOT VALID + VALIDATE so that the table-
-- scan VALIDATE phase runs with a SHARE UPDATE EXCLUSIVE lock instead of an
-- AccessExclusiveLock. On a small table this is largely academic, but the
-- migration policy in CLAUDE.md asks for the pattern uniformly, and it
-- keeps the door open for adding kindled rows during the upgrade.
-- ---------------------------------------------------------------------------
ALTER TABLE operators
    DROP CONSTRAINT IF EXISTS operators_email_not_empty;

ALTER TABLE operators
    ADD CONSTRAINT operators_email_not_empty
    CHECK (email <> '') NOT VALID;

ALTER TABLE operators
    VALIDATE CONSTRAINT operators_email_not_empty;

-- ---------------------------------------------------------------------------
-- SET NOT NULL — literal task wording (ka-0000008244). Column was already
-- NOT NULL via migration 015, so this is a no-op on every existing env;
-- re-applying it costs nothing and pins the invariant in this migration
-- for the record.
-- ---------------------------------------------------------------------------
ALTER TABLE operators
    ALTER COLUMN email SET NOT NULL;
