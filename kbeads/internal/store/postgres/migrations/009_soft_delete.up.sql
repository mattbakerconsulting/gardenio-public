-- Soft-delete: beads marked deleted_at instead of being removed. Default
-- retention is 30d (enforced by a future purge job, not in this migration).
-- Listings hide deleted rows by default; admins can opt back in via the
-- include_deleted flag. `kd restore <id>` clears deleted_at.

ALTER TABLE beads ADD COLUMN deleted_at TIMESTAMPTZ;

-- Partial index supports retention sweeps (`WHERE deleted_at < cutoff`)
-- without bloating the common-case (deleted_at IS NULL) lookups.
CREATE INDEX idx_beads_deleted_at ON beads(deleted_at) WHERE deleted_at IS NOT NULL;
