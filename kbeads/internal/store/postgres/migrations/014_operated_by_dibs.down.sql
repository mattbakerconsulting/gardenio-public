-- Reverse 014_operated_by_dibs.up.sql in dependency order: drop the VIEW
-- first (it references mutation_history columns), then the partial indexes,
-- then the column itself.

DROP VIEW IF EXISTS bead_operator_history;

DROP INDEX IF EXISTS idx_mutation_history_op_dibs;
DROP INDEX IF EXISTS idx_mutation_history_bead_operated_by;

ALTER TABLE mutation_history
    DROP COLUMN IF EXISTS operated_by_set_at;
