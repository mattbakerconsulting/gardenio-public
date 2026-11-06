package beadsapi

import (
	"context"
	"fmt"
	"net/http"
)

// EmitHookRequest is the request body for POST /v1/hooks/emit.
type EmitHookRequest struct {
	AgentBeadID     string `json:"agent_bead_id"`
	HookType        string `json:"hook_type"` // Stop, PreToolUse, SessionStart, UserPromptSubmit, PreCompact, PostCompact
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	// SessionID is the harness-neutral session id (Claude session or Codex
	// thread id); Harness is "claude" or "codex". ClaudeSessionID stays
	// populated for daemons that predate the neutral field.
	SessionID string `json:"session_id,omitempty"`
	Harness   string `json:"harness,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Actor     string `json:"actor,omitempty"`
	ToolName  string `json:"tool_name,omitempty"` // e.g. "Bash", "Read" (for PreToolUse)
}

// EmitHookResponse is the response from POST /v1/hooks/emit.
type EmitHookResponse struct {
	Block    bool     `json:"block"`
	Reason   string   `json:"reason,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Inject   string   `json:"inject,omitempty"`
}

// EmitHook sends a hook event to the daemon for gate evaluation and soft checks.
func (c *Client) EmitHook(ctx context.Context, req EmitHookRequest) (*EmitHookResponse, error) {
	var resp EmitHookResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/hooks/emit", req, &resp); err != nil {
		return nil, fmt.Errorf("emitting hook: %w", err)
	}
	return &resp, nil
}
