-- Persistent store for issued refresh tokens. Mirrors the revoked_tokens
-- pattern (table 007) — only the SHA-256 hash is stored. Refresh tokens
-- carry a longer TTL than access tokens (default 30d) and are used to mint
-- new short-lived access tokens via POST /v1/auth/refresh. Revoking a
-- refresh token (revoked_at set) invalidates any access tokens issued from
-- it; the rotation chain is preserved via parent_refresh_hash so a single
-- revocation can cascade to descendants (token-replay detection).

CREATE TABLE IF NOT EXISTS refresh_tokens (
    token_hash           TEXT PRIMARY KEY,
    fleet                TEXT NOT NULL,
    project              TEXT NOT NULL DEFAULT '',
    scopes_json          JSONB NOT NULL DEFAULT '{}'::jsonb,
    subject              TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at           TIMESTAMPTZ NOT NULL,
    revoked_at           TIMESTAMPTZ,
    parent_refresh_hash  TEXT
);

CREATE INDEX IF NOT EXISTS idx_refresh_tokens_fleet ON refresh_tokens(fleet);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_expires_at ON refresh_tokens(expires_at);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_parent ON refresh_tokens(parent_refresh_hash);
