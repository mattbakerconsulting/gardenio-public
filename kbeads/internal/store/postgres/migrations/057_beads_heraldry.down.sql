DROP INDEX IF EXISTS idx_beads_heraldry_register;
ALTER TABLE beads DROP COLUMN IF EXISTS project;
ALTER TABLE beads DROP COLUMN IF EXISTS heraldry;
