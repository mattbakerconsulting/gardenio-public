-- Edges derived from ka-0000001713 IDs that authors paste into bead text
-- (description, notes, title, individual comments). Stored separately from
-- `deps` so derived state can be rebuilt from text without disturbing
-- user-entered dependencies, and so multiple source fields can reference
-- the same target without collapsing under deps' (bead_id, depends_on_id,
-- type) primary key. See kbeads/docs/design-bead-references.md § 1.

CREATE TABLE bead_references (
    source_bead_id  TEXT NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
    target_bead_id  TEXT NOT NULL,
    source_field    TEXT NOT NULL,
    extracted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (source_bead_id, target_bead_id, source_field)
);

-- target_bead_id is intentionally NOT a foreign key: dangling references
-- (typos, cross-fleet IDs) are valid signal we want to preserve.
CREATE INDEX idx_bead_references_target ON bead_references (target_bead_id);
CREATE INDEX idx_bead_references_source ON bead_references (source_bead_id);
