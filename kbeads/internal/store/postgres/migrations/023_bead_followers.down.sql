-- Reverse 023_bead_followers.up.sql.
DROP INDEX IF EXISTS idx_bead_followers_forwarded_by;
DROP INDEX IF EXISTS idx_bead_followers_operator_id;
DROP TABLE IF EXISTS bead_followers;
