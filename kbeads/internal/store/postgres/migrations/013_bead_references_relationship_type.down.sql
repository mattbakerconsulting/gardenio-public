-- Reverse 013: restore the original (source, target, field) PK and drop the
-- relationship_type column. Note: if any typed-mention rows exist, dropping
-- the column is what makes the PK restore-able — otherwise duplicate
-- (source, target, field) tuples would conflict.

DROP INDEX IF EXISTS idx_bead_references_target_type;
DROP INDEX IF EXISTS bead_references_unique;

ALTER TABLE bead_references
    DROP COLUMN IF EXISTS relationship_type;

ALTER TABLE bead_references
    ADD PRIMARY KEY (source_bead_id, target_bead_id, source_field);
