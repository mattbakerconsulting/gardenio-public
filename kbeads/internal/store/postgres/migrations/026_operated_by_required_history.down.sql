-- Rollback for R2 (ka-0000005772) history-table NOT NULL constraints.
-- Mirror of 026 up: drop only the CHECK constraints we added. The columns
-- themselves and their indexes are unaffected — older daemons (pre-R2) wrote
-- NULL into operated_by_id on the best-effort observability paths, so
-- dropping the constraints restores that behavior.

ALTER TABLE events
    DROP CONSTRAINT IF EXISTS events_operated_by_id_not_null;

ALTER TABLE mutation_history
    DROP CONSTRAINT IF EXISTS mutation_history_operated_by_id_not_null;
