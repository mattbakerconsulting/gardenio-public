-- bead_followers: many-to-many "I want this bead in your inbox" join table.
-- Tracking bead: ka-0000004114 (parent ka-0000002474, related ka-0000007385).
--
-- A bead's `operated_by_id` is single-valued — at most one operator owns a
-- bead at a time. Followers are additive and many-to-many: forwarding a bead
-- does NOT remove it from anywhere it currently shows up. The inbox query
-- unions `operated_by_id = $me` with `id IN (SELECT bead_id FROM bead_followers
-- WHERE operator_id = $me)`.
--
-- Reasons for a join table over a `followed_by BIGINT[]` column on `beads`:
--   - Plays nicely with the operator FK shape introduced in migration 015
--     and finalized in 016/017 (no array → FK pivot to redo).
--   - Lets us record `forwarded_by` and an optional `note` cheaply.
--   - Indexable by operator without GIN.
--
-- Idempotent DDL so a partial-apply can be retried without manual cleanup.

CREATE TABLE IF NOT EXISTS bead_followers (
    bead_id      TEXT        NOT NULL REFERENCES beads(id)     ON DELETE CASCADE,
    operator_id  BIGINT      NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    forwarded_by BIGINT               REFERENCES operators(id), -- who put it there (NULL = system / pre-resolver)
    note         TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (bead_id, operator_id)
);

-- Lookup by operator drives the inbox UNION predicate
-- (`WHERE operator_id = $me`). The PK already covers (bead_id, operator_id);
-- this index covers the inverse direction.
CREATE INDEX IF NOT EXISTS idx_bead_followers_operator_id
    ON bead_followers(operator_id);

-- Forwarder-attribution lookups and "who did I forward to" reverse joins.
-- Partial so rows with NULL forwarded_by (e.g. system inserts) do not bloat
-- the index.
CREATE INDEX IF NOT EXISTS idx_bead_followers_forwarded_by
    ON bead_followers(forwarded_by)
    WHERE forwarded_by IS NOT NULL;
