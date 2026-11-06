package server

import (
	"net/http"

	"gardenio/kbeads/internal/store"
)

// RepairReversedParentChildDepsRequest is the JSON body for
// POST /v1/admin/repair-reversed-parent-child-deps. Drives one run of
// ka-0000006880's reversed-edge audit/repair over the deps table.
type RepairReversedParentChildDepsRequest struct {
	BatchSize  int  `json:"batch_size,omitempty"`
	MaxBatches int  `json:"max_batches,omitempty"`
	DryRun     bool `json:"dry_run,omitempty"`
}

// RepairReversedParentChildDepsResponse mirrors
// store.RepairReversedParentChildDepsStats over the wire.
type RepairReversedParentChildDepsResponse struct {
	DryRun                 bool                            `json:"dry_run"`
	ReversedRows           int64                           `json:"reversed_rows"`
	EdgesFlipped           int64                           `json:"edges_flipped"`
	SkippedCanonicalExists int64                           `json:"skipped_canonical_exists"`
	Edges                  []store.ReversedParentChildEdge `json:"edges,omitempty"`
	Batches                int                             `json:"batches"`
	BatchesCapped          bool                            `json:"batches_capped,omitempty"`
	DurationMs             int64                           `json:"duration_ms"`
}

// handleRepairReversedParentChildDeps handles
// POST /v1/admin/repair-reversed-parent-child-deps. Admin-only.
func (s *BeadsServer) handleRepairReversedParentChildDeps(w http.ResponseWriter, r *http.Request) {
	var req RepairReversedParentChildDepsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.RepairReversedParentChildDeps(r.Context(), store.RepairReversedParentChildDepsOptions{
		BatchSize:  req.BatchSize,
		MaxBatches: req.MaxBatches,
		DryRun:     req.DryRun,
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleRepairReversedParentChildDeps", err, "failed to repair reversed parent-child deps")
		return
	}
	writeJSON(w, http.StatusOK, &RepairReversedParentChildDepsResponse{
		DryRun:                 stats.DryRun,
		ReversedRows:           stats.ReversedRows,
		EdgesFlipped:           stats.EdgesFlipped,
		SkippedCanonicalExists: stats.SkippedCanonicalExists,
		Edges:                  stats.Edges,
		Batches:                stats.Batches,
		BatchesCapped:          stats.BatchesCapped,
		DurationMs:             stats.DurationMs,
	})
}
