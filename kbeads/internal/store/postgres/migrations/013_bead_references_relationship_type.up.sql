-- Typed mention syntax (epic ka-0000003840): authors can declare *what kind*
-- of relationship a `ka-XXX` text reference represents (parent, blocks, …).
-- Untyped mentions stay rows with relationship_type = NULL (current behavior);
-- typed mentions populate it. See research-typed-mention-syntax.md § 4.2 for
-- the storage-model decision (extend bead_references rather than materialize
-- into deps) and § 10.1 for the schema shape.

ALTER TABLE bead_references
    ADD COLUMN relationship_type TEXT;

-- The original PK was (source_bead_id, target_bead_id, source_field). Two
-- typed mentions of the same target from the same field — e.g. a description
-- that contains both `parent(ka-X)` and `blocks(ka-X)` — must coexist as
-- distinct rows. Replace the PK with a unique index that includes
-- relationship_type. Postgres treats NULL as not-equal even within a unique
-- constraint, so we COALESCE to '' to give untyped rows a stable key (and
-- keep the original "one untyped row per (source, target, field)" invariant).
ALTER TABLE bead_references
    DROP CONSTRAINT bead_references_pkey;

CREATE UNIQUE INDEX bead_references_unique
    ON bead_references (
        source_bead_id,
        target_bead_id,
        source_field,
        COALESCE(relationship_type, '')
    );

-- Reverse lookup of typed mentions: "who mentions ka-X as a parent?"
-- Partial — untyped rows already have idx_bead_references_target.
CREATE INDEX idx_bead_references_target_type
    ON bead_references (target_bead_id, relationship_type)
    WHERE relationship_type IS NOT NULL;
