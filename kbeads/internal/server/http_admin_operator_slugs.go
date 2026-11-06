package server

import (
	"net/http"

	"gardenio/kbeads/internal/store"
)

// BackfillOperatorSlugsRequest is the JSON body for
// POST /v1/admin/backfill-operator-slugs. Drives one run of ka-0000001149's
// op-<id> → readable slug backfill.
type BackfillOperatorSlugsRequest struct {
	BatchSize int  `json:"batch_size,omitempty"`
	DryRun    bool `json:"dry_run,omitempty"`
}

// BackfillOperatorSlugsResponse mirrors store.BackfillOperatorSlugsStats over
// the wire.
type BackfillOperatorSlugsResponse struct {
	DryRun           bool  `json:"dry_run"`
	BeforeOpaque     int64 `json:"before_opaque"`
	AfterOpaque      int64 `json:"after_opaque"`
	Renamed          int64 `json:"renamed"`
	SkippedCollision int64 `json:"skipped_collision"`
	Machine          int64 `json:"machine"`
	DurationMs       int64 `json:"duration_ms"`
}

// handleBackfillOperatorSlugs handles
// POST /v1/admin/backfill-operator-slugs. Admin-only.
func (s *BeadsServer) handleBackfillOperatorSlugs(w http.ResponseWriter, r *http.Request) {
	var req BackfillOperatorSlugsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.BackfillOperatorSlugs(r.Context(), store.BackfillOperatorSlugsOptions{
		BatchSize: req.BatchSize,
		DryRun:    req.DryRun,
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillOperatorSlugs", err, "failed to backfill operator slugs")
		return
	}
	writeJSON(w, http.StatusOK, &BackfillOperatorSlugsResponse{
		DryRun:           stats.DryRun,
		BeforeOpaque:     stats.BeforeOpaque,
		AfterOpaque:      stats.AfterOpaque,
		Renamed:          stats.Renamed,
		SkippedCollision: stats.SkippedCollision,
		Machine:          stats.Machine,
		DurationMs:       stats.DurationMs,
	})
}
