-- Reverse of 022: drop the NOT NULL re-add. Backfilled rows stay populated
-- (no harm in keeping operator_id stamps); only the constraint is removed.
ALTER TABLE beads    ALTER COLUMN operated_by_id DROP NOT NULL;
ALTER TABLE comments ALTER COLUMN operated_by_id DROP NOT NULL;
ALTER TABLE deps     ALTER COLUMN operated_by_id DROP NOT NULL;
