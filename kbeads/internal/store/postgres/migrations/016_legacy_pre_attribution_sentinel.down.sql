-- Reverse of 016_legacy_pre_attribution_sentinel.up.sql.
-- Removes the system:legacy-pre-attribution sentinel operator and its
-- canonical alias. Safe even if FK rows still reference the sentinel because
-- the up migration only inserts the row; downstream backfill rebinds happen
-- in the P3 admin command (separately reversible by re-running the backfill
-- after re-applying the up migration).

DELETE FROM operator_aliases
 WHERE kind = 'system' AND subject = 'legacy-pre-attribution';

DELETE FROM operators
 WHERE kind = 'system' AND subject = 'legacy-pre-attribution';
