-- Add fleet column to all tables for multi-namespace isolation.
-- Default 'default' backfills existing rows automatically.

ALTER TABLE beads ADD COLUMN fleet TEXT NOT NULL DEFAULT 'default';
CREATE INDEX idx_beads_fleet_status_type ON beads(fleet, status, type);

ALTER TABLE labels ADD COLUMN fleet TEXT NOT NULL DEFAULT 'default';
CREATE INDEX idx_labels_fleet ON labels(fleet);

ALTER TABLE deps ADD COLUMN fleet TEXT NOT NULL DEFAULT 'default';
CREATE INDEX idx_deps_fleet ON deps(fleet);

ALTER TABLE comments ADD COLUMN fleet TEXT NOT NULL DEFAULT 'default';
CREATE INDEX idx_comments_fleet ON comments(fleet);

ALTER TABLE events ADD COLUMN fleet TEXT NOT NULL DEFAULT 'default';
CREATE INDEX idx_events_fleet ON events(fleet);

ALTER TABLE configs ADD COLUMN fleet TEXT NOT NULL DEFAULT 'default';
CREATE INDEX idx_configs_fleet ON configs(fleet);
