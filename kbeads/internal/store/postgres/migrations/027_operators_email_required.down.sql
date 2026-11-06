-- Rollback for R2 (ka-0000008244): drop the CHECK constraint and restore
-- the empty-string DEFAULT. We deliberately do NOT clear the email column
-- (data was either real, harvested from user beads, or marked with the
-- `@noreply.kbeads.local` sentinel domain — losing it on a rollback would
-- discard work that the up migration did and that the resolver has been
-- preserving since). Older daemons (pre-R2) inserted rows with `email = ''`
-- via the default; restoring the default re-enables that behavior. The
-- CHECK removal is what actually lets those writes land again.

ALTER TABLE operators
    DROP CONSTRAINT IF EXISTS operators_email_not_empty;

ALTER TABLE operators
    ALTER COLUMN email SET DEFAULT '';
