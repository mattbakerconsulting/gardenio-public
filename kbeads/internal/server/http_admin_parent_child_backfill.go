package server

import (
	"net/http"
	"time"

	"gardenio/kbeads/internal/store"
)

// BackfillParentChildDepsRequest is the JSON body for
// POST /v1/admin/backfill-parent-child-deps. Drives one run of
// ka-0000006775's parent-child reconstruction over agent-authored beads.
type BackfillParentChildDepsRequest struct {
	// SinceSeconds limits the scan to beads created within the last
	// SinceSeconds seconds. Zero falls back to the store default
	// (6 months ≈ 180 days).
	SinceSeconds  int64 `json:"since_seconds,omitempty"`
	IncludeClosed bool  `json:"include_closed,omitempty"`
	BatchSize     int   `json:"batch_size,omitempty"`
	MaxBatches    int   `json:"max_batches,omitempty"`
	DryRun        bool  `json:"dry_run,omitempty"`
}

// BackfillParentChildDepsResponse mirrors store.BackfillParentChildDepsStats
// over the wire.
type BackfillParentChildDepsResponse struct {
	DryRun               bool             `json:"dry_run"`
	CandidateBeads       int64            `json:"candidate_beads"`
	EdgesAdded           int64            `json:"edges_added"`
	EdgesByAuthor        map[string]int64 `json:"edges_by_author,omitempty"`
	SkippedNoAgent       int64            `json:"skipped_no_agent"`
	SkippedNoTaskID      int64            `json:"skipped_no_task_id"`
	SkippedSelfParent    int64            `json:"skipped_self_parent"`
	SkippedTaskMissing   int64            `json:"skipped_task_missing"`
	SkippedExistingDep   int64            `json:"skipped_existing_dep"`
	SkippedParentMention int64            `json:"skipped_parent_mention"`
	Batches              int              `json:"batches"`
	BatchesCapped        bool             `json:"batches_capped,omitempty"`
	LastID               string           `json:"last_id,omitempty"`
	DurationMs           int64            `json:"duration_ms"`
}

// handleBackfillParentChildDeps handles
// POST /v1/admin/backfill-parent-child-deps. Admin-only.
func (s *BeadsServer) handleBackfillParentChildDeps(w http.ResponseWriter, r *http.Request) {
	var req BackfillParentChildDepsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.BackfillParentChildDeps(r.Context(), store.BackfillParentChildDepsOptions{
		Since:         time.Duration(req.SinceSeconds) * time.Second,
		IncludeClosed: req.IncludeClosed,
		BatchSize:     req.BatchSize,
		MaxBatches:    req.MaxBatches,
		DryRun:        req.DryRun,
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillParentChildDeps", err, "failed to backfill parent-child deps")
		return
	}
	writeJSON(w, http.StatusOK, &BackfillParentChildDepsResponse{
		DryRun:               stats.DryRun,
		CandidateBeads:       stats.CandidateBeads,
		EdgesAdded:           stats.EdgesAdded,
		EdgesByAuthor:        stats.EdgesByAuthor,
		SkippedNoAgent:       stats.SkippedNoAgent,
		SkippedNoTaskID:      stats.SkippedNoTaskID,
		SkippedSelfParent:    stats.SkippedSelfParent,
		SkippedTaskMissing:   stats.SkippedTaskMissing,
		SkippedExistingDep:   stats.SkippedExistingDep,
		SkippedParentMention: stats.SkippedParentMention,
		Batches:              stats.Batches,
		BatchesCapped:        stats.BatchesCapped,
		LastID:               stats.LastID,
		DurationMs:           stats.DurationMs,
	})
}
