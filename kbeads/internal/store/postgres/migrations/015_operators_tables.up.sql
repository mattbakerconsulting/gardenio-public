-- Operators + operator_aliases (Option B schema introduction).
-- Implements docs/adr/human-attribution-identity-v2.md §3.1, §3.2, §4.2 row 015.
-- Umbrella: ka-0000003625 (pivot operated_by from Option A TEXT to Option B FK).
--
-- Phase 1 of the rollout (ADR §6): introduce schema only. The new
-- operated_by_id columns are NULLABLE here; the resolver continues to write
-- the legacy operated_by TEXT column added by migration 012. Backfill,
-- NOT NULL enforcement, and the TEXT-column drop land in 016/017.
--
-- Migration 012 stays in tree untouched (ADR §4.7). Pre-existing values in
-- beads.operated_by / comments.operated_by / deps.operated_by /
-- events.operated_by / mutation_history.operated_by remain authoritative
-- until the backfill admin job and 016 cutover run.

-- operators: one row per known human OR sentinel autonomous identity.
-- See ADR §3.1.
CREATE TABLE operators (
    id          BIGSERIAL PRIMARY KEY,
    kind        TEXT NOT NULL,            -- 'slack', 'oidc', 'gitlab', 'jira', 'system', 'fleet'
    subject     TEXT NOT NULL,            -- canonical subject within kind (slack U-id, OIDC email, …)
    display     TEXT NOT NULL DEFAULT '',
    email       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (kind, subject)
);

-- operator_aliases: every (kind, subject) the system has ever seen for an
-- operator, including the canonical (kind, subject) mirrored from operators.
-- Lookups go alias → operator_id; bead → string predicate goes away in 017.
-- See ADR §3.1.
CREATE TABLE operator_aliases (
    operator_id  BIGINT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    subject      TEXT NOT NULL,
    source       TEXT NOT NULL DEFAULT '', -- 'migration', 'slack-resync', 'oidc-login', 'manual', …
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- A given (kind, subject) maps to at most one operator. Consolidation
    -- (when two humans share an alias) moves the row, never duplicates it.
    PRIMARY KEY (kind, subject)
);

CREATE INDEX idx_operator_aliases_operator_id ON operator_aliases(operator_id);

-- Pre-seed sentinel operators for the known-autonomous identities the
-- resolver writes today (ADR §3.1 table). Each canonical row also gets a
-- mirrored alias so resolver lookups on the alias side find them. The alias
-- source 'migration' marks these as migration-time inserts.
INSERT INTO operators (kind, subject) VALUES
    ('system', 'scheduler'),
    ('system', 'gitlab-sync'),
    ('system', 'gitlab-poller'),
    ('system', 'gitlab-pipeline'),
    ('system', 'gitlab-approval'),
    ('system', 'gitlab-webhook'),
    ('system', 'gitlab-mr-review'),
    ('fleet',  'gardenio');

INSERT INTO operator_aliases (operator_id, kind, subject, source)
SELECT id, kind, subject, 'migration' FROM operators;

-- FK column on every audit-bearing table (ADR §3.2). Initially NULLABLE;
-- 016 enforces NOT NULL after backfill (with §4.5 carve-outs for history-only
-- tables where 'unknown' is a feature).
ALTER TABLE beads             ADD COLUMN operated_by_id BIGINT REFERENCES operators(id);
ALTER TABLE comments          ADD COLUMN operated_by_id BIGINT REFERENCES operators(id);
ALTER TABLE deps              ADD COLUMN operated_by_id BIGINT REFERENCES operators(id);
ALTER TABLE events            ADD COLUMN operated_by_id BIGINT REFERENCES operators(id);
ALTER TABLE mutation_history  ADD COLUMN operated_by_id BIGINT REFERENCES operators(id);

-- Partial indexes mirroring migration 012's plan but on the FK
-- (ADR §3.2). All gated on `operated_by_id IS NOT NULL` so pre-backfill
-- rows do not bloat the partial set.

-- Mirrors idx_beads_operated_by from migration 012.
CREATE INDEX idx_beads_operated_by_id
    ON beads(operated_by_id)
    WHERE operated_by_id IS NOT NULL;

-- Mirrors idx_beads_operated_by_fleet_status from migration 012.
CREATE INDEX idx_beads_operated_by_id_fleet_status
    ON beads(fleet, operated_by_id, status)
    WHERE operated_by_id IS NOT NULL;

-- Mirrors idx_events_operated_by_created_at from migration 012.
CREATE INDEX idx_events_operated_by_id_created_at
    ON events(operated_by_id, created_at)
    WHERE operated_by_id IS NOT NULL;

-- Mirrors idx_mutation_history_operated_by_created_at from migration 012.
CREATE INDEX idx_mutation_history_operated_by_id_created_at
    ON mutation_history(operated_by_id, created_at)
    WHERE operated_by_id IS NOT NULL;

-- D2-equivalent for the dibs hot path (mirrors idx_mutation_history_op_dibs
-- from migration 014 but keyed on the FK; the field-name string the dibs
-- write path uses flips from 'operated_by' to 'operated_by_id' in 016).
CREATE INDEX idx_mutation_history_op_id_dibs
    ON mutation_history(operated_by_id, bead_id)
    WHERE field = 'operated_by_id' AND operated_by_id IS NOT NULL;
