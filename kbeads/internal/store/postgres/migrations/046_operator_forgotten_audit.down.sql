-- Reverse 046_operator_forgotten_audit.up.sql. Dropping the columns discards
-- the who/why audit trail of any forget performed while they existed;
-- forgotten_at (migration 030) still marks the rows as soft-deleted, so the
-- redaction itself survives the rollback — only its attribution is lost.
ALTER TABLE operators DROP COLUMN IF EXISTS forgotten_reason;
ALTER TABLE operators DROP COLUMN IF EXISTS forgotten_by;
