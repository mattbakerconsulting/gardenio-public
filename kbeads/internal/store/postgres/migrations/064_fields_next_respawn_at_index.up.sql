-- Perf (gd-lCYkpAlNas, gd-4uUkp4DQFD): index the parked-agent wake scan.
--
-- The controller wake scanner (gardenio internal/statusreporter
-- wake_scanner.go) looks for closed agent beads whose wake is stamped:
-- `status = 'closed' AND type = 'agent' AND fields ? 'next_respawn_at' AND
-- fields->>'next_respawn_at' != ''` (the `field=next_respawn_at=*` presence
-- filter). Nothing indexes next_respawn_at, so every pass reads the fields
-- of every closed agent bead. Measured 2026-11-05 08:30Z: ~15.5k closed
-- agents, 0 carrying next_respawn_at; the count over them takes ~2.5 s.
--
-- Partial B-tree expression index, same shape and rationale as 037
-- (thread_id), 055 (channel_id), 062 (subject_id) and 063 (task_id). Only
-- parked beads carry the key, so the index stays tiny. Idempotent DDL only,
-- not CONCURRENTLY (matches 063).

CREATE INDEX IF NOT EXISTS idx_beads_fields_next_respawn_at
    ON beads ((fields->>'next_respawn_at'))
    WHERE fields ? 'next_respawn_at';

ANALYZE beads;
