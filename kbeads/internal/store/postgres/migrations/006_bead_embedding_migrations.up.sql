-- Audit log for embedding model / dimension migrations. One row per
-- `kd embed migrate` invocation. Used to answer "when did we last change
-- the embedding space and to what?" without mining event logs.

CREATE TABLE bead_embedding_migrations (
    id          BIGSERIAL PRIMARY KEY,
    model       TEXT NOT NULL,
    dims        INTEGER NOT NULL,
    actor       TEXT NOT NULL DEFAULT '',
    note        TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_bead_embedding_migrations_created_at
    ON bead_embedding_migrations(created_at DESC);
