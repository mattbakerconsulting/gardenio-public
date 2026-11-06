-- Reverse of 017_operators_cutover.up.sql. Restores NULLABLE operated_by_id
-- on the live-state tables, restores the 014 view shape (TEXT-keyed
-- projection over field='operated_by'), and recreates the legacy
-- idx_mutation_history_op_dibs partial index that 017 dropped.
--
-- The FK-keyed idx_mutation_history_op_id_dibs (added by 015) is left in
-- place — it lives across this rollback because 015 owns it.

ALTER TABLE beads    ALTER COLUMN operated_by_id DROP NOT NULL;
ALTER TABLE comments ALTER COLUMN operated_by_id DROP NOT NULL;
ALTER TABLE deps     ALTER COLUMN operated_by_id DROP NOT NULL;

DROP VIEW IF EXISTS bead_operator_history;

-- Recreate the 014 projection verbatim.
CREATE VIEW bead_operator_history AS
SELECT
    id,
    bead_id,
    operated_by AS operator_id,
    created_at AS became_owner_at,
    old_value AS prev_operator,
    actor,
    CASE
        WHEN mutation_type = 'create'                                  THEN 'create'
        WHEN operated_by = old_value                                   THEN 'refresh'
        WHEN operated_by LIKE 'system:%' OR operated_by LIKE 'fleet:%' THEN 'sentinel-attempt'
        WHEN operated_by = ''                                          THEN 'unknown-attempt'
        ELSE                                                                'dibs-flip'
    END AS source_kind
FROM mutation_history
WHERE field = 'operated_by';

-- Recreate the legacy TEXT-keyed dibs partial index.
CREATE INDEX idx_mutation_history_op_dibs
    ON mutation_history(operated_by, bead_id)
    WHERE field = 'operated_by' AND operated_by <> '';
