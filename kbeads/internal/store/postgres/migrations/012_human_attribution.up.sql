-- Human attribution ("operated_by"): the human who caused each bead-related
-- mutation to exist, distinct from the agent that performed the write
-- (which lives in created_by / actor / author). Server-set from a future
-- operatedByFromContext resolver (R3, ka-0000005586); the column itself
-- mirrors the existing TEXT-default-empty audit-column convention.
--
-- Format: '<provider>:<subject>'.
--   slack:U0OPERATOR2   — Slack user ID
--   oidc:matt@acme.example — OIDC subject (email) for direct-CLI callers
--   system:scheduler    — sentinel: known-autonomous (cron-driven agent)
--   fleet:gardenio       — sentinel: fleet-token / controller-internal mutation
--   ''                  — pre-cutover or unresolved; R7 backfill narrows this
--                         set; new writes should never be empty.
--
-- Schema only — no reads/writes against the new columns yet. Behavior is
-- gated by BEADS_OPERATED_BY_ENABLED (R9) in subsequent phases.
--
-- See docs/research/human-attribution-schema.md for index justification and
-- nullable-semantics rationale (R4, ka-0000002055); R10 epic ka-0000001133
-- approves this shape.

ALTER TABLE beads
    ADD COLUMN operated_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN operated_by_display TEXT;

ALTER TABLE comments
    ADD COLUMN operated_by TEXT NOT NULL DEFAULT '';

ALTER TABLE deps
    ADD COLUMN operated_by TEXT NOT NULL DEFAULT '';

ALTER TABLE events
    ADD COLUMN operated_by TEXT NOT NULL DEFAULT '';

ALTER TABLE mutation_history
    ADD COLUMN operated_by TEXT NOT NULL DEFAULT '';

-- I1: primary filter for `kd list -f operated_by=...` and beads-viewer.
-- Partial keeps the index small while pre-backfill rows linger.
CREATE INDEX idx_beads_operated_by
    ON beads(operated_by)
    WHERE operated_by <> '';

-- I2: composite for the dashboard query
--   "what's open in this fleet, operated by me?"
-- Mirrors idx_beads_fleet_status_type (004_add_fleet_column.up.sql:5).
CREATE INDEX idx_beads_operated_by_fleet_status
    ON beads(fleet, operated_by, status)
    WHERE operated_by <> '';

-- I3: per-operator audit queries on the event log.
CREATE INDEX idx_events_operated_by_created_at
    ON events(operated_by, created_at)
    WHERE operated_by <> '';

-- I4: per-operator audit queries on per-field mutation history.
CREATE INDEX idx_mutation_history_operated_by_created_at
    ON mutation_history(operated_by, created_at)
    WHERE operated_by <> '';
