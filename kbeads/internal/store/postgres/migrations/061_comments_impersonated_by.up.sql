-- Writes made while impersonating (plan gd-0SUIdKXWV3 step 5, gd-L8LeYDLACc):
-- the admin behind an impersonated session that added the comment, stamped by
-- the daemon from the verified token's impersonator claim
-- (internal/server/impersonation.go). '' for every other comment. A constant
-- default makes this a catalog-only change on Postgres 11+, with no table
-- rewrite and no backfill.
ALTER TABLE comments ADD COLUMN IF NOT EXISTS impersonated_by TEXT NOT NULL DEFAULT '';
