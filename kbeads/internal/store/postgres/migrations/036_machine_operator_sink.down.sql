-- Reverse of 036_machine_operator_sink.up.sql.
-- Removes the system:machine operator, its canonical alias, and its slug
-- alias. Safe even if FK rows already reference system:machine because the
-- up migration only inserts the row; any beads attributed to it would need a
-- separate rebind before this row can be deleted (the FK is ON DELETE
-- RESTRICT by default), so this down migration is a best-effort cleanup for
-- the common case where no machine-origin write has landed yet.

DELETE FROM operator_slug_aliases
 WHERE operator_id IN (
     SELECT id FROM operators WHERE kind = 'system' AND subject = 'machine'
 );

DELETE FROM operator_aliases
 WHERE kind = 'system' AND subject = 'machine';

DELETE FROM operators
 WHERE kind = 'system' AND subject = 'machine';
