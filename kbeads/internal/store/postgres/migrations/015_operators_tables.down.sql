-- Best-effort reversal of 015_operators_tables.up.sql.
--
-- Operational policy: roll forward (ADR §4.2 narrative — reversibility is
-- best-effort; the canonical stance is forward-only). This down migration
-- exists for local dev / test convenience and to keep golang-migrate
-- happy — production never runs it.
--
-- Drops the partial indexes, the operated_by_id FK columns on the five
-- audit tables, and the operator_aliases + operators tables (in dependency
-- order). Pre-seeded sentinel rows are dropped with the operators table.
-- Migration 012's TEXT columns and indexes are untouched.

DROP INDEX IF EXISTS idx_mutation_history_op_id_dibs;
DROP INDEX IF EXISTS idx_mutation_history_operated_by_id_created_at;
DROP INDEX IF EXISTS idx_events_operated_by_id_created_at;
DROP INDEX IF EXISTS idx_beads_operated_by_id_fleet_status;
DROP INDEX IF EXISTS idx_beads_operated_by_id;

ALTER TABLE mutation_history DROP COLUMN IF EXISTS operated_by_id;
ALTER TABLE events           DROP COLUMN IF EXISTS operated_by_id;
ALTER TABLE deps             DROP COLUMN IF EXISTS operated_by_id;
ALTER TABLE comments         DROP COLUMN IF EXISTS operated_by_id;
ALTER TABLE beads            DROP COLUMN IF EXISTS operated_by_id;

DROP INDEX IF EXISTS idx_operator_aliases_operator_id;
DROP TABLE IF EXISTS operator_aliases;
DROP TABLE IF EXISTS operators;
