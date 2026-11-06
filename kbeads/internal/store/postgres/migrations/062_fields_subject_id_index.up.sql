-- Perf (gd-XOi5cqZP3I): index the Commons card lookup by subject.
--
-- The Commons finds a bead's cards with
-- `type = 'message' AND fields ? 'kind' AND fields->>'kind' = 'card'
--  AND fields ? 'subject_id' AND fields->>'subject_id' = $1`
-- (gardenio internal/commons: the hub's subjectCards on every agent,
-- decision, permission or escalation change; locateCard behind
-- GET /garden/c/bead/{id}; cardOf for permission cards). Nothing indexes
-- subject_id, so each lookup scans every message bead. Measured 2026-11-02
-- from the daemon's slow-request log (09:00Z, 37 min): this one shape was 236
-- of the beads-viewer's 407 list calls over 1 s, mean 2.19 s, 517 of its 868
-- slow seconds, and 67 of them were cancelled at the viewer's 3 s lookup
-- timeout (503/500, pq 57014). Most subjects have no card, so the answer is
-- usually empty after a full scan.
--
-- Partial B-tree expression index, same shape and rationale as 037
-- (thread_id) and 055 (channel_id): single-key equality on a sparse JSON key
-- that only card and notice messages carry. Idempotent DDL only, not CONCURRENTLY
-- (matches 055).

CREATE INDEX IF NOT EXISTS idx_beads_fields_subject_id
    ON beads ((fields->>'subject_id'))
    WHERE fields ? 'subject_id';

ANALYZE beads;
