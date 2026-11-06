package server

import (
	"net/http"

	"gardenio/kbeads/internal/store"
)

// BackfillOperatedByLegacyRequest is the JSON body for
// POST /v1/admin/backfill-operated-by-legacy. Drives one run of
// ka-0000001880's legacy-operated-by cleanup.
type BackfillOperatedByLegacyRequest struct {
	Target        string   `json:"target"`
	Statuses      []string `json:"statuses,omitempty"`
	BatchSize     int      `json:"batch_size,omitempty"`
	MaxIterations int      `json:"max_iterations,omitempty"`
	DryRun        bool     `json:"dry_run,omitempty"`
}

// BackfillOperatedByLegacyResponse mirrors store.BackfillOperatedByLegacyStats
// over the wire.
type BackfillOperatedByLegacyResponse struct {
	DryRun               bool             `json:"dry_run"`
	Target               string           `json:"target"`
	TargetOperatorID     int64            `json:"target_operator_id"`
	BeforeOpenCandidates int64            `json:"before_open_candidates"`
	AfterOpenCandidates  int64            `json:"after_open_candidates"`
	CategoryA            int64            `json:"category_a"`
	CategoryB            int64            `json:"category_b"`
	CategoryC            int64            `json:"category_c"`
	CategoryD            int64            `json:"category_d"`
	RebindCount          map[string]int64 `json:"rebind_count,omitempty"`
	Iterations           int              `json:"iterations"`
	IterationsCapped     bool             `json:"iterations_capped,omitempty"`
	DurationMs           int64            `json:"duration_ms"`
}

// handleBackfillOperatedByLegacy handles
// POST /v1/admin/backfill-operated-by-legacy. Admin-only.
func (s *BeadsServer) handleBackfillOperatedByLegacy(w http.ResponseWriter, r *http.Request) {
	var req BackfillOperatedByLegacyRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.BackfillOperatedByLegacy(r.Context(), store.BackfillOperatedByLegacyOptions{
		Target:        req.Target,
		Statuses:      req.Statuses,
		BatchSize:     req.BatchSize,
		MaxIterations: req.MaxIterations,
		DryRun:        req.DryRun,
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillOperatedByLegacy", err, "failed to backfill operated-by-legacy")
		return
	}
	writeJSON(w, http.StatusOK, &BackfillOperatedByLegacyResponse{
		DryRun:               stats.DryRun,
		Target:               stats.Target,
		TargetOperatorID:     stats.TargetOperatorID,
		BeforeOpenCandidates: stats.BeforeOpenCandidates,
		AfterOpenCandidates:  stats.AfterOpenCandidates,
		CategoryA:            stats.CategoryA,
		CategoryB:            stats.CategoryB,
		CategoryC:            stats.CategoryC,
		CategoryD:            stats.CategoryD,
		RebindCount:          stats.RebindCount,
		Iterations:           stats.Iterations,
		IterationsCapped:     stats.IterationsCapped,
		DurationMs:           stats.DurationMs,
	})
}
