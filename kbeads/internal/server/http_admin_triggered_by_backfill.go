package server

import (
	"net/http"
	"time"

	"gardenio/kbeads/internal/store"
)

// BackfillTriggeredByDepsRequest is the JSON body for
// POST /v1/admin/backfill-triggered-by-deps. Drives one run of the
// M4 auto-link backfill scanner from epic ka-0000000933 / task
// ka-0000000004 — fills in triggered-by deps on recently-created beads
// that carry an unambiguous origin signal (slack_thread_bead_id or
// trigger_id) but never got linked at creation time.
type BackfillTriggeredByDepsRequest struct {
	// SinceSeconds limits the scan to beads created within the last
	// SinceSeconds seconds. Zero falls back to the store default
	// (24 hours).
	SinceSeconds int64 `json:"since_seconds,omitempty"`
	BatchSize    int   `json:"batch_size,omitempty"`
	MaxBatches   int   `json:"max_batches,omitempty"`
	DryRun       bool  `json:"dry_run,omitempty"`
}

// BackfillTriggeredByDepsResponse mirrors store.BackfillTriggeredByDepsStats
// over the wire.
type BackfillTriggeredByDepsResponse struct {
	DryRun               bool             `json:"dry_run"`
	CandidateBeads       int64            `json:"candidate_beads"`
	EdgesAdded           int64            `json:"edges_added"`
	EdgesBySignal        map[string]int64 `json:"edges_by_signal,omitempty"`
	SkippedExistingDep   int64            `json:"skipped_existing_dep"`
	SkippedNoSignal      int64            `json:"skipped_no_signal"`
	SkippedAmbiguous     int64            `json:"skipped_ambiguous"`
	SkippedSelfReference int64            `json:"skipped_self_reference"`
	SkippedTargetMissing int64            `json:"skipped_target_missing"`
	Batches              int              `json:"batches"`
	BatchesCapped        bool             `json:"batches_capped,omitempty"`
	LastID               string           `json:"last_id,omitempty"`
	DurationMs           int64            `json:"duration_ms"`
}

// handleBackfillTriggeredByDeps handles
// POST /v1/admin/backfill-triggered-by-deps. Admin-only.
func (s *BeadsServer) handleBackfillTriggeredByDeps(w http.ResponseWriter, r *http.Request) {
	var req BackfillTriggeredByDepsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.BackfillTriggeredByDeps(r.Context(), store.BackfillTriggeredByDepsOptions{
		Since:      time.Duration(req.SinceSeconds) * time.Second,
		BatchSize:  req.BatchSize,
		MaxBatches: req.MaxBatches,
		DryRun:     req.DryRun,
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillTriggeredByDeps", err, "failed to backfill triggered-by deps")
		return
	}
	writeJSON(w, http.StatusOK, &BackfillTriggeredByDepsResponse{
		DryRun:               stats.DryRun,
		CandidateBeads:       stats.CandidateBeads,
		EdgesAdded:           stats.EdgesAdded,
		EdgesBySignal:        stats.EdgesBySignal,
		SkippedExistingDep:   stats.SkippedExistingDep,
		SkippedNoSignal:      stats.SkippedNoSignal,
		SkippedAmbiguous:     stats.SkippedAmbiguous,
		SkippedSelfReference: stats.SkippedSelfReference,
		SkippedTargetMissing: stats.SkippedTargetMissing,
		Batches:              stats.Batches,
		BatchesCapped:        stats.BatchesCapped,
		LastID:               stats.LastID,
		DurationMs:           stats.DurationMs,
	})
}
