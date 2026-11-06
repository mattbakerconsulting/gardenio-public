-- Server-side idempotency_key dedup for bead creation (ka-0000002856).
--
-- `kd create --idempotency-key <k>` persists the key in beads.fields as
-- fields->>'idempotency_key'. The server does a project-scoped lookup-or-
-- return on create: if a non-deleted bead already carries the key it returns
-- that bead instead of inserting a duplicate. This partial UNIQUE index is
-- the concurrent-insert RACE BACKSTOP for that lookup — two pods replaying the
-- same create simultaneously both miss the pre-create lookup, race into the
-- insert, and exactly one wins; the loser's 23505 is caught server-side and
-- converted into "return the winning bead".
--
-- Scope: per-FLEET. fleet is the only isolation column on the beads row
-- (project lives in the labels table, so it can't participate in a beads-only
-- index). The server's lookup adds the finer per-PROJECT scope on top. Per
-- fleet, the key must be globally unique among live beads — in practice a
-- key is generated per logical operation (e.g. a dwf-followup source_run_id),
-- so cross-project key collisions within one fleet do not occur; if one ever
-- did, the server re-lookup still returns an existing bead (never a 500).
--
-- Soft-deleted beads are excluded (deleted_at IS NULL) so that deleting a
-- keyed bead frees the key for a fresh create — the dedup guard tracks LIVE
-- beads only, matching the default listing semantics (migration 009).
--
-- Migration policy (kbeads/CLAUDE.md): idempotent DDL only
-- (CREATE UNIQUE INDEX IF NOT EXISTS), no table backfill. Runs via
-- `kd migrate` with no daemon-startup deadline. The index is built inline
-- (not CONCURRENTLY) because golang-migrate wraps each migration in a
-- transaction and CREATE INDEX CONCURRENTLY cannot run inside one; on the
-- current bead population this is a sub-second build over the small partial
-- set (only beads carrying the key), consistent with migration 025.

CREATE UNIQUE INDEX IF NOT EXISTS idx_beads_idempotency_key
    ON beads (fleet, (fields->>'idempotency_key'))
    WHERE fields ? 'idempotency_key' AND deleted_at IS NULL;
