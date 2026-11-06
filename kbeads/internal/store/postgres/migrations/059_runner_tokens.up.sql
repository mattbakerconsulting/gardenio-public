-- Runner bearer tokens (local-agents spec §4.2, plan gd-fwFefkDP4H Task 7
-- gd-zPtKJKzZhE). Mirrors refresh_tokens (011): only the SHA-256 hash of the
-- "gr_..." bearer is stored, revocation is a timestamp, and there is one row
-- per issued token. runner_id is the type=runner bead the token acts for.
CREATE TABLE IF NOT EXISTS runner_tokens (
    token_hash  TEXT PRIMARY KEY,
    runner_id   TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_runner_tokens_runner ON runner_tokens(runner_id);
