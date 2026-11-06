package beadsapi

import (
	"context"
	"fmt"
	"net/http"
)

// GraphStats holds aggregate bead counts by status, mirroring the daemon's
// GET /v1/stats response.
type GraphStats struct {
	TotalOpen       int `json:"total_open"`
	TotalInProgress int `json:"total_in_progress"`
	TotalBlocked    int `json:"total_blocked"`
	TotalClosed     int `json:"total_closed"`
	TotalDeferred   int `json:"total_deferred"`
}

// Total returns the sum across all status counts.
func (s GraphStats) Total() int {
	return s.TotalOpen + s.TotalInProgress + s.TotalBlocked + s.TotalClosed + s.TotalDeferred
}

// GetStats returns aggregate bead counts from GET /v1/stats.
func (c *Client) GetStats(ctx context.Context) (*GraphStats, error) {
	var stats GraphStats
	if err := c.doJSON(ctx, http.MethodGet, "/v1/stats", nil, &stats); err != nil {
		return nil, fmt.Errorf("getting stats: %w", err)
	}
	return &stats, nil
}
