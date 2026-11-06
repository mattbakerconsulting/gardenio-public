-- Reverse of 030_operator_forgotten.up.sql.

DROP INDEX IF EXISTS idx_operators_forgotten_at;

ALTER TABLE operators
    DROP COLUMN IF EXISTS forgotten_at;
