-- Persistent store for revoked auth-token SHA-256 hashes. Previously kept in
-- memory only (seeded from BEADS_REVOKED_TOKENS at startup), which meant a
-- runtime revocation was lost on daemon restart. Rows here are consulted by
-- every request via an in-process cache populated at startup and kept in sync
-- when new rows are inserted through the admin revoke-token endpoint.

CREATE TABLE revoked_tokens (
    token_hash  TEXT PRIMARY KEY,
    revoked_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_by  TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_revoked_tokens_revoked_at ON revoked_tokens(revoked_at DESC);
