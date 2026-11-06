-- Append-only audit log of bead mutations. Records per-field changes
-- (create/update/delete) so operators can reconstruct change history.
-- Distinct from the existing audit-bead system in audit_log.go, which
-- records authorization-denial events as full beads.

CREATE TABLE mutation_history (
    id            BIGSERIAL PRIMARY KEY,
    bead_id       TEXT NOT NULL,
    actor         TEXT NOT NULL DEFAULT '',
    field         TEXT NOT NULL,
    old_value     TEXT NOT NULL DEFAULT '',
    new_value     TEXT NOT NULL DEFAULT '',
    mutation_type TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_mutation_history_bead_id ON mutation_history(bead_id, created_at);
