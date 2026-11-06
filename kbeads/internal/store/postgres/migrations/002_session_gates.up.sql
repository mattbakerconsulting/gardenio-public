CREATE TABLE session_gates (
    agent_bead_id    TEXT        NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
    gate_id          TEXT        NOT NULL,
    status           TEXT        NOT NULL DEFAULT 'pending',
    satisfied_at     TIMESTAMPTZ,
    claude_session_id TEXT,
    metadata         JSONB,
    PRIMARY KEY (agent_bead_id, gate_id)
);
