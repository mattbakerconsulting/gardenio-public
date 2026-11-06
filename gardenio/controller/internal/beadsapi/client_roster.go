package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// RosterActor represents one agent in the roster.
type RosterActor struct {
	Actor     string  `json:"actor"`
	TaskID    string  `json:"task_id"`
	TaskTitle string  `json:"task_title"`
	EpicID    string  `json:"epic_id"`
	EpicTitle string  `json:"epic_title"`
	IdleSecs  float64 `json:"idle_secs"`
	LastEvent string  `json:"last_event"`
	ToolName  string  `json:"tool_name"`
	SessionID string  `json:"session_id"`
	CWD       string  `json:"cwd"`
	Reaped    bool    `json:"reaped"`
}

// UnclaimedTask represents an unclaimed task in the roster response.
type UnclaimedTask struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Priority int    `json:"priority"`
}

// AgentRosterResponse is the response from GET /v1/agents/roster.
type AgentRosterResponse struct {
	Actors         []RosterActor   `json:"actors"`
	UnclaimedTasks []UnclaimedTask `json:"unclaimed_tasks"`
}

// GetAgentRoster returns the live agent roster from the daemon.
// staleThresholdSecs controls how long since last event before an agent is
// considered stale (0 uses server default).
func (c *Client) GetAgentRoster(ctx context.Context, staleThresholdSecs int) (*AgentRosterResponse, error) {
	q := url.Values{}
	if staleThresholdSecs > 0 {
		q.Set("stale_threshold_secs", strconv.Itoa(staleThresholdSecs))
	}
	path := "/v1/agents/roster"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var resp AgentRosterResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting agent roster: %w", err)
	}
	return &resp, nil
}
