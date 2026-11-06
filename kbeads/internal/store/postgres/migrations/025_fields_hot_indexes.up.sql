-- Task 3 / epic ka-0000003045: add expression indexes on hot beads.fields
-- JSONB keys (agent, agent_state, project, role, slack_user_id).
--
-- Bead: ka-0000005120.
--
-- beads.fields was declared as a plain JSONB column in migration 001 with no
-- GIN or expression index. Daemon-side filters like fields->>'agent' = $1
-- therefore degrade to a sequential scan, which on a busy fleet means
-- scanning thousands of beads per request (Task 1 will route by-name agent
-- lookups to the daemon — that query plan must be O(log n), not O(n)).
--
-- We index the keys we filter on today as PARTIAL B-tree expression
-- indexes — the index size scales with the bead population actually
-- carrying each key (e.g. only agent-kind beads have 'agent', only
-- Slack-attributed beads have 'slack_user_id'), and equality lookups
-- (`fields->>'agent' = $1`) are a textbook B-tree fit.
--
-- We do NOT install a GIN(fields jsonb_path_ops) index here: every hot
-- filter is a single-key equality, which B-tree expression indexes serve
-- both smaller and faster than GIN. GIN remains a viable add later if an
-- ad-hoc containment query surfaces; landing it now is speculative
-- footprint we don't need.
--
-- Migration policy: idempotent DDL only (CREATE INDEX IF NOT EXISTS), no
-- table backfill, runs via `kd migrate` (no daemon-startup deadline).

CREATE INDEX IF NOT EXISTS idx_beads_fields_agent
    ON beads ((fields->>'agent'))
    WHERE fields ? 'agent';

CREATE INDEX IF NOT EXISTS idx_beads_fields_agent_state
    ON beads ((fields->>'agent_state'))
    WHERE fields ? 'agent_state';

CREATE INDEX IF NOT EXISTS idx_beads_fields_project
    ON beads ((fields->>'project'))
    WHERE fields ? 'project';

CREATE INDEX IF NOT EXISTS idx_beads_fields_role
    ON beads ((fields->>'role'))
    WHERE fields ? 'role';

CREATE INDEX IF NOT EXISTS idx_beads_fields_slack_user_id
    ON beads ((fields->>'slack_user_id'))
    WHERE fields ? 'slack_user_id';
