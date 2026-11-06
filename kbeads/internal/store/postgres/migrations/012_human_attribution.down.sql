DROP INDEX IF EXISTS idx_mutation_history_operated_by_created_at;
DROP INDEX IF EXISTS idx_events_operated_by_created_at;
DROP INDEX IF EXISTS idx_beads_operated_by_fleet_status;
DROP INDEX IF EXISTS idx_beads_operated_by;

ALTER TABLE mutation_history DROP COLUMN IF EXISTS operated_by;
ALTER TABLE events            DROP COLUMN IF EXISTS operated_by;
ALTER TABLE deps              DROP COLUMN IF EXISTS operated_by;
ALTER TABLE comments          DROP COLUMN IF EXISTS operated_by;
ALTER TABLE beads             DROP COLUMN IF EXISTS operated_by_display;
ALTER TABLE beads             DROP COLUMN IF EXISTS operated_by;
