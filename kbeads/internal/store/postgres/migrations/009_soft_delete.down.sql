DROP INDEX IF EXISTS idx_beads_deleted_at;
ALTER TABLE beads DROP COLUMN IF EXISTS deleted_at;
