package server

import (
	"net/http"
	"time"

	"gardenio/kbeads/internal/store"
)

// BackfillOwnerRequest is the JSON body for
// POST /v1/admin/backfill-owner. Drives one run of ka-0000006961's
// Matt-default owner rebind.
type BackfillOwnerRequest struct {
	Target          string    `json:"target"`
	Since           time.Time `json:"since"`
	BatchSize       int       `json:"batch_size,omitempty"`
	MaxBatches      int       `json:"max_batches,omitempty"`
	UnownedSentinel string    `json:"unowned_sentinel,omitempty"`
	DryRun          bool      `json:"dry_run,omitempty"`
}

// BackfillOwnerResponse mirrors store.BackfillOwnerStats over the wire.
type BackfillOwnerResponse struct {
	DryRun           bool             `json:"dry_run"`
	Target           string           `json:"target"`
	Since            time.Time        `json:"since,omitempty"`
	BeforeCandidates int64            `json:"before_candidates"`
	AfterCandidates  int64            `json:"after_candidates"`
	CandidateBeads   int64            `json:"candidate_beads"`
	RebindCount      map[string]int64 `json:"rebind_count,omitempty"`
	KeptMattCount    int64            `json:"kept_matt_count"`
	UnresolvedCount  int64            `json:"unresolved_count"`
	Batches          int              `json:"batches"`
	BatchesCapped    bool             `json:"batches_capped,omitempty"`
	LastID           string           `json:"last_id,omitempty"`
	DurationMs       int64            `json:"duration_ms"`
}

// handleBackfillOwner handles POST /v1/admin/backfill-owner. Admin-only.
func (s *BeadsServer) handleBackfillOwner(w http.ResponseWriter, r *http.Request) {
	var req BackfillOwnerRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.BackfillOwner(r.Context(), store.BackfillOwnerOptions{
		Target:          req.Target,
		Since:           req.Since,
		BatchSize:       req.BatchSize,
		MaxBatches:      req.MaxBatches,
		UnownedSentinel: req.UnownedSentinel,
		DryRun:          req.DryRun,
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleBackfillOwner", err, "failed to backfill owner")
		return
	}
	writeJSON(w, http.StatusOK, &BackfillOwnerResponse{
		DryRun:           stats.DryRun,
		Target:           stats.Target,
		Since:            stats.Since,
		BeforeCandidates: stats.BeforeCandidates,
		AfterCandidates:  stats.AfterCandidates,
		CandidateBeads:   stats.CandidateBeads,
		RebindCount:      stats.RebindCount,
		KeptMattCount:    stats.KeptMattCount,
		UnresolvedCount:  stats.UnresolvedCount,
		Batches:          stats.Batches,
		BatchesCapped:    stats.BatchesCapped,
		LastID:           stats.LastID,
		DurationMs:       stats.DurationMs,
	})
}
