package server

import (
	"net/http"

	"gardenio/kbeads/internal/store"
)

// CloseTriggerAuditBeadsRequest is the JSON body for
// POST /v1/admin/close-trigger-audit-beads. Drives one run of
// ka-0000006341's legacy trigger-audit close sweep.
type CloseTriggerAuditBeadsRequest struct {
	BatchSize  int  `json:"batch_size,omitempty"`
	MaxBatches int  `json:"max_batches,omitempty"`
	DryRun     bool `json:"dry_run,omitempty"`
}

// CloseTriggerAuditBeadsResponse mirrors store.CloseTriggerAuditBeadsStats
// over the wire.
type CloseTriggerAuditBeadsResponse struct {
	DryRun         bool   `json:"dry_run"`
	CandidateBeads int64  `json:"candidate_beads"`
	BeadsClosed    int64  `json:"beads_closed"`
	Batches        int    `json:"batches"`
	BatchesCapped  bool   `json:"batches_capped,omitempty"`
	LastID         string `json:"last_id,omitempty"`
	DurationMs     int64  `json:"duration_ms"`
}

// handleCloseTriggerAuditBeads handles
// POST /v1/admin/close-trigger-audit-beads. Admin-only.
func (s *BeadsServer) handleCloseTriggerAuditBeads(w http.ResponseWriter, r *http.Request) {
	var req CloseTriggerAuditBeadsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	stats, err := s.store.CloseTriggerAuditBeads(r.Context(), store.CloseTriggerAuditBeadsOptions{
		BatchSize:  req.BatchSize,
		MaxBatches: req.MaxBatches,
		DryRun:     req.DryRun,
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleCloseTriggerAuditBeads", err, "failed to close trigger-audit beads")
		return
	}
	writeJSON(w, http.StatusOK, &CloseTriggerAuditBeadsResponse{
		DryRun:         stats.DryRun,
		CandidateBeads: stats.CandidateBeads,
		BeadsClosed:    stats.BeadsClosed,
		Batches:        stats.Batches,
		BatchesCapped:  stats.BatchesCapped,
		LastID:         stats.LastID,
		DurationMs:     stats.DurationMs,
	})
}
