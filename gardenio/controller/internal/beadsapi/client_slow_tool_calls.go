package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// SlowToolCallRow mirrors model.SlowToolCallRow on the daemon side — one row
// in slow_tool_calls, the P2 record-plane ledger (plan
// gardenio/plans/agent-tool-latency-attribution.md, bead gd-sWwPnwkZtw) for
// agent tool calls over a configurable duration threshold. CommandRedacted
// and ToolInputRedacted must already be scrubbed by the caller before this
// row is sent — the daemon stores them as given, it does not re-redact.
type SlowToolCallRow struct {
	ID                int64           `json:"id,omitempty"`
	TS                time.Time       `json:"ts,omitempty"`
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
	CreatedAt         time.Time       `json:"created_at,omitempty"`
}

// SlowToolCallFilter narrows ListSlowToolCalls. Empty/zero fields mean no
// filter on that dimension; Limit <= 0 lets the daemon apply its default
// page size. Rows come back slowest-first (duration_ms DESC).
type SlowToolCallFilter struct {
	Since       time.Time
	Until       time.Time
	CmdClass    string
	AgentBeadID string
	Limit       int
}

// RecordSlowToolCall POSTs one ledger row to the daemon
// (POST /v1/metrics/slow-tool-calls, RoleOperator).
func (c *Client) RecordSlowToolCall(ctx context.Context, row *SlowToolCallRow) error {
	if row == nil {
		return fmt.Errorf("recording slow tool call: nil row")
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/metrics/slow-tool-calls", row, nil); err != nil {
		return fmt.Errorf("recording slow tool call: %w", err)
	}
	return nil
}

// ListSlowToolCalls fetches ledger rows from the daemon
// (GET /v1/metrics/slow-tool-calls, RoleViewer). Rows come back slowest-first.
func (c *Client) ListSlowToolCalls(ctx context.Context, filter SlowToolCallFilter) ([]*SlowToolCallRow, error) {
	q := url.Values{}
	if !filter.Since.IsZero() {
		q.Set("since", filter.Since.UTC().Format(time.RFC3339))
	}
	if !filter.Until.IsZero() {
		q.Set("until", filter.Until.UTC().Format(time.RFC3339))
	}
	if filter.CmdClass != "" {
		q.Set("cmd_class", filter.CmdClass)
	}
	if filter.AgentBeadID != "" {
		q.Set("agent_bead_id", filter.AgentBeadID)
	}
	if filter.Limit > 0 {
		q.Set("limit", strconv.Itoa(filter.Limit))
	}
	path := "/v1/metrics/slow-tool-calls"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp struct {
		Rows []*SlowToolCallRow `json:"rows"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing slow tool calls: %w", err)
	}
	return resp.Rows, nil
}
