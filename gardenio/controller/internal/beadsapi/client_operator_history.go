package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// OperatorHistoryRow mirrors one row from the kbeads bead_operator_history
// VIEW (R4 Option C, migration 014). The VIEW projects mutation_history
// rows where field='operated_by' and adds a CASE-derived source_kind
// classifier ("create" / "refresh" / "sentinel-attempt" /
// "unknown-attempt" / "dibs-flip"). See
// kbeads/internal/store/postgres/migrations/014_operated_by_dibs.up.sql
// and gardenio/docs/research/operated-by-transitivity-r4-audit.md §4 / §7.
type OperatorHistoryRow struct {
	ID            int64     `json:"id"`
	BeadID        string    `json:"bead_id"`
	OperatorID    string    `json:"operator_id"`
	BecameOwnerAt time.Time `json:"became_owner_at"`
	PrevOperator  string    `json:"prev_operator"`
	Actor         string    `json:"actor"`
	SourceKind    string    `json:"source_kind"`
}

// GetOperatorHistory fetches the operator-ownership timeline for a bead from
// the daemon. Wraps GET /v1/beads/{id}/operator-history; rows are returned
// newest-first (became_owner_at DESC, id DESC).
func (c *Client) GetOperatorHistory(ctx context.Context, beadID string) ([]OperatorHistoryRow, error) {
	var resp struct {
		OperatorHistory []OperatorHistoryRow `json:"operator_history"`
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/operator-history"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting operator history for %s: %w", beadID, err)
	}
	return resp.OperatorHistory, nil
}
