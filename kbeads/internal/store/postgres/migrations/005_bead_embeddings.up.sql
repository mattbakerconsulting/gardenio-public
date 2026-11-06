-- pgvector-backed embedding storage for semantic bead search.
-- See kbeads/docs/design-embedding-search.md section 1 for rationale.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE bead_embeddings (
    bead_id    TEXT PRIMARY KEY REFERENCES beads(id) ON DELETE CASCADE,
    embedding  vector(1024),
    model      TEXT NOT NULL,
    text_hash  TEXT NOT NULL,
    fleet      TEXT NOT NULL DEFAULT 'default',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- IVFFlat index — good enough for <100k rows, easy to tune.
-- Switch to HNSW if we exceed ~500k beads per fleet.
CREATE INDEX idx_bead_embeddings_ivfflat
    ON bead_embeddings USING ivfflat (embedding vector_cosine_ops)
    WITH (lists = 100);

CREATE INDEX idx_bead_embeddings_fleet ON bead_embeddings(fleet);
