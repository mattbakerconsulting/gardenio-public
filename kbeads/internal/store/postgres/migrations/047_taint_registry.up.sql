-- Taint registry: taints, bead_taints, taint_grants, taint_audit.
-- Implements docs/adr/taints.md §3 (phase 1, ka-0000006... umbrella
-- gd-6fmhiIEiLr "Taints: implementation plan"). Schema only — no propagation
-- hooks, no read gate, no CLI. Idempotent DDL, no backfill: a fresh install
-- has no taints (ADR §3 preamble).

-- One row per named taint. The name is an alias; the id is what rows key on,
-- so a rename never orphans anything.
CREATE TABLE IF NOT EXISTS taints (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,          -- e.g. 'acme-production', 'jira-prod'
    description   TEXT NOT NULL DEFAULT '',
    owner_team    TEXT NOT NULL,                 -- type:team bead id (ADR §8)
    scrub_policy  JSONB NOT NULL DEFAULT '{}',   -- ADR §6.2
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at    TIMESTAMPTZ                    -- retired taints still gate reads; they just cannot be newly applied
);

-- Which beads carry which taints, and how they got there.
CREATE TABLE IF NOT EXISTS bead_taints (
    bead_id        TEXT   NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
    taint_id       BIGINT NOT NULL REFERENCES taints(id),
    source         TEXT   NOT NULL,   -- 'explicit' | 'inherit:create' | 'inherit:assign' | 'inherit:read' | 'inherit:write' | 'inherit:parent'
    source_bead_id TEXT,              -- the bead or agent bead the taint flowed from
    applied_by     TEXT   NOT NULL,   -- server-derived subject, never client-supplied
    applied_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (bead_id, taint_id)
);
CREATE INDEX IF NOT EXISTS idx_bead_taints_taint ON bead_taints(taint_id);

-- Who may read beads carrying a taint. principal_kind ∈ {'team','operator','agent','fleet'}.
CREATE TABLE IF NOT EXISTS taint_grants (
    taint_id        BIGINT NOT NULL REFERENCES taints(id),
    principal_kind  TEXT   NOT NULL,
    principal       TEXT   NOT NULL,   -- team bead id, operators.id as text, agent bead id, or fleet name
    granted_by      BIGINT NOT NULL REFERENCES operators(id),
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ,       -- agent grants are always bounded (ADR §5.2)
    PRIMARY KEY (taint_id, principal_kind, principal)
);

-- Hash-only record of every taint mutation. Never holds content.
CREATE TABLE IF NOT EXISTS taint_audit (
    id            BIGSERIAL PRIMARY KEY,
    taint_id      BIGINT NOT NULL REFERENCES taints(id),
    action        TEXT   NOT NULL,   -- 'apply' | 'grant' | 'revoke' | 'scrub-request' | 'scrub-approve' | 'scrub-execute' | 'scrub-reject'
    bead_id       TEXT,
    request_id    TEXT,              -- the scrub-request bead
    actor         TEXT   NOT NULL,   -- server-derived
    approval_mode TEXT,              -- 'auto' | 'human' | 'human-four-eyes'
    content_sha   TEXT,              -- sha256 of the content that was removed, when applicable
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
