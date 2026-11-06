-- Slow-call ledger (plan gardenio/plans/agent-tool-latency-attribution.md P2,
-- bead gd-sWwPnwkZtw). Every agent tool call over a threshold, plus each
-- session's top-N, is written here WHOLE with its full redacted argument
-- vector. Prometheus can hold the bounded per-tool/per-class histograms; it
-- cannot hold "what exactly was slow and what were its arguments" (unbounded,
-- high-entropy text is the worst thing to put in a metric label) — this
-- table is the record plane that answers that question.
--
-- Redaction already happened twice before a row lands here: once in the
-- agent pod (gb hook relay, before the text ever reaches NATS) and again on
-- write (the controller-side HOOK_EVENTS consumer, internal/toollatency) —
-- neither end trusts the other. command_redacted/tool_input_redacted are
-- expected to already be scrubbed; this migration does not and cannot
-- enforce that, it only names the columns.
--
-- Standalone sink, deliberately: the sibling plan
-- (claude-session-subagent-tracking.md P3, "agent_hook_events") proposes a
-- broader lineage/forensics table and was PARKED with nothing implemented.
-- Per this plan's §4.1/§8, if that plan stays parked this table ships and
-- stands on its own; if it lands later, it adopts this one rather than the
-- reverse. Migration policy (kbeads/CLAUDE.md): idempotent DDL only, no
-- backfill — a fresh table, nothing to migrate into it.
CREATE TABLE IF NOT EXISTS slow_tool_calls (
    id                   BIGSERIAL PRIMARY KEY,
    ts                   TIMESTAMPTZ NOT NULL,
    agent_bead_id        TEXT NOT NULL DEFAULT '',
    session_id           TEXT NOT NULL DEFAULT '',
    project              TEXT NOT NULL DEFAULT '',
    role                 TEXT NOT NULL DEFAULT '',
    tool_name            TEXT NOT NULL,
    tool_use_id          TEXT NOT NULL DEFAULT '',
    duration_ms          BIGINT NOT NULL,
    source               TEXT NOT NULL,             -- 'claude' | 'derived'
    cmd_class            TEXT NOT NULL DEFAULT '',
    command_redacted     TEXT NOT NULL DEFAULT '',   -- full Bash argv, secrets scrubbed
    tool_input_redacted  JSONB,                      -- non-Bash tools' real params, scrubbed
    exit_status          INTEGER,
    output_bytes         INTEGER NOT NULL DEFAULT 0,
    is_sidechain         BOOLEAN NOT NULL DEFAULT FALSE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Time-window scans ("slowest calls since X") and the ts-ordered reaper this
-- table will eventually need (plan §P6) both want a plain ts index.
CREATE INDEX IF NOT EXISTS idx_slow_tool_calls_ts ON slow_tool_calls (ts);
-- Per-class drill-down within a window (dashboard's per-cmd_class top-10, P5).
CREATE INDEX IF NOT EXISTS idx_slow_tool_calls_cmd_class_ts ON slow_tool_calls (cmd_class, ts);
-- Per-agent/session drill-down ("this agent's slowest calls").
CREATE INDEX IF NOT EXISTS idx_slow_tool_calls_agent_bead_ts ON slow_tool_calls (agent_bead_id, ts);
-- "The N slowest calls in any window" — the ledger's headline query.
CREATE INDEX IF NOT EXISTS idx_slow_tool_calls_duration_desc ON slow_tool_calls (duration_ms DESC);
