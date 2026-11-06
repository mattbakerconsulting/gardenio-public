package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// GateRow represents a session gate from the daemon.
type GateRow struct {
	AgentBeadID string     `json:"agent_bead_id"`
	GateID      string     `json:"gate_id"`
	Status      string     `json:"status"` // "pending" or "satisfied"
	SatisfiedAt *time.Time `json:"satisfied_at,omitempty"`
}

// ListGates returns all gates for the given agent bead.
func (c *Client) ListGates(ctx context.Context, agentBeadID string) ([]GateRow, error) {
	var resp []GateRow
	path := "/v1/agents/" + url.PathEscape(agentBeadID) + "/gates"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing gates for %s: %w", agentBeadID, err)
	}
	return resp, nil
}

// SatisfyGate marks a gate as satisfied.
func (c *Client) SatisfyGate(ctx context.Context, agentBeadID, gateID string) error {
	path := "/v1/agents/" + url.PathEscape(agentBeadID) + "/gates/" + url.PathEscape(gateID) + "/satisfy"
	if err := c.doJSON(ctx, http.MethodPost, path, nil, nil); err != nil {
		return fmt.Errorf("satisfying gate %s for %s: %w", gateID, agentBeadID, err)
	}
	return nil
}

// ClearGate resets a gate to pending.
func (c *Client) ClearGate(ctx context.Context, agentBeadID, gateID string) error {
	path := "/v1/agents/" + url.PathEscape(agentBeadID) + "/gates/" + url.PathEscape(gateID)
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return fmt.Errorf("clearing gate %s for %s: %w", gateID, agentBeadID, err)
	}
	return nil
}
