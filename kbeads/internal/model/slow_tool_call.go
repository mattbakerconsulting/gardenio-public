package model

import (
	"encoding/json"
	"time"
)

// SlowToolCallRow is one row in slow_tool_calls — the P2 record-plane ledger
// (plan gardenio/plans/agent-tool-latency-attribution.md, bead gd-sWwPnwkZtw)
// for agent tool calls over a configurable duration threshold. Prometheus
// (the metric plane) holds bounded per-tool/per-cmd_class histograms; this
// row holds the FULL redacted argument vector for the calls that actually
// mattered, so an operator can answer "what exactly was slow" without
// unbounded text ever touching a metric label.
//
// CommandRedacted and ToolInputRedacted are expected to already be scrubbed
// by the caller (defense in depth: once at the producer in the agent pod,
// once again by the HOOK_EVENTS consumer before this insert) — this type
// carries the post-redaction values, it does not itself redact.
type SlowToolCallRow struct {
	ID                int64           `json:"id"`
	TS                time.Time       `json:"ts"`
	AgentBeadID       string          `json:"agent_bead_id,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	Project           string          `json:"project,omitempty"`
	Role              string          `json:"role,omitempty"`
	ToolName          string          `json:"tool_name"`
	ToolUseID         string          `json:"tool_use_id,omitempty"`
	DurationMs        int64           `json:"duration_ms"`
	Source            string          `json:"source"` // "claude" | "derived"
	CmdClass          string          `json:"cmd_class,omitempty"`
	CommandRedacted   string          `json:"command_redacted,omitempty"`
	ToolInputRedacted json.RawMessage `json:"tool_input_redacted,omitempty"`
	ExitStatus        *int            `json:"exit_status,omitempty"`
	OutputBytes       int             `json:"output_bytes,omitempty"`
	IsSidechain       bool            `json:"is_sidechain,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

// SlowToolCallFilter narrows ListSlowToolCalls. Since/Until bound the ts
// window (zero = no bound on that side); CmdClass and AgentBeadID are exact-
// match filters (empty = no filter). Rows come back ordered by duration_ms
// DESCENDING — the ledger's headline query is "the N slowest calls in this
// window", not "the newest calls" — capped by Limit (0 = implementation
// default).
type SlowToolCallFilter struct {
	Since       time.Time
	Until       time.Time
	CmdClass    string
	AgentBeadID string
	Limit       int
}
