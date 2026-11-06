-- Reverse 048_taint_effects.up.sql.
ALTER TABLE taints DROP COLUMN IF EXISTS effects;
