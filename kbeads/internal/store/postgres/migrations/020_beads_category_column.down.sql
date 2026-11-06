-- Reverse 018_beads_category_column.up.sql.

DROP INDEX IF EXISTS idx_beads_inbox_predicate;

DROP TRIGGER IF EXISTS deps_recompute_child_category_trigger ON deps;
DROP TRIGGER IF EXISTS beads_set_category_trigger ON beads;

DROP FUNCTION IF EXISTS deps_recompute_child_category();
DROP FUNCTION IF EXISTS beads_set_category();
DROP FUNCTION IF EXISTS derive_bead_category(TEXT, TEXT);

ALTER TABLE beads DROP COLUMN IF EXISTS category;
