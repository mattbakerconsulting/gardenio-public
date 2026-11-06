package server

import (
	"net/http"

	"gardenio/kbeads/internal/store"
)

// BackfillEmptyOwnerRequest is the JSON body for
// POST /v1/admin/backfill-empty-owner. Drives one run of the three-phase
// empty-owner backfill (ka-0000007029).
type BackfillEmptyOwnerRequest struct {
	BatchSize                int      `json:"batch_size,omitempty"`
	MaxBatches               int      `json:"max_batches,omitempty"`
	UnownedSentinel          string   `json:"unowned_sentinel,omitempty"`
	LegacyOperatedBySentinel string   `json:"legacy_operated_by_sentinel,omitempty"`
	Phase2Exclusions         []string `json:"phase2_exclusions,omitempty"`
	DryRun                   bool     `json:"dry_run,omitempty"`
}

// BackfillEmptyOwnerResponse mirrors store.BackfillEmptyOwnerStats over the wire.
type BackfillEmptyOwnerResponse struct {
	DryRun        bool  `json:"dry_run"`
	BeforeEmpty   int64 `json:"before_empty"`
	AfterEmpty    int64 `json:"after_empty"`
	Phase1Filled  int64 `json:"phase1_filled"`
	Phase2Filled  int64 `json:"phase2_filled"`
	Phase3Filled  int64 `json:"phase3_filled"`
	Phase1Batches int   `json:"phase1_batches"`
	Phase2Batches int   `json:"phase2_batches"`
	Phase3Batches int   `json:"phase3_batches"`
	BatchesCapped bool  `json:"batches_capped,omitempty"`
	DurationMs    int64 `json:"duration_ms"`
}

// handleBackfillEmptyOwner handles POST /v1/admin/backfill-empty-owner. Admin-only.
func (s *BeadsServer) handleBackfillEmptyOwner(w http.ResponseWriter, r *http.Request) {
	var req BackfillEmptyOwnerRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// A non-dry-run writes one mutation_history row per affected bead, and
	// migration 026 added a NOT-NULL CHECK on
	// mutation_history.operated_by_id — so every write must carry a resolved
	// operator FK. Resolve it fail-closed from the auth identity (never from
	// the request body): an unattributable caller gets a 400 rather than a
	// 500 on the first phase-1 batch (ka-0000000343). A dry-run writes
	// nothing, so it skips this.
	var operatedByID int64
	if !req.DryRun {
		_, id, refusalSource, ok := s.resolveAuditOperatorRequired(r.Context())
		if !ok {
			writeOperatorRefusal(w, refusalSource)
			return
		}
		operatedByID = id
	}
	stats, err := s.store.BackfillEmptyOwner(r.Context(), store.BackfillEmptyOwnerOptions{
		BatchSize:                req.BatchSize,
		MaxBatches:               req.MaxBatches,
		UnownedSentinel:          req.UnownedSentinel,
		LegacyOperatedBySentinel: req.LegacyOperatedBySentinel,
		Phase2Exclusions:         req.Phase2Exclusions,
		OperatedByID:             operatedByID,
		DryRun:                   req.DryRun,
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillEmptyOwner", err, "failed to backfill empty owner")
		return
	}
	writeJSON(w, http.StatusOK, &BackfillEmptyOwnerResponse{
		DryRun:        stats.DryRun,
		BeforeEmpty:   stats.BeforeEmpty,
		AfterEmpty:    stats.AfterEmpty,
		Phase1Filled:  stats.Phase1Filled,
		Phase2Filled:  stats.Phase2Filled,
		Phase3Filled:  stats.Phase3Filled,
		Phase1Batches: stats.Phase1Batches,
		Phase2Batches: stats.Phase2Batches,
		Phase3Batches: stats.Phase3Batches,
		BatchesCapped: stats.BatchesCapped,
		DurationMs:    stats.DurationMs,
	})
}
