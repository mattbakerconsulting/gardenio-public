-- Reverse of 042_fields_mr_url_unique_index.up.sql: drop the UNIQUE index and
-- restore migration 041's NON-UNIQUE partial index (same name + predicate as
-- 041) so a down-migration lands on 041's exact schema state.

DROP INDEX IF EXISTS idx_beads_fields_mr_url_unique;

CREATE INDEX IF NOT EXISTS idx_beads_fields_mr_url
    ON beads ((fields->>'mr_url'))
    WHERE type = 'merge-request' AND fields ? 'mr_url';
