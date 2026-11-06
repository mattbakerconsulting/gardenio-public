package server

import (
	"net/http"

	"gardenio/kbeads/internal/store"
)

// BackfillOperatedByAgentsRequest is the JSON body for
// POST /v1/admin/backfill-operated-by-agents. Drives one run of the
// per-user homepage M2 agent operated_by_id backfill — sub-item #1 of
// ka-0000006817.
type BackfillOperatedByAgentsRequest struct {
	BatchSize       int  `json:"batch_size,omitempty"`
	MaxBatches      int  `json:"max_batches,omitempty"`
	IncludeSentinel bool `json:"include_sentinel,omitempty"`
	DryRun          bool `json:"dry_run,omitempty"`
}

// BackfillOperatedByAgentsResponse mirrors
// store.BackfillOperatedByAgentsStats over the wire.
type BackfillOperatedByAgentsResponse struct {
	DryRun         bool             `json:"dry_run"`
	Processed      int64            `json:"processed"`
	Updated        int64            `json:"updated"`
	Skipped        int64            `json:"skipped"`
	RebindBySignal map[string]int64 `json:"rebind_by_signal,omitempty"`
	Batches        int              `json:"batches"`
	BatchesCapped  bool             `json:"batches_capped,omitempty"`
	LastID         string           `json:"last_id,omitempty"`
	DurationMs     int64            `json:"duration_ms"`
}

// handleBackfillOperatedByAgents handles
// POST /v1/admin/backfill-operated-by-agents. Admin-only.
func (s *BeadsServer) handleBackfillOperatedByAgents(w http.ResponseWriter, r *http.Request) {
	var req BackfillOperatedByAgentsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.BackfillOperatedByAgents(r.Context(), store.BackfillOperatedByAgentsOptions{
		BatchSize:       req.BatchSize,
		MaxBatches:      req.MaxBatches,
		IncludeSentinel: req.IncludeSentinel,
		DryRun:          req.DryRun,
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillOperatedByAgents", err, "failed to backfill operated_by on agent beads")
		return
	}
	writeJSON(w, http.StatusOK, &BackfillOperatedByAgentsResponse{
		DryRun:         stats.DryRun,
		Processed:      stats.Processed,
		Updated:        stats.Updated,
		Skipped:        stats.Skipped,
		RebindBySignal: stats.RebindBySignal,
		Batches:        stats.Batches,
		BatchesCapped:  stats.BatchesCapped,
		LastID:         stats.LastID,
		DurationMs:     stats.DurationMs,
	})
}
