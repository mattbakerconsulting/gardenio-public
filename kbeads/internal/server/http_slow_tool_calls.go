package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"gardenio/kbeads/internal/model"
)

const (
	defaultSlowToolCallsLimit = 50
	maxSlowToolCallsLimit     = 500
)

// slowToolCallBody is the on-the-wire shape POSTed by the controller's
// HOOK_EVENTS consumer (internal/toollatency) for one over-threshold tool
// call. CommandRedacted / ToolInputRedacted are expected to already be
// scrubbed by the caller — the daemon stores them as given, it does not
// re-redact (that layer lives producer-side and consumer-side, not here).
type slowToolCallBody struct {
	TS                string          `json:"ts"`
	AgentBeadID       string          `json:"agent_bead_id,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	Project           string          `json:"project,omitempty"`
	Role              string          `json:"role,omitempty"`
	ToolName          string          `json:"tool_name"`
	ToolUseID         string          `json:"tool_use_id,omitempty"`
	DurationMs        int64           `json:"duration_ms"`
	Source            string          `json:"source"`
	CmdClass          string          `json:"cmd_class,omitempty"`
	CommandRedacted   string          `json:"command_redacted,omitempty"`
	ToolInputRedacted json.RawMessage `json:"tool_input_redacted,omitempty"`
	ExitStatus        *int            `json:"exit_status,omitempty"`
	OutputBytes       int             `json:"output_bytes,omitempty"`
	IsSidechain       bool            `json:"is_sidechain,omitempty"`
}

// handleInsertSlowToolCall handles POST /v1/metrics/slow-tool-calls.
// RoleOperator. Persists one slow-call ledger row. Returns 204 on success.
func (s *BeadsServer) handleInsertSlowToolCall(w http.ResponseWriter, r *http.Request) {
	var body slowToolCallBody
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.ToolName == "" {
		writeError(w, http.StatusBadRequest, "tool_name: required")
		return
	}
	if body.Source != "claude" && body.Source != "derived" {
		writeError(w, http.StatusBadRequest, "source: must be \"claude\" or \"derived\"")
		return
	}
	ts := time.Now().UTC()
	if body.TS != "" {
		parsed, err := time.Parse(time.RFC3339Nano, body.TS)
		if err != nil {
			writeError(w, http.StatusBadRequest, "ts: must be an RFC3339 timestamp")
			return
		}
		ts = parsed
	}
	row := &model.SlowToolCallRow{
		TS:                ts,
		AgentBeadID:       body.AgentBeadID,
		SessionID:         body.SessionID,
		Project:           body.Project,
		Role:              body.Role,
		ToolName:          body.ToolName,
		ToolUseID:         body.ToolUseID,
		DurationMs:        body.DurationMs,
		Source:            body.Source,
		CmdClass:          body.CmdClass,
		CommandRedacted:   body.CommandRedacted,
		ToolInputRedacted: body.ToolInputRedacted,
		ExitStatus:        body.ExitStatus,
		OutputBytes:       body.OutputBytes,
		IsSidechain:       body.IsSidechain,
	}
	if err := s.store.InsertSlowToolCall(r.Context(), row); err != nil {
		writeServerError(r.Context(), w, "handleInsertSlowToolCall", err, "failed to record slow tool call")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListSlowToolCalls handles GET /v1/metrics/slow-tool-calls. RoleViewer.
// Query params: since/until (RFC3339), cmd_class, agent_bead_id, limit
// (default 50, max 500). Response: { "rows": [...] }, slowest-first
// (duration_ms DESC) — this is the ledger's headline "N slowest calls in a
// window" query, not a newest-first feed.
func (s *BeadsServer) handleListSlowToolCalls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := model.SlowToolCallFilter{
		CmdClass:    q.Get("cmd_class"),
		AgentBeadID: q.Get("agent_bead_id"),
		Limit:       defaultSlowToolCallsLimit,
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since: must be an RFC3339 timestamp")
			return
		}
		filter.Since = t
	}
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "until: must be an RFC3339 timestamp")
			return
		}
		filter.Until = t
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit: must be a positive integer")
			return
		}
		if n > maxSlowToolCallsLimit {
			n = maxSlowToolCallsLimit
		}
		filter.Limit = n
	}
	rows, err := s.store.ListSlowToolCalls(r.Context(), filter)
	if err != nil {
		writeServerError(r.Context(), w, "handleListSlowToolCalls", err, "failed to list slow tool calls")
		return
	}
	if rows == nil {
		rows = []*model.SlowToolCallRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}
