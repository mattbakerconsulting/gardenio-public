-- heraldry: a bead's arms, one to five emoji on the five places of a shield,
-- stored in the canonical form (five space-separated tokens, "." for an empty
-- place). Spec gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md
-- §3.1, step 1.1 gd-pTtoEKamiI. Nullable and optional (R1): no backfill, and
-- the server validates the value (kbeads/internal/heraldry.Parse).
--
-- project: the bead's project, copied from its project:<x> label so an index
-- can scope by it (the project otherwise lives only in the labels table). The
-- store rewrites it whenever a project: label is added or removed and on every
-- bead update; it holds the project when the bead has exactly one project:
-- label and NULL otherwise. It is the register key and nothing else reads it.
-- Legacy rows stay NULL until their next write: a bead can only gain arms
-- through a write, and that write sets project, so every bead with arms has it.
--
-- The register (R5): arms are unique within a project among live beads, closed
-- ones included (arms are not released at close). The spec's draft keyed this
-- index on (fleet, project, heraldry), as idempotency_key is; it is keyed on
-- (project, heraldry) because fleet is audit attribution only and one project's
-- beads span many fleets (agent-gd-…, default), so a fleet-scoped index would
-- let two beads of one project bear the same arms. The server checks the
-- register first for a 409 naming the bead that bears them; this index is the
-- race backstop (its 23505 is mapped to the same 409).
--
-- Idempotent DDL only (kbeads/CLAUDE.md migration policy). The index is built
-- inline over a partial set that is empty at deploy.
ALTER TABLE beads ADD COLUMN IF NOT EXISTS heraldry TEXT;
ALTER TABLE beads ADD COLUMN IF NOT EXISTS project TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_beads_heraldry_register
    ON beads (project, heraldry)
    WHERE heraldry IS NOT NULL AND deleted_at IS NULL;
