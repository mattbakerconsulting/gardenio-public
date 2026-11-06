-- Perf (gd-qCPRRNtm4M): index the agent-by-task lookup.
--
-- An agent bead's fields.task_id names the step it claimed. Callers find a
-- step's crews with `type = 'agent' AND fields ? 'task_id' AND
-- fields->>'task_id' = $1`, closed agents included (gardenio
-- internal/gardenhub readCrews for the harvest cell, gb prime's prior work,
-- the task-predecessor and bridge catch-up reads). Nothing indexes task_id,
-- so each lookup scans every agent bead. Measured 2026-11-02 18:10Z on the
-- viewer's per-site histogram: readCrews mean 4.99 s, p99 9.9 s.
--
-- Partial B-tree expression index, same shape and rationale as 037
-- (thread_id), 055 (channel_id) and 062 (subject_id). Idempotent DDL only,
-- not CONCURRENTLY (matches 062).

CREATE INDEX IF NOT EXISTS idx_beads_fields_task_id
    ON beads ((fields->>'task_id'))
    WHERE fields ? 'task_id';

ANALYZE beads;
