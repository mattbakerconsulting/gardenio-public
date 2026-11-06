package beadsapi

import (
	"encoding/json"
	"sort"
)

// SessionSlowestCallsField is the session-bead field `gb session close` writes
// the per-session top-N slowest tool calls into (plan
// gardenio/plans/agent-tool-latency-attribution.md P4, bead gd-YcmtujOTrC).
// The value is a JSON array of SessionSlowCall, slowest first. It lives on the
// session bead — not in NATS, Postgres or S3 — so every agent and operator can
// read "the 10 slowest commands this session, with their arguments" on any
// cluster, including ones where the S3 archive is off.
const SessionSlowestCallsField = "slowest_calls"

// SessionSlowCall is one entry of a session's slowest-calls list. Args is
// ALWAYS post-redaction (toollatency.RedactText / RedactJSON ran before the
// entry was built): the full Bash command line for Bash, or the compact
// tool_input JSON for every other tool. It is truncated to a fixed byte cap by
// the writer, with TruncatedArgs set so a reader can say so.
type SessionSlowCall struct {
	TS            string `json:"ts,omitempty"` // RFC3339 UTC, when the call finished
	Tool          string `json:"tool"`
	ToolUseID     string `json:"tool_use_id,omitempty"`
	DurationMs    int64  `json:"duration_ms"`
	Source        string `json:"source"` // "claude" (native duration_ms) | "derived" (Pre/Post pairing)
	CmdClass      string `json:"cmd_class,omitempty"`
	Args          string `json:"args,omitempty"`
	TruncatedArgs bool   `json:"truncated_args,omitempty"`
}

// ParseSessionSlowCalls decodes the slowest_calls session field. It returns
// nil for an empty or malformed value (a session closed before this field
// existed, or a hand-edited bead) so callers can treat "absent" and "unusable"
// the same way, and re-sorts slowest-first so a reader never depends on the
// writer's ordering.
func ParseSessionSlowCalls(raw string) []SessionSlowCall {
	if raw == "" {
		return nil
	}
	var calls []SessionSlowCall
	if err := json.Unmarshal([]byte(raw), &calls); err != nil {
		return nil
	}
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].DurationMs > calls[j].DurationMs })
	return calls
}
